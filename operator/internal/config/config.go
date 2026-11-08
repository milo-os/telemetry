package config

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	ctrl "sigs.k8s.io/controller-runtime"

	mulicluster "go.miloapis.com/milo/pkg/multicluster-runtime"
)

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +k8s:defaulter-gen=true

type TelemetryServicesOperator struct {
	metav1.TypeMeta

	Discovery                    DiscoveryConfig                    `json:"discovery"`
	DownstreamResourceManagement DownstreamResourceManagementConfig `json:"downstreamResourceManagement"`
}

// +k8s:deepcopy-gen=true

type DownstreamResourceManagementConfig struct {
	// KubeconfigPath is the path to the kubeconfig file to use when managing
	// downstream resources. When not provided, the operator will use the
	// in-cluster config.
	KubeconfigPath string `json:"kubeconfigPath"`
}

func (c *DownstreamResourceManagementConfig) RestConfig() (*rest.Config, error) {
	if c.KubeconfigPath == "" {
		return ctrl.GetConfig()
	}

	return clientcmd.BuildConfigFromFlags("", c.KubeconfigPath)
}

// +k8s:deepcopy-gen=true

type DiscoveryConfig struct {
	// Mode is the mode that the operator should use to discover clusters.
	//
	// Defaults to "single"
	Mode mulicluster.Provider `json:"mode"`

	// InternalServiceDiscovery will result in the operator to connect to internal
	// service addresses for projects.
	InternalServiceDiscovery bool `json:"internalServiceDiscovery"`

	// DiscoveryKubeconfigPath is the path to the kubeconfig file to use for
	// project discovery. When not provided, the operator will use the in-cluster
	// config.
	DiscoveryKubeconfigPath string `json:"discoveryKubeconfigPath"`

	// ProjectKubeconfigPath is the path to the kubeconfig file to use as a
	// template when connecting to project control planes. When not provided,
	// the operator will use the in-cluster config.
	ProjectKubeconfigPath string `json:"projectKubeconfigPath"`
}

func SetDefaults_DiscoveryConfig(obj *DiscoveryConfig) {
	if obj.Mode == "" {
		obj.Mode = mulicluster.ProviderSingle
	}
}

func (c *DiscoveryConfig) DiscoveryRestConfig() (*rest.Config, error) {
	if c.DiscoveryKubeconfigPath == "" {
		return ctrl.GetConfig()
	}

	return clientcmd.BuildConfigFromFlags("", c.DiscoveryKubeconfigPath)
}

func (c *DiscoveryConfig) ProjectRestConfig() (*rest.Config, error) {
	if c.ProjectKubeconfigPath == "" {
		return ctrl.GetConfig()
	}

	return clientcmd.BuildConfigFromFlags("", c.ProjectKubeconfigPath)
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(GroupVersion, &TelemetryServicesOperator{})
		metav1.AddToGroupVersion(s, GroupVersion)
		return nil
	})
}
