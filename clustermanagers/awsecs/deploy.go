package awsecs

import (
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/cloudwatchlogs"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/ecs"
	"github.com/aws/aws-sdk-go/service/iam"
	"github.com/golang/glog"
	logging "github.com/op/go-logging"
	"github.com/spf13/viper"

	"github.com/hyperpilotio/deployer/apis"
	"github.com/hyperpilotio/deployer/clusters"
	hpaws "github.com/hyperpilotio/deployer/clusters/aws"
	"github.com/hyperpilotio/deployer/common"
	"github.com/hyperpilotio/deployer/job"
	"github.com/hyperpilotio/go-utils/log"
)

// NewDeployer return the EC2 of Deployer
func NewDeployer(
	config *viper.Viper,
	cluster clusters.Cluster,
	deployment *apis.Deployment) (*ECSDeployer, error) {
	log, err := log.NewLogger(config.GetString("filesPath"), deployment.Name)
	if err != nil {
		return nil, errors.New("Error creating deployment logger: " + err.Error())
	}

	deployer := &ECSDeployer{
		Deployment:    deployment,
		Config:        config,
		AWSCluster:    cluster.(*hpaws.AWSCluster),
		DeploymentLog: log,
	}

	return deployer, nil
}

// ReloadClusterState reload EC2 cluster state
func (ecsDeployer *ECSDeployer) ReloadClusterState(storeInfo interface{}) error {
	awsCluster := ecsDeployer.AWSCluster

	sess, sessionErr := hpaws.CreateSession(awsCluster.AWSProfile, awsCluster.Region)
	if sessionErr != nil {
		return fmt.Errorf("Unable to create session: %s", sessionErr.Error())
	}

	ec2Svc := ec2.New(sess)
	ecsSvc := ecs.New(sess)

	deploymentName := awsCluster.Name
	listInstancesInput := &ecs.ListContainerInstancesInput{
		Cluster: aws.String(deploymentName),
	}

	listContainerInstancesOutput, err := ecsSvc.ListContainerInstances(listInstancesInput)
	if err != nil {
		return fmt.Errorf("Unable to list container instances: %s", err.Error())
	}

	ecsDescribeInstancesInput := &ecs.DescribeContainerInstancesInput{
		Cluster:            aws.String(deploymentName),
		ContainerInstances: listContainerInstancesOutput.ContainerInstanceArns,
	}

	ecsDescribeInstancesOutput, err := ecsSvc.DescribeContainerInstances(ecsDescribeInstancesInput)
	if err != nil {
		return fmt.Errorf("Unable to describe container instances: %s", err.Error())
	}

	var instanceIds []*string
	for _, containerInstance := range ecsDescribeInstancesOutput.ContainerInstances {
		instanceIds = append(instanceIds, containerInstance.Ec2InstanceId)
	}
	awsCluster.InstanceIds = instanceIds

	if err := checkVPC(ec2Svc, awsCluster); err != nil {
		return fmt.Errorf("Unable to find VPC: %s", err.Error())
	}

	return nil
}

func (ecsDeployer *ECSDeployer) GetCluster() clusters.Cluster {
	return ecsDeployer.AWSCluster
}

func (ecsDeployer *ECSDeployer) GetLog() *log.FileLog {
	return ecsDeployer.DeploymentLog
}

func (ecsDeployer *ECSDeployer) GetScheduler() *job.Scheduler {
	return ecsDeployer.Scheduler
}

func (ecsDeployer *ECSDeployer) SetScheduler(sheduler *job.Scheduler) {
	ecsDeployer.Scheduler = sheduler
}

func (ecsDeployer *ECSDeployer) GetKubeConfigPath() (string, error) {
	return "", errors.New("Unsupported kubernetes")
}

// CreateDeployment start a deployment
func (ecsDeployer *ECSDeployer) CreateDeployment(uploadedFiles map[string]string) (interface{}, error) {
	awsCluster := ecsDeployer.AWSCluster
	awsProfile := awsCluster.AWSProfile
	deployment := ecsDeployer.Deployment
	log := ecsDeployer.DeploymentLog.Logger

	sess, sessionErr := hpaws.CreateSession(awsProfile, awsCluster.Region)
	if sessionErr != nil {
		return nil, errors.New("Unable to create session: " + sessionErr.Error())
	}

	ecsSvc := ecs.New(sess)
	ec2Svc := ec2.New(sess)
	iamSvc := iam.New(sess)

	log.Infof("Creating AWS Log Group")
	if err := setupAWSLogsGroup(sess, deployment); err != nil {
		ecsDeployer.DeleteDeployment()
		return nil, errors.New("Unable to setup AWS Log Group for container: " + err.Error())
	}

	log.Infof("Setting up ECS cluster")
	if err := setupECS(ecsSvc, awsCluster, deployment); err != nil {
		ecsDeployer.DeleteDeployment()
		return nil, errors.New("Unable to setup ECS: " + err.Error())
	}

	if err := ecsDeployer.SetupEC2Infra("ec2-user", uploadedFiles, ec2Svc, iamSvc, ecsAmis); err != nil {
		ecsDeployer.DeleteDeployment()
		return nil, errors.New("Unable to setup EC2: " + err.Error())
	}

	log.Infof("Waiting for ECS cluster to be ready")
	if err := waitUntilECSClusterReady(ecsSvc, awsCluster, deployment, log); err != nil {
		ecsDeployer.DeleteDeployment()
		return nil, errors.New("Unable to wait until ECS cluster ready: " + err.Error())
	}

	log.Infof("Add attribute on ECS instances")
	if err := setupInstanceAttribute(ecsSvc, awsCluster, deployment); err != nil {
		ecsDeployer.DeleteDeployment()
		return nil, errors.New("Unable to setup instance attribute: " + err.Error())
	}

	log.Infof("Launching ECS services")
	if err := createServices(ecsSvc, awsCluster, deployment, log); err != nil {
		ecsDeployer.DeleteDeployment()
		return nil, errors.New("Unable to launch ECS tasks: " + err.Error())
	}

	return nil, nil
}

func (ecsDeployer *ECSDeployer) UpdateDeployment(updateDeployment *apis.Deployment) error {
	// TODO Implement EC2 UpdateDeployment
	return errors.New("Unimplemented")
}

func (ecsDeployer *ECSDeployer) DeployExtensions(
	extensions *apis.Deployment,
	newDeployment *apis.Deployment) error {
	return errors.New("Unimplemented")
}

// DeleteDeployment clean up the cluster from AWS ECS.
func (ecsDeployer *ECSDeployer) DeleteDeployment() error {
	awsCluster := ecsDeployer.AWSCluster
	deployment := ecsDeployer.Deployment
	awsProfile := awsCluster.AWSProfile
	log := ecsDeployer.DeploymentLog.Logger

	sess, sessionErr := hpaws.CreateSession(awsProfile, deployment.Region)
	if sessionErr != nil {
		log.Errorf("Unable to create session: %s" + sessionErr.Error())
		return sessionErr
	}

	ec2Svc := ec2.New(sess)
	ecsSvc := ecs.New(sess)

	log.Infof("Checking VPC for deletion")
	if err := checkVPC(ec2Svc, awsCluster); err != nil {
		log.Errorf("Unable to find VPC: %s", err.Error())
		return err
	}

	if ecsDeployer.Deployment.ECSDeployment != nil {
		// Stop all running tasks
		log.Infof("Stopping all ECS services")
		if err := stopECSServices(ecsSvc, awsCluster, deployment, log); err != nil {
			log.Errorf("Unable to stop ECS services: ", err.Error())
			return err
		}

		// delete all the task definitions
		log.Infof("Deleting task definitions")
		if err := deleteTaskDefinitions(ecsSvc, awsCluster, deployment, log); err != nil {
			log.Errorf("Unable to delete task definitions: %s", err.Error())
			return err
		}
	}

	// Terminate EC2 instance
	log.Infof("Deleting EC2 instances")
	if err := deleteEC2(ec2Svc, awsCluster); err != nil {
		log.Errorf("Unable to delete task definitions: %s", err.Error())
		return err
	}

	// NOTE if we create autoscaling, delete it. Wait until the deletes all the instance.
	// Delete the launch configuration

	// delete IAM role
	iamSvc := iam.New(sess)

	log.Infof("Deleting IAM role")
	if err := deleteIAM(iamSvc, awsCluster, log); err != nil {
		log.Errorf("Unable to delete IAM: %s", err.Error())
		return err
	}

	// delete key pair
	log.Infof("Deleting key pair")
	if err := DeleteKeyPair(ec2Svc, awsCluster); err != nil {
		log.Errorf("Unable to delete key pair: %s", err.Error())
		return err
	}

	// delete security group
	log.Infof("Deleting security group")
	if err := deleteSecurityGroup(ec2Svc, awsCluster, log); err != nil {
		log.Errorf("Unable to delete security group: %s", err.Error())
		return err
	}

	// delete internet gateway.
	log.Infof("Deleting internet gateway")
	if err := deleteInternetGateway(ec2Svc, awsCluster, log); err != nil {
		log.Errorf("Unable to delete internet gateway: %s", err.Error())
		return err
	}

	// delete subnet.
	log.Infof("Deleting subnet")
	if err := deleteSubnet(ec2Svc, awsCluster, log); err != nil {
		log.Errorf("Unable to delete subnet: %s", err.Error())
		return err
	}

	// Delete VPC
	log.Infof("Deleting VPC")
	if err := deleteVPC(ec2Svc, awsCluster); err != nil {
		log.Errorf("Unable to delete VPC: %s", err)
		return err
	}

	if ecsDeployer.Deployment.ECSDeployment != nil {
		// Delete ecs cluster
		log.Infof("Deleting ECS cluster")
		if err := deleteCluster(ecsSvc, awsCluster); err != nil {
			log.Errorf("Unable to delete ECS cluster: %s", err)
			return err
		}
	}

	return nil
}

func createTags(ec2Svc *ec2.EC2, resources []*string, tags []*ec2.Tag) error {
	tagParams := &ec2.CreateTagsInput{
		Resources: resources,
		Tags:      tags,
	}

	if _, err := ec2Svc.CreateTags(tagParams); err != nil {
		return err
	}

	return nil
}

func createTag(ec2Svc *ec2.EC2, resources []*string, key string, value string) error {
	tags := []*ec2.Tag{
		&ec2.Tag{
			Key:   &key,
			Value: &value,
		},
	}
	return createTags(ec2Svc, resources, tags)
}

func setupECS(ecsSvc *ecs.ECS, awsCluster *hpaws.AWSCluster, deployment *apis.Deployment) error {
	// FIXME check if the cluster exists or not
	clusterParams := &ecs.CreateClusterInput{
		ClusterName: aws.String(awsCluster.Name),
	}

	if _, err := ecsSvc.CreateCluster(clusterParams); err != nil {
		return errors.New("Unable to create cluster: " + err.Error())
	}

	for _, taskDefinition := range deployment.ECSDeployment.TaskDefinitions {
		if _, err := ecsSvc.RegisterTaskDefinition(&taskDefinition); err != nil {
			return errors.New("Unable to register task definition: " + err.Error())
		}
	}

	return nil
}

func setupNetwork(ec2Svc *ec2.EC2, awsCluster *hpaws.AWSCluster, deployment *apis.Deployment, log *logging.Logger) error {
	log.Infof("Creating VPC")
	createVpcInput := &ec2.CreateVpcInput{
		CidrBlock: aws.String("172.31.0.0/28"),
	}

	if createVpcResponse, err := ec2Svc.CreateVpc(createVpcInput); err != nil {
		return errors.New("Unable to create VPC: " + err.Error())
	} else {
		awsCluster.VpcId = *createVpcResponse.Vpc.VpcId
	}

	vpcId := awsCluster.VpcId
	vpcAttributeParams := &ec2.ModifyVpcAttributeInput{
		VpcId: aws.String(vpcId),
		EnableDnsHostnames: &ec2.AttributeBooleanValue{
			Value: aws.Bool(true),
		},
	}

	if _, err := ec2Svc.ModifyVpcAttribute(vpcAttributeParams); err != nil {
		return errors.New("Unable to enable DNS Support with VPC attribute: " + err.Error())
	}

	vpcAttributeParams = &ec2.ModifyVpcAttributeInput{
		VpcId: aws.String(vpcId),
		EnableDnsHostnames: &ec2.AttributeBooleanValue{
			Value: aws.Bool(true),
		},
	}

	if _, err := ec2Svc.ModifyVpcAttribute(vpcAttributeParams); err != nil {
		return errors.New("Unable to enable DNS Hostname with VPC attribute: " + err.Error())
	}

	log.Infof("Tagging VPC with deployment name")
	if err := createTag(ec2Svc, []*string{&vpcId}, "Name", awsCluster.VPCName()); err != nil {
		return errors.New("Unable to tag VPC: " + err.Error())
	}

	createSubnetInput := &ec2.CreateSubnetInput{
		VpcId:     aws.String(awsCluster.VpcId),
		CidrBlock: aws.String("172.31.0.0/28"),
	}

	// NOTE Unhandled case:
	// 1. if subnet created successfuly but aws sdk connection broke or request reached timeout
	if subnetResponse, err := ec2Svc.CreateSubnet(createSubnetInput); err != nil {
		return errors.New("Unable to create subnet: " + err.Error())
	} else {
		awsCluster.SubnetId = *subnetResponse.Subnet.SubnetId
	}

	describeSubnetsInput := &ec2.DescribeSubnetsInput{
		SubnetIds: []*string{aws.String(awsCluster.SubnetId)},
	}

	if err := ec2Svc.WaitUntilSubnetAvailable(describeSubnetsInput); err != nil {
		return errors.New("Unable to wait until subnet available: " + err.Error())
	}

	if err := createTag(ec2Svc, []*string{&awsCluster.SubnetId}, "Name", awsCluster.SubnetName()); err != nil {
		return errors.New("Unable to tag subnet: " + err.Error())
	}

	if gatewayResponse, err := ec2Svc.CreateInternetGateway(&ec2.CreateInternetGatewayInput{}); err != nil {
		return errors.New("Unable to create internet gateway: " + err.Error())
	} else {
		awsCluster.InternetGatewayId = *gatewayResponse.InternetGateway.InternetGatewayId
	}

	// We have to sleep as sometimes the internet gateway won't be available yet. And sadly aws-sdk-go has no function to wait for it.
	time.Sleep(time.Second * 5)

	if err := createTag(ec2Svc, []*string{&awsCluster.InternetGatewayId}, "Name", awsCluster.Name); err != nil {
		return errors.New("Unable to tag internet gateway: " + err.Error())
	}

	attachInternetInput := &ec2.AttachInternetGatewayInput{
		InternetGatewayId: aws.String(awsCluster.InternetGatewayId),
		VpcId:             aws.String(awsCluster.VpcId),
	}

	if _, err := ec2Svc.AttachInternetGateway(attachInternetInput); err != nil {
		return errors.New("Unable to attach internet gateway: " + err.Error())
	}

	var routeTableId = ""
	if describeRoutesOutput, err := ec2Svc.DescribeRouteTables(&ec2.DescribeRouteTablesInput{}); err != nil {
		return errors.New("Unable to describe route tables: " + err.Error())
	} else {
		for _, routeTable := range describeRoutesOutput.RouteTables {
			if *routeTable.VpcId == awsCluster.VpcId {
				routeTableId = *routeTable.RouteTableId
				break
			}
		}
	}

	if routeTableId == "" {
		return errors.New("Unable to find route table associated with vpc")
	}

	createRouteInput := &ec2.CreateRouteInput{
		RouteTableId:         aws.String(routeTableId),
		DestinationCidrBlock: aws.String("0.0.0.0/0"),
		GatewayId:            aws.String(awsCluster.InternetGatewayId),
	}

	if createRouteOutput, err := ec2Svc.CreateRoute(createRouteInput); err != nil {
		return errors.New("Unable to create route: " + err.Error())
	} else if !*createRouteOutput.Return {
		return errors.New("Unable to create route")
	}

	securityGroupParams := &ec2.CreateSecurityGroupInput{
		Description: aws.String(awsCluster.Name),
		GroupName:   aws.String(awsCluster.Name),
		VpcId:       aws.String(awsCluster.VpcId),
	}
	log.Infof("Creating security group")
	securityGroupResp, err := ec2Svc.CreateSecurityGroup(securityGroupParams)
	if err != nil {
		return errors.New("Unable to create security group: " + err.Error())
	}

	awsCluster.SecurityGroupId = *securityGroupResp.GroupId

	ports := make(map[int]string)
	for _, port := range deployment.AllowedPorts {
		ports[port] = "tcp"
	}
	// Open http and https by default
	ports[22] = "tcp"
	ports[80] = "tcp"

	// Also ports needed by Weave
	ports[6783] = "tcp,udp"
	ports[6784] = "udp"

	log.Infof("Allowing ingress input")
	for port, protocols := range ports {
		for _, protocol := range strings.Split(protocols, ",") {
			securityGroupIngressParams := &ec2.AuthorizeSecurityGroupIngressInput{
				CidrIp:     aws.String("0.0.0.0/0"),
				FromPort:   aws.Int64(int64(port)),
				ToPort:     aws.Int64(int64(port)),
				GroupId:    securityGroupResp.GroupId,
				IpProtocol: aws.String(protocol),
			}

			_, err = ec2Svc.AuthorizeSecurityGroupIngress(securityGroupIngressParams)
			if err != nil {
				return errors.New("Unable to authorize security group ingress: " + err.Error())
			}
		}
	}

	return nil
}

func uploadFiles(user string, ec2Svc *ec2.EC2, awsCluster *hpaws.AWSCluster, deployment *apis.Deployment, uploadedFiles map[string]string) error {
	if len(deployment.Files) == 0 {
		return nil
	}

	clientConfig, err := awsCluster.SshConfig(user)
	if err != nil {
		return errors.New("Unable to create ssh config: " + err.Error())
	}

	for _, nodeInfo := range awsCluster.NodeInfos {
		address := nodeInfo.PublicDnsName + ":22"
		scpClient := common.NewSshClient(address, clientConfig, "")
		for _, deployFile := range deployment.Files {
			// TODO: Bulk upload all files, where ssh client needs to support multiple files transfer
			// in the same connection
			location, ok := uploadedFiles[deployment.UserId+"_"+deployFile.FileId]
			if !ok {
				return errors.New("Unable to find uploaded file " + deployFile.FileId)
			}

			if err := scpClient.CopyLocalFileToRemote(location, deployFile.Path); err != nil {
				errorMsg := fmt.Sprintf("Unable to upload file %s to server %s: %s",
					deployFile.FileId, address, err.Error())
				return errors.New(errorMsg)
			}
		}
	}

	return nil
}

func setupEC2(ec2Svc *ec2.EC2, awsCluster *hpaws.AWSCluster, deployment *apis.Deployment, log *logging.Logger, images map[string]string) error {
	if keyOutput, err := hpaws.CreateKeypair(ec2Svc, awsCluster.KeyName()); err != nil {
		return err
	} else {
		awsCluster.KeyPair = keyOutput
	}

	userData := base64.StdEncoding.EncodeToString([]byte(
		fmt.Sprintf(`#!/bin/bash
echo ECS_CLUSTER=%s >> /etc/ecs/ecs.config
echo manual > /etc/weave/scope.override
weave launch`, awsCluster.Name)))

	associatePublic := true
	for _, node := range deployment.ClusterDefinition.Nodes {
		runResult, runErr := ec2Svc.RunInstances(&ec2.RunInstancesInput{
			KeyName: aws.String(*awsCluster.KeyPair.KeyName),
			ImageId: aws.String(images[awsCluster.Region]),
			NetworkInterfaces: []*ec2.InstanceNetworkInterfaceSpecification{
				&ec2.InstanceNetworkInterfaceSpecification{
					AssociatePublicIpAddress: &associatePublic,
					DeleteOnTermination:      &associatePublic,
					DeviceIndex:              aws.Int64(0),
					Groups:                   []*string{&awsCluster.SecurityGroupId},
					SubnetId:                 aws.String(awsCluster.SubnetId),
				},
			},
			InstanceType: aws.String(node.InstanceType),
			IamInstanceProfile: &ec2.IamInstanceProfileSpecification{
				Name: aws.String(awsCluster.Name),
			},
			MinCount: aws.Int64(1),
			MaxCount: aws.Int64(1),
			UserData: aws.String(userData),
		})

		if runErr != nil {
			return errors.New("Unable to run ec2 instance '" + strconv.Itoa(node.Id) + "': " + runErr.Error())
		}

		awsCluster.NodeInfos[node.Id] = &hpaws.NodeInfo{
			Instance: runResult.Instances[0],
		}
		awsCluster.InstanceIds = append(awsCluster.InstanceIds, runResult.Instances[0].InstanceId)
	}

	tags := []*ec2.Tag{
		{
			Key:   aws.String("Deployment"),
			Value: aws.String(awsCluster.Name),
		},
		{
			Key:   aws.String("weave:peerGroupName"),
			Value: aws.String(awsCluster.Name),
		},
	}

	nodeCount := len(awsCluster.InstanceIds)
	log.Infof("Waitng for %d EC2 instances to exist", nodeCount)

	describeInstancesInput := &ec2.DescribeInstancesInput{
		InstanceIds: awsCluster.InstanceIds,
	}

	if err := ec2Svc.WaitUntilInstanceExists(describeInstancesInput); err != nil {
		return errors.New("Unable to wait for ec2 instances to exist: " + err.Error())
	}

	// We are trying to tag before it's running as weave requires the tag to function,
	// so the earlier we tag the better chance we have to see the cluster ready in ECS
	if err := createTags(ec2Svc, awsCluster.InstanceIds, tags); err != nil {
		return errors.New("Unable to create tags for instances: " + err.Error())
	}

	describeInstanceStatusInput := &ec2.DescribeInstanceStatusInput{
		InstanceIds: awsCluster.InstanceIds,
	}

	log.Infof("Waitng for %d EC2 instances to be status ok", nodeCount)
	if err := ec2Svc.WaitUntilInstanceStatusOk(describeInstanceStatusInput); err != nil {
		return errors.New("Unable to wait for ec2 instances be status ok: " + err.Error())
	}

	return nil
}

func setupIAM(iamSvc *iam.IAM, awsCluster *hpaws.AWSCluster, deployment *apis.Deployment, log *logging.Logger) error {
	roleParams := &iam.CreateRoleInput{
		AssumeRolePolicyDocument: aws.String(trustDocument),
		RoleName:                 aws.String(awsCluster.RoleName()),
	}
	// create IAM role
	if _, err := iamSvc.CreateRole(roleParams); err != nil {
		return errors.New("Unable to create IAM role: " + err.Error())
	}

	var policyDocument *string
	if deployment.IamRole.PolicyDocument != "" {
		policyDocument = aws.String(deployment.IamRole.PolicyDocument)
	} else {
		policyDocument = aws.String(defaultRolePolicy)
	}

	// create role policy
	rolePolicyParams := &iam.PutRolePolicyInput{
		RoleName:       aws.String(awsCluster.RoleName()),
		PolicyName:     aws.String(awsCluster.PolicyName()),
		PolicyDocument: policyDocument,
	}

	if _, err := iamSvc.PutRolePolicy(rolePolicyParams); err != nil {
		log.Errorf("Unable to put role policy: %s", err)
		return err
	}

	iamParams := &iam.CreateInstanceProfileInput{
		InstanceProfileName: aws.String(awsCluster.Name),
	}

	if _, err := iamSvc.CreateInstanceProfile(iamParams); err != nil {
		log.Errorf("Unable to create instance profile: %s", err)
		return err
	}

	getProfileInput := &iam.GetInstanceProfileInput{
		InstanceProfileName: aws.String(awsCluster.Name),
	}

	if err := iamSvc.WaitUntilInstanceProfileExists(getProfileInput); err != nil {
		return errors.New("Unable to wait for instance profile to exist: " + err.Error())
	}

	roleInstanceProfileParams := &iam.AddRoleToInstanceProfileInput{
		InstanceProfileName: aws.String(awsCluster.Name),
		RoleName:            aws.String(awsCluster.RoleName()),
	}

	if _, err := iamSvc.AddRoleToInstanceProfile(roleInstanceProfileParams); err != nil {
		log.Errorf("Unable to add role to instance profile: %s", err)
		return err
	}

	return nil
}

/*
func setupAutoScaling(deployment *apis.Deployment, sess *session.Session, awsCluster *hpaws.AWSCluster) error {
	svc := autoscaling.New(sess)

	userData := base64.StdEncoding.EncodeToString([]byte(
		`#!/bin/bash
echo ECS_CLUSTER=` + deployment.Name + " >> /etc/ecs/ecs.config"))

	params := &autoscaling.CreateLaunchConfigurationInput{
		LaunchConfigurationName:  aws.String(deployment.Name), // Required
		AssociatePublicIpAddress: aws.Bool(true),
		EbsOptimized:             aws.Bool(true),
		IamInstanceProfile:       aws.String(deployment.Name),
		ImageId:                  aws.String(ecsAmis[deployment.Region]),
		InstanceMonitoring: &autoscaling.InstanceMonitoring{
			Enabled: aws.Bool(false),
		},
		InstanceType: aws.String("t2.medium"),
		KeyName:      aws.String(deployment.Name),
		SecurityGroups: []*string{
			&awsCluster.SecurityGroupId,
		},
		UserData: aws.String(userData),
	}
	_, err := svc.CreateLaunchConfiguration(params)

	if err != nil {
		return err
	}

	autoScalingGroupParams := &autoscaling.CreateAutoScalingGroupInput{
		AutoScalingGroupName:    aws.String(deployment.Name),
		MaxSize:                 aws.Int64(deployment.Scale),
		MinSize:                 aws.Int64(deployment.Scale),
		DefaultCooldown:         aws.Int64(1),
		DesiredCapacity:         aws.Int64(deployment.Scale),
		LaunchConfigurationName: aws.String(deployment.Name),
		VPCZoneIdentifier:       aws.String(awsCluster.SubnetId),
	}

	if _, err = svc.CreateAutoScalingGroup(autoScalingGroupParams); err != nil {
		return err
	}
	return nil
}
*/

func errorMessageFromFailures(failures []*ecs.Failure) string {
	var failureMessage = ""
	for _, failure := range failures {
		failureMessage += *failure.Reason + ", "
	}

	return failureMessage
}

// Max compare two inputs and return bigger one
func Max(x, y int) int {
	if x > y {
		return x
	}
	return y
}

func startService(awsCluster *hpaws.AWSCluster, mapping *apis.NodeMapping, ecsSvc *ecs.ECS, log *logging.Logger) error {
	serviceInput := &ecs.CreateServiceInput{
		DesiredCount:   aws.Int64(int64(1)),
		ServiceName:    aws.String(mapping.Service()),
		TaskDefinition: aws.String(mapping.Task),
		Cluster:        aws.String(awsCluster.Name),
		PlacementConstraints: []*ecs.PlacementConstraint{
			{
				Expression: aws.String(fmt.Sprintf("attribute:imageId == %s", mapping.ImageIdAttribute())),
				Type:       aws.String("memberOf"),
			},
		},
	}

	log.Infof("Starting service %v\n", serviceInput)
	if _, err := ecsSvc.CreateService(serviceInput); err != nil {
		return fmt.Errorf("Unable to start service %v\nError: %v\n", mapping.Service(), err)
	}

	return nil
}

func createServices(ecsSvc *ecs.ECS, awsCluster *hpaws.AWSCluster, deployment *apis.Deployment, log *logging.Logger) error {
	for _, mapping := range deployment.NodeMapping {
		startService(awsCluster, &mapping, ecsSvc, log)
	}

	return nil
}

func waitUntilECSClusterReady(ecsSvc *ecs.ECS, awsCluster *hpaws.AWSCluster, deployment *apis.Deployment, log *logging.Logger) error {
	// Wait for ECS instances to be ready
	clusterReady := false
	totalCount := int64(len(deployment.ClusterDefinition.Nodes))
	describeClustersInput := &ecs.DescribeClustersInput{
		Clusters: []*string{&awsCluster.Name},
	}

	for !clusterReady {
		if describeOutput, err := ecsSvc.DescribeClusters(describeClustersInput); err != nil {
			return errors.New("Unable to list ECS clusters: " + err.Error())
		} else if len(describeOutput.Failures) > 0 {
			return errors.New(
				"Unable to list ECS clusters: " + errorMessageFromFailures(describeOutput.Failures))
		} else {
			registeredCount := *describeOutput.Clusters[0].RegisteredContainerInstancesCount

			if registeredCount >= totalCount {
				break
			} else {
				log.Infof("Cluster not ready. registered %d, total %v", registeredCount, totalCount)
				time.Sleep(time.Duration(3) * time.Second)
			}
		}
	}

	return nil
}

func populatePublicDnsNames(ec2Svc *ec2.EC2, awsCluster *hpaws.AWSCluster, log *logging.Logger) error {
	// We need to describe instances again to obtain the PublicDnsAddresses for ssh.
	describeInstanceOutput, err := ec2Svc.DescribeInstances(&ec2.DescribeInstancesInput{
		InstanceIds: awsCluster.InstanceIds,
	})
	if err != nil {
		return errors.New("Unable to describe instances: " + err.Error())
	}

	for _, reservation := range describeInstanceOutput.Reservations {
		for _, instance := range reservation.Instances {
			if instance.PublicDnsName != nil && *instance.PublicDnsName != "" {
				for nodeId, nodeInfo := range awsCluster.NodeInfos {
					if *nodeInfo.Instance.InstanceId == *instance.InstanceId {
						log.Infof("Assigning public dns name %s to node %d",
							*instance.PublicDnsName, nodeId)
						nodeInfo.PublicDnsName = *instance.PublicDnsName
						nodeInfo.PrivateIp = *instance.PrivateIpAddress
					}
				}
			}
		}
	}

	return nil
}

func setupInstanceAttribute(ecsSvc *ecs.ECS, awsCluster *hpaws.AWSCluster, deployment *apis.Deployment) error {
	var containerInstances []*string
	listInstancesInput := &ecs.ListContainerInstancesInput{
		Cluster: aws.String(awsCluster.Name),
	}

	listInstancesOutput, err := ecsSvc.ListContainerInstances(listInstancesInput)

	if err != nil {
		return errors.New("Unable to list container instances: " + err.Error())
	}

	containerInstances = listInstancesOutput.ContainerInstanceArns

	describeInstancesInput := &ecs.DescribeContainerInstancesInput{
		Cluster:            aws.String(awsCluster.Name),
		ContainerInstances: containerInstances,
	}

	describeInstancesOutput, err := ecsSvc.DescribeContainerInstances(describeInstancesInput)

	if err != nil {
		return errors.New("Unable to describe container instances: " + err.Error())
	}

	for _, instance := range describeInstancesOutput.ContainerInstances {
		for _, nodeInfo := range awsCluster.NodeInfos {
			if *instance.Ec2InstanceId == *nodeInfo.Instance.InstanceId {
				nodeInfo.Arn = *instance.ContainerInstanceArn
				break
			}
		}
	}

	for _, mapping := range deployment.NodeMapping {
		nodeInfo, ok := awsCluster.NodeInfos[mapping.Id]
		if !ok {
			return fmt.Errorf("Unable to find Node id %d in instance map", mapping.Id)
		}

		params := &ecs.PutAttributesInput{
			Attributes: []*ecs.Attribute{
				{
					Name:       aws.String("imageId"),
					TargetId:   aws.String(nodeInfo.Arn),
					TargetType: aws.String("container-instance"),
					Value:      aws.String(mapping.ImageIdAttribute()),
				},
			},
			Cluster: aws.String(awsCluster.Name),
		}

		_, err := ecsSvc.PutAttributes(params)

		if err != nil {
			return fmt.Errorf("Unable to put attribute on ECS instance: %v\nMessage:%s\n", params, err.Error())
		}
	}
	return nil
}

func createAWSLogsGroup(groupName string, svc *cloudwatchlogs.CloudWatchLogs) error {
	params := &cloudwatchlogs.CreateLogGroupInput{
		LogGroupName: aws.String(groupName),
	}
	_, err := svc.CreateLogGroup(params)

	if err != nil {
		if strings.Contains(err.Error(), "ResourceAlreadyExistsException") {
			glog.Infof("Skip creating log group %s as it has already been created", groupName)
			return nil
		}

		errMsg := fmt.Sprintf("Unable to create the AWS log group of %s.\nException Message: %s\n", groupName, err.Error())
		glog.Warning(errMsg)
		return errors.New(errMsg)
	}

	return nil
}

func setupAWSLogsGroup(sess *session.Session, deployment *apis.Deployment) error {
	svc := cloudwatchlogs.New(sess)
	for _, task := range deployment.TaskDefinitions {
		for _, container := range task.ContainerDefinitions {
			// NOTE assume the log group does not exist and ignore any error.
			// Error types:
			// * InvalidParameterException
			// A parameter is specified incorrectly.
			// 	* ResourceAlreadyExistsException
			// The specified resource already exists.
			// 	* LimitExceededException
			// You have reached the maximum number of resources that can be created.
			// 	* OperationAbortedException
			// Multiple requests to update the same resource were in conflict.
			// 	* ServiceUnavailableException
			// The service cannot complete the request.
			// https://docs.aws.amazon.com/sdk-for-go/api/service/cloudwatchlogs/#example_CloudWatchLogs_CreateLogGroup
			createAWSLogsGroup(*container.Name, svc)

			// NOTE ensure each container definition containing logConfiguration field.
			container.LogConfiguration = &ecs.LogConfiguration{
				LogDriver: aws.String("awslogs"),
				Options: map[string]*string{
					"awslogs-group":         container.Name,
					"awslogs-region":        aws.String(deployment.Region),
					"awslogs-stream-prefix": aws.String("awslogs"),
				},
			}
		}
	}
	return nil
}

func (ecsDeployer *ECSDeployer) SetupEC2Infra(user string, uploadedFiles map[string]string, ec2Svc *ec2.EC2, iamSvc *iam.IAM, images map[string]string) error {
	awsCluster := ecsDeployer.AWSCluster
	deployment := ecsDeployer.Deployment
	log := ecsDeployer.DeploymentLog.Logger

	log.Infof("Setting up IAM Role")
	if err := setupIAM(iamSvc, awsCluster, deployment, log); err != nil {
		ecsDeployer.DeleteDeployment()
		return errors.New("Unable to setup IAM: " + err.Error())
	}

	log.Infof("Setting up Network")
	if err := setupNetwork(ec2Svc, awsCluster, deployment, log); err != nil {
		ecsDeployer.DeleteDeployment()
		return errors.New("Unable to setup Network: " + err.Error())
	}

	log.Infof("Launching EC2 instances")
	if err := setupEC2(ec2Svc, awsCluster, deployment, log, images); err != nil {
		ecsDeployer.DeleteDeployment()
		return errors.New("Unable to setup EC2: " + err.Error())
	}

	log.Infof("Populating public dns names")
	if err := populatePublicDnsNames(ec2Svc, awsCluster, log); err != nil {
		ecsDeployer.DeleteDeployment()
		return errors.New("Unable to populate public dns names: " + err.Error())
	}

	log.Infof("Uploading files to EC2 Instances")
	if err := uploadFiles(user, ec2Svc, awsCluster, deployment, uploadedFiles); err != nil {
		return errors.New("Unable to upload files to EC2: " + err.Error())
	}

	return nil
}

func isRegionValid(region string, images map[string]string) bool {
	_, ok := images[region]
	return ok
}

func stopECSTasks(svc *ecs.ECS, awsCluster *hpaws.AWSCluster, log *logging.Logger) error {
	errMsg := false
	params := &ecs.ListTasksInput{
		Cluster: aws.String(awsCluster.Name),
	}
	resp, err := svc.ListTasks(params)
	if err != nil {
		log.Errorf("Unable to list tasks: %s", err.Error())
		return err
	}

	for _, task := range resp.TaskArns {
		stopTaskParams := &ecs.StopTaskInput{
			Cluster: aws.String(awsCluster.Name),
			Task:    task,
			Reason:  aws.String("Gracefully stop by hyperpilot/deployer."),
		}

		// NOTE Should we store the output of svc.StopTask ?
		// https://docs.aws.amazon.com/sdk-for-go/api/service/ecs/#StopTaskOutput
		_, err := svc.StopTask(stopTaskParams)
		if err != nil {
			log.Errorf("Unable to stop task (%s) %s\n", *task, err.Error())
			errMsg = true
		}
	}

	if errMsg {
		return errors.New("Unable to clean up all the tasks.")
	}
	return nil
}

func updateECSService(svc *ecs.ECS, nodemapping *apis.NodeMapping, cluster string, count int) error {
	params := &ecs.UpdateServiceInput{
		Service:      aws.String(nodemapping.Service()),
		Cluster:      aws.String(cluster),
		DesiredCount: aws.Int64(int64(count)),
	}

	if _, err := svc.UpdateService(params); err != nil {
		return fmt.Errorf("Unable to update ECS service: %s\n", err.Error())
	}
	return nil
}

func deleteECSService(svc *ecs.ECS, nodemapping *apis.NodeMapping, cluster string) error {
	params := &ecs.DeleteServiceInput{
		Service: aws.String(nodemapping.Service()),
		Cluster: aws.String(cluster),
	}

	if _, err := svc.DeleteService(params); err != nil {
		return fmt.Errorf("Unable to delete ECS service (%s): %s\n", nodemapping.Service(), err.Error())
	}
	return nil
}

func stopECSServices(svc *ecs.ECS, awsCluster *hpaws.AWSCluster, deployment *apis.Deployment, log *logging.Logger) error {
	errMsg := false
	for _, nodemapping := range deployment.NodeMapping {
		if err := updateECSService(svc, &nodemapping, awsCluster.Name, 0); err != nil {
			errMsg = true
			log.Warningf("Unable to update ECS service service %s to 0:\nMessage:%v\n", nodemapping.Service(), err.Error())
		}

		if err := deleteECSService(svc, &nodemapping, awsCluster.Name); err != nil {
			errMsg = true
			log.Warningf("Unable to delete ECS service (%s):\nMessage:%v\n", nodemapping.Service(), err.Error())
		}
	}

	if errMsg {
		return errors.New("Unable to clean up all the services.")
	}
	return nil
}

func deleteTaskDefinitions(ecsSvc *ecs.ECS, awsCluster *hpaws.AWSCluster, deployment *apis.Deployment, log *logging.Logger) error {
	var tasks []*string
	var errBool bool

	for _, taskDefinition := range deployment.TaskDefinitions {
		params := &ecs.DescribeTaskDefinitionInput{
			TaskDefinition: taskDefinition.Family,
		}
		resp, err := ecsSvc.DescribeTaskDefinition(params)
		if err != nil {
			log.Warningf("Unable to describe task (%s) : %s\n", *taskDefinition.Family, err.Error())
			continue
		}

		tasks = append(tasks, aws.String(fmt.Sprintf("%s:%d", *taskDefinition.Family, *resp.TaskDefinition.Revision)))
	}

	for _, task := range tasks {
		if _, err := ecsSvc.DeregisterTaskDefinition(&ecs.DeregisterTaskDefinitionInput{TaskDefinition: task}); err != nil {
			log.Warningf("Unable to de-register task definition: %s\n", err.Error())
			errBool = true
		}
	}

	if errBool {
		return errors.New("Unable to clean up task definitions")
	}

	return nil
}

func deleteIAM(iamSvc *iam.IAM, awsCluster *hpaws.AWSCluster, log *logging.Logger) error {
	// NOTE For now (2016/12/28), we don't know how to handle the error of request timeout from AWS SDK.
	// Ignore all the errors and log them as error messages into log file. deleteIAM has different severity,
	//in contrast to other functions. Since the error of deleteIAM causes next time deployment's failure .

	errBool := false

	// remove role from instance profile
	roleInstanceProfileParam := &iam.RemoveRoleFromInstanceProfileInput{
		InstanceProfileName: aws.String(awsCluster.Name),
		RoleName:            aws.String(awsCluster.RoleName()),
	}
	if _, err := iamSvc.RemoveRoleFromInstanceProfile(roleInstanceProfileParam); err != nil {
		log.Errorf("Unable to delete role %s from instance profile: %s\n",
			awsCluster.RoleName(), err.Error())
		errBool = true
	}

	// delete instance profile
	instanceProfile := &iam.DeleteInstanceProfileInput{
		InstanceProfileName: aws.String(awsCluster.Name),
	}
	if _, err := iamSvc.DeleteInstanceProfile(instanceProfile); err != nil {
		log.Errorf("Unable to delete instance profile: %s\n", err.Error())
		errBool = true
	}

	// delete role policy
	rolePolicyParams := &iam.DeleteRolePolicyInput{
		PolicyName: aws.String(awsCluster.PolicyName()),
		RoleName:   aws.String(awsCluster.RoleName()),
	}
	if _, err := iamSvc.DeleteRolePolicy(rolePolicyParams); err != nil {
		log.Errorf("Unable to delete role policy IAM role: %s\n", err.Error())
		errBool = true
	}

	// delete role
	roleParams := &iam.DeleteRoleInput{
		RoleName: aws.String(awsCluster.RoleName()),
	}
	if _, err := iamSvc.DeleteRole(roleParams); err != nil {
		log.Errorf("Unable to delete IAM role: %s", err.Error())
		errBool = true
	}

	if errBool {
		return errors.New("Unable to clean up AWS IAM")
	}

	return nil
}

func DeleteKeyPair(ec2Svc *ec2.EC2, awsCluster *hpaws.AWSCluster) error {
	params := &ec2.DeleteKeyPairInput{
		KeyName: aws.String(awsCluster.KeyName()),
	}
	if _, err := ec2Svc.DeleteKeyPair(params); err != nil {
		return fmt.Errorf("Unable to delete key pair: %s", err.Error())
	}
	return nil
}

func deleteSecurityGroup(ec2Svc *ec2.EC2, awsCluster *hpaws.AWSCluster, log *logging.Logger) error {
	errBool := false
	describeParams := &ec2.DescribeSecurityGroupsInput{
		Filters: []*ec2.Filter{
			{
				Name: aws.String("group-name"),
				Values: []*string{
					aws.String(awsCluster.Name),
				},
			},
		},
	}

	resp, err := ec2Svc.DescribeSecurityGroups(describeParams)
	if err != nil {
		return fmt.Errorf("Unable to describe tags of security group: %s\n", err.Error())
	}

	for _, group := range resp.SecurityGroups {
		params := &ec2.DeleteSecurityGroupInput{
			GroupId: group.GroupId,
		}
		if _, err := ec2Svc.DeleteSecurityGroup(params); err != nil {
			log.Warningf("Unable to delete security group: %s\n", err.Error())
			errBool = true
		}
	}

	if errBool {
		return errors.New("Unable to delete all the relative security groups")
	}

	return nil
}

func deleteSubnet(ec2Svc *ec2.EC2, awsCluster *hpaws.AWSCluster, log *logging.Logger) error {
	errBool := false
	params := &ec2.DescribeTagsInput{
		Filters: []*ec2.Filter{
			{
				Name: aws.String("resource-type"),
				Values: []*string{
					aws.String("subnet"),
				},
			},
			{
				Name: aws.String("tag:Name"),
				Values: []*string{
					aws.String(awsCluster.SubnetName()),
				},
			},
		},
	}

	resp, err := ec2Svc.DescribeTags(params)

	if err != nil {
		return fmt.Errorf("Unable to describe tags of subnet: %s\n", err.Error())
	}

	for _, subnet := range resp.Tags {
		params := &ec2.DeleteSubnetInput{
			SubnetId: subnet.ResourceId,
		}
		_, err := ec2Svc.DeleteSubnet(params)
		if err != nil {
			log.Warningf("Unable to delete subnet (%s) %s\n", *subnet.ResourceId, err.Error())
			errBool = true
		}
	}

	if errBool {
		return errors.New("Unable to clan up subnet")
	}
	return nil
}

func checkVPC(ec2Svc *ec2.EC2, awsCluster *hpaws.AWSCluster) error {
	if awsCluster.VpcId == "" {
		params := &ec2.DescribeTagsInput{
			Filters: []*ec2.Filter{
				{
					Name: aws.String("resource-type"),
					Values: []*string{
						aws.String("vpc"),
					},
				},
				{
					Name: aws.String("tag:Name"),
					Values: []*string{
						aws.String(awsCluster.VPCName()),
					},
				},
			},
		}
		resp, err := ec2Svc.DescribeTags(params)
		if err != nil {
			return fmt.Errorf("Unable to describe tags for VPC: %s\n", err.Error())
		} else if len(resp.Tags) <= 0 {
			return errors.New("Can not find VPC")
		}
		awsCluster.VpcId = *resp.Tags[0].ResourceId
	}
	return nil
}

func deleteInternetGateway(ec2Svc *ec2.EC2, awsCluster *hpaws.AWSCluster, log *logging.Logger) error {
	params := &ec2.DescribeTagsInput{
		Filters: []*ec2.Filter{
			{
				Name: aws.String("resource-type"),
				Values: []*string{
					aws.String("internet-gateway"),
				},
			},
			{
				Name: aws.String("tag:Name"),
				Values: []*string{
					aws.String(awsCluster.Name),
				},
			},
		},
	}

	resp, err := ec2Svc.DescribeTags(params)
	if err != nil {
		log.Warningf("Unable to describe tags: %s\n", err.Error())
	} else if len(resp.Tags) == 0 {
		log.Warningf("Unable to find the internet gateway (%s)\n", awsCluster.Name)
	} else {
		detachGatewayParams := &ec2.DetachInternetGatewayInput{
			InternetGatewayId: resp.Tags[0].ResourceId,
			VpcId:             aws.String(awsCluster.VpcId),
		}
		if _, err := ec2Svc.DetachInternetGateway(detachGatewayParams); err != nil {
			log.Warningf("Unable to detach InternetGateway: %s\n", err.Error())
		}

		deleteGatewayParams := &ec2.DeleteInternetGatewayInput{
			InternetGatewayId: resp.Tags[0].ResourceId,
		}
		if _, err := ec2Svc.DeleteInternetGateway(deleteGatewayParams); err != nil {
			log.Warningf("Unable to delete internet gateway: %s\n", err.Error())
		}
	}

	return nil
}

func deleteVPC(ec2Svc *ec2.EC2, awsCluster *hpaws.AWSCluster) error {
	params := &ec2.DeleteVpcInput{
		VpcId: aws.String(awsCluster.VpcId),
	}

	if _, err := ec2Svc.DeleteVpc(params); err != nil {
		return errors.New("Unable to delete VPC: " + err.Error())
	}
	return nil
}

func deleteEC2(ec2Svc *ec2.EC2, awsCluster *hpaws.AWSCluster) error {
	var instanceIds []*string

	for _, id := range awsCluster.InstanceIds {
		instanceIds = append(instanceIds, id)
	}

	params := &ec2.TerminateInstancesInput{
		InstanceIds: instanceIds,
	}

	if _, err := ec2Svc.TerminateInstances(params); err != nil {
		return fmt.Errorf("Unable to terminate EC2 instance: %s\n", err.Error())
	}

	terminatedInstanceParams := &ec2.DescribeInstancesInput{
		InstanceIds: instanceIds,
	}

	if err := ec2Svc.WaitUntilInstanceTerminated(terminatedInstanceParams); err != nil {
		return fmt.Errorf("Unable to wait until EC2 instance terminated: %s\n", err.Error())
	}
	return nil
}

func deleteCluster(ecsSvc *ecs.ECS, awsCluster *hpaws.AWSCluster) error {
	params := &ecs.DeleteClusterInput{
		Cluster: aws.String(awsCluster.Name),
	}

	if _, err := ecsSvc.DeleteCluster(params); err != nil {
		return fmt.Errorf("Unable to delete cluster: %s", err.Error())
	}

	return nil
}

func (ecsDeployer *ECSDeployer) GetServiceMappings() (map[string]interface{}, error) {
	return nil, errors.New("Unimplemented")
}

func (ecsDeployer *ECSDeployer) GetServiceUrl(serviceName string) (string, error) {
	nodePort := ""
	taskFamilyName := ""
	for _, task := range ecsDeployer.Deployment.TaskDefinitions {
		for _, container := range task.ContainerDefinitions {
			if *container.Name == serviceName {
				nodePort = strconv.FormatInt(*container.PortMappings[0].HostPort, 10)
				taskFamilyName = *task.Family
				break
			}
		}
	}

	if nodePort == "" {
		return "", errors.New("Unable to find container in deployment container defintiions")
	}

	nodeId := -1
	for _, nodeMapping := range ecsDeployer.Deployment.NodeMapping {
		if nodeMapping.Task == taskFamilyName {
			nodeId = nodeMapping.Id
			break
		}
	}

	if nodeId == -1 {
		return "", errors.New("Unable to find task in deployment node mappings")
	}

	nodeInfo, nodeOk := ecsDeployer.AWSCluster.NodeInfos[nodeId]
	if !nodeOk {
		return "", errors.New("Unable to find node in cluster")
	}

	return nodeInfo.PublicDnsName + ":" + nodePort, nil
}

func (ecsDeployer *ECSDeployer) GetServiceAddress(serviceName string) (*apis.ServiceAddress, error) {
	var nodePort int32
	taskFamilyName := ""
	for _, task := range ecsDeployer.Deployment.TaskDefinitions {
		for _, container := range task.ContainerDefinitions {
			if *container.Name == serviceName {
				nodePort = int32(*container.PortMappings[0].HostPort)
				taskFamilyName = *task.Family
				break
			}
		}
	}

	if nodePort == 0 {
		return nil, errors.New("Unable to find container in deployment container defintiions")
	}

	nodeId := -1
	for _, nodeMapping := range ecsDeployer.Deployment.NodeMapping {
		if nodeMapping.Task == taskFamilyName {
			nodeId = nodeMapping.Id
			break
		}
	}

	if nodeId == -1 {
		return nil, errors.New("Unable to find task in deployment node mappings")
	}

	nodeInfo, nodeOk := ecsDeployer.AWSCluster.NodeInfos[nodeId]
	if !nodeOk {
		return nil, errors.New("Unable to find node in cluster")
	}

	return &apis.ServiceAddress{Host: nodeInfo.PublicDnsName, Port: nodePort}, nil
}

func (ecsDeployer *ECSDeployer) GetStoreInfo() interface{} {
	return nil
}

func (ecsDeployer *ECSDeployer) NewStoreInfo() interface{} {
	return nil
}
