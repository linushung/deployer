package clustermanagers

import (
	"errors"
	"fmt"
	"strings"

	"github.com/hyperpilotio/deployer/apis"
	"github.com/hyperpilotio/deployer/aws"
	"github.com/hyperpilotio/deployer/clustermanagers/awsecs"
	"github.com/hyperpilotio/deployer/clustermanagers/kubernetes"
	"github.com/hyperpilotio/deployer/job"
	"github.com/hyperpilotio/go-utils/log"
	"github.com/pborman/uuid"
	"github.com/spf13/viper"
)

// Cluster manager specific Deployer, able to deployer containers and services
type Deployer interface {
	CreateDeployment(uploadedFiles map[string]string) (interface{}, error)
	UpdateDeployment() error
	DeployExtensions(extensions *apis.Deployment, mergedDeployment *apis.Deployment) error
	DeleteDeployment() error
	CreateInClusterDeployment(uploadedFiles map[string]string, inCluster interface{}) (interface{}, error)
	ReloadClusterState(storeInfo interface{}) error
	GetStoreInfo() interface{}
	// TODO(tnachen): Eventually we should support multiple clouds, then we need to abstract AWSCluster
	GetAWSCluster() *aws.AWSCluster
	GetLog() *log.FileLog
	GetScheduler() *job.Scheduler
	GetServiceUrl(serviceName string) (string, error)
	GetServiceAddress(serviceName string) (*apis.ServiceAddress, error)
	GetServiceMappings() (map[string]interface{}, error)
}

type InCluster interface {
	GetLog() *log.FileLog
}

func NewDeployer(
	config *viper.Viper,
	awsProfile *aws.AWSProfile,
	deployType string,
	deployment *apis.Deployment,
	createName bool) (Deployer, error) {

	if createName {
		deployment.Name = CreateUniqueDeploymentName(deployment.Name)
	}

	switch deployType {
	case "ECS":
		return awsecs.NewDeployer(config, awsProfile, deployment)
	case "K8S":
		return kubernetes.NewDeployer(config, awsProfile, deployment)
	default:
		return nil, errors.New("Unsupported deploy type: " + deployType)
	}
}

func NewInCluster(deployType string, filesPath string, deployment *apis.Deployment) (InCluster, error) {
	switch deployType {
	case "ECS":
		return awsecs.NewInCluster(filesPath, deployment)
	case "K8S":
		return kubernetes.NewInCluster(filesPath, deployment)
	default:
		return nil, errors.New("Unsupported deploy type: " + deployType)
	}
}

func CreateUniqueDeploymentName(familyName string) string {
	randomId := strings.ToUpper(strings.Split(uuid.NewUUID().String(), "-")[0])
	return fmt.Sprintf("%s-%s", familyName, randomId)
}
