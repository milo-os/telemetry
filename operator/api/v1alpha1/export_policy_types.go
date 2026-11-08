// SPDX-License-Identifier: AGPL-3.0-only

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// NOTE: json tags are required.  Any new fields you add must have json tags for
// the fields to be serialized.

// ExportPolicySpec defines the desired state of ExportPolicy.
type ExportPolicySpec struct {
	// INSERT ADDITIONAL SPEC FIELDS - desired state of cluster Important: Run
	// "make" to regenerate code after modifying this file

	// Defines how the export policy should source telemetry data to publish to
	// the configured sinks. An export policy can define multiple telemetry
	// sources. The export policy will **not** de-duplicate telemetry data that
	// matches multiple sources.
	//
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=20
	// +listType=map
	// +listMapKey=name
	Sources []TelemetrySource `json:"sources"`

	// Configures how telemetry data should be sent to a third-party telemetry
	// platforms.
	//
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=20
	// +listType=map
	// +listMapKey=name
	Sinks []TelemetrySink `json:"sinks"`
}

// ExportPolicyStatus defines the observed state of ExportPolicy.
type ExportPolicyStatus struct {
	// Provides summary status information on the export policy as a whole. Review
	// the sink status information for detailed information on each sink.
	//
	// Known condition types are: "Ready"
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// Provides status information on each sink that's configured.
	Sinks []SinkStatus `json:"sinks,omitempty"`
}

// SinkStatus provides status information on the current status of a sink. This
// can be used to determine whether a sink is configured correctly and is
// exporting telemetry data.
type SinkStatus struct {
	// The name of the corresponding sink configuration in the spec of the export
	// policy.
	Name string `json:"name"`
	// Provides status information on the current status of the sink. This can be
	// used to determine whether a sink is configured correctly and is exporting
	// telemetry data.
	//
	// Known condition types are: "Ready"
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:metadata:annotations="discovery.miloapis.com/parent-contexts=Project"

// ExportPolicy is the Schema for the export policy API.
type ExportPolicy struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// Describes the expected state of the ExportPolicy's configuration. The
	// control plane will constantly evaluate the current state of exporters that
	// are deployed and ensure it matches the expected configuration. This field
	// is required when configuring an export policy.
	Spec ExportPolicySpec `json:"spec"`

	// Provides information on the current state of the export policy that was
	// observed by the control plane. This will be continuously updated as the
	// control plane monitors exporters.
	Status ExportPolicyStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// ExportPolicyList contains a list of ExportPolicy.
type ExportPolicyList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ExportPolicy `json:"items"`
}

// A metric source configures the metric data that should be exported to the
// configured sinks. The options below are expected to be mutually exclusive.
type MetricSource struct {
	// The MetricSQL option allows to user to provide a metricsql query that can
	// be used to select and filter metric data that should be published by the
	// export policy.
	//
	// Here's an example of a metricsql query that will publish gateway metrics:
	//
	// ``` {service_name=“networking.miloapis.com”, resource_kind="Gateway"} ```
	//
	// See: https://docs.victoriametrics.com/metricsql/
	MetricsQL string `json:"metricsql,omitempty"`
}

// Defines how the export policy should source telemetry data from resources on
// the platform.
type TelemetrySource struct {
	// A unique name given to the telemetry source within an export policy. Must
	// be a valid DNS label.
	//
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=^[a-z0-9]([-a-z0-9]*[a-z0-9])?$
	Name string `json:"name"`

	// Configures how the telemetry source should retrieve metric data from the
	// Datum Cloud platform.
	Metrics *MetricSource `json:"metrics,omitempty"`
}

// Configures how telemetry data should be sent to a third-party platform. As of
// now there are no guarantees around delivery of telemetry data, especially if
// the sink's endpoint is unavailable.
type TelemetrySink struct {
	// A name provided to the telemetry sink that's unique within the export
	// policy.
	//
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=^[a-z0-9]([-a-z0-9]*[a-z0-9])?$
	Name string `json:"name"`

	// A list of sources that should be sent to the telemetry sink.
	//
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=20
	Sources []string `json:"sources"`

	// Configures the target of the telemetry sink.
	//
	// +kubebuilder:validation:Required
	Target *SinkTarget `json:"target"`
}

// Configures the target of the telemetry sink. The target defines the protocol
// that's used to send telemetry data to the sink. Only one target protocol can
// be configured per sink.
type SinkTarget struct {
	// Configures the export policy to publish telemetry using the Prometheus
	// Remote Write protocol.
	PrometheusRemoteWrite *PrometheusRemoteWriteSink `json:"prometheusRemoteWrite,omitempty"`
}

// References a secret in the same namespace as the entity defining the
// reference.
type LocalSecretReference struct {
	// The name of the secret
	//
	// +kubebuilder:validation:Required
	Name string `json:"name"`
}

// Configures how the sink should use Basic Auth for authenticating with a
// telemetry endpoint.
type BasicAuthAuthentication struct {
	// Configures which secret is used to retrieve the bearer token to add to the
	// authorization header. Secret must be a `kubernetes.io/basic-auth` type.
	//
	// +kubebuilder:validation:Required
	SecretRef LocalSecretReference `json:"secretRef"`
}

// Configures how the sink will authenticate with the configured endpoint. These
// options are mutually exclusive.
type Authentication struct {
	// Configures the sink to use basic auth to authenticate with the configured
	// endpoint.
	BasicAuth *BasicAuthAuthentication `json:"basicAuth,omitempty"`
}

// Configures how the sink should send data to a OTLP HTTP endpoint.
type PrometheusRemoteWriteSink struct {
	// Configures how the sink should authenticate with the HTTP endpoint.
	Authentication *Authentication `json:"authentication,omitempty"`

	// Configure an HTTP endpoint to use for publishing telemetry data.
	//
	// +kubebuilder:validation:Required
	Endpoint string `json:"endpoint"`

	// Configures how telemetry data should be batched before sending to the sink.
	// By default, the sink will batch telemetry data every 5 seconds or when
	// the batch size reaches 500 entries, whichever comes first.
	//
	// +kubebuilder:default={timeout: "5s", maxSize: 500}
	Batch Batch `json:"batch"`

	// Configures the export policies' retry behavior when it fails to send
	// requests to the sink's endpoint. There's no guarantees that the export
	// policy will retry until success if the endpoint is not available or
	// configured incorrectly.
	//
	// +kubebuilder:default={maxAttempts: 3, backoffDuration: "5s"}
	Retry Retry `json:"retry"`
}

// Configures the batching behavior the sink will use to batch requests before
// publishing them to the endpoint.
type Batch struct {
	// Batch timeout before sending telemetry. Must be a duration (e.g. 5s).
	//
	// +kubebuilder:validation:Required
	Timeout metav1.Duration `json:"timeout"`
	// Maximum number of telemetry entries per batch.
	//
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=5000
	MaxSize int `json:"maxSize"`
}

// Configures the retry behavior of the sink when it fails to send telemetry
// data to the configured endpoint.
type Retry struct {
	// Maximum number of attempts before telemetry data should be dropped.
	//
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=10
	MaxAttempts int `json:"maxAttempts"`
	// Backoff duration that should be used to backoff when retrying requests.
	//
	// +kubebuilder:validation:Required
	BackoffDuration metav1.Duration `json:"backoffDuration"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(GroupVersion, &ExportPolicy{}, &ExportPolicyList{})
		metav1.AddToGroupVersion(s, GroupVersion)
		return nil
	})
}
