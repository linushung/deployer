package awsk8s

import (
	"github.com/hyperpilotio/deployer/apis"
	"github.com/hyperpilotio/deployer/clusters/aws"
	"github.com/hyperpilotio/deployer/job"
	"github.com/hyperpilotio/go-utils/log"
	"github.com/spf13/viper"

	"k8s.io/client-go/rest"
)

type ServiceMapping struct {
	NodeId     int    `json:"nodeId"`
	NodeName   string `json:"nodeName"`
	PublicUrl  string `json:"publicUrl"`
	PrivateUrl string `json:"privateUrl"`
}

type K8SDeployer struct {
	Config     *viper.Viper
	AWSCluster *aws.AWSCluster

	DeploymentLog *log.FileLog
	Deployment    *apis.Deployment
	Scheduler     *job.Scheduler

	BastionIp              string
	MasterIp               string
	KubeConfigPath         string
	Services               map[string]ServiceMapping
	KubeConfig             *rest.Config
	VpcPeeringConnectionId string
}

type CreateDeploymentResponse struct {
	Name      string                    `json:"name"`
	Services  map[string]ServiceMapping `json:"services"`
	BastionIp string                    `json:"bastionIp"`
	MasterIp  string                    `json:"masterIp"`
}

type DeploymentLoadBalancers struct {
	StackName             string
	ApiServerBalancerName string
	LoadBalancerNames     []string
}

type StoreInfo struct {
	BastionIp              string
	MasterIp               string
	VpcPeeringConnectionId string
}
