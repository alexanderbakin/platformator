/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// +kubebuilder:validation:XValidation:rule="!has(self.minReplicas) || !has(self.maxReplicas) || self.maxReplicas >= self.minReplicas",message="maxReplicas must be greater than or equal to minReplicas"
type AppSpec struct {
	// image is the container image to run, for example "nginx:1.27".
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=255
	// +kubebuilder:validation:Pattern=`^\S+$`
	Image string `json:"image"`

	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	Port int32 `json:"port"`

	// healthCheck switches readiness from a TCP connect on the port to an HTTP
	// request on a path, and can add a liveness probe on a separate path.
	// Without it, readiness is a TCP connect on the port and there is no
	// liveness probe.
	// +optional
	HealthCheck *HealthCheck `json:"healthCheck,omitempty"`

	// +optional
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:default=1
	MinReplicas *int32 `json:"minReplicas,omitempty"`

	// +optional
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:default=1
	MaxReplicas *int32 `json:"maxReplicas,omitempty"`

	// +optional
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=100
	// +kubebuilder:default=50
	TargetCPUUtilizationPercentage *int32 `json:"targetCPUUtilizationPercentage,omitempty"`

	// +optional
	Env []corev1.EnvVar `json:"env"`

	// +kubebuilder:validation:Required
	// +kubebuilder:validation:XValidation:rule="('cpu' in self.limits && 'cpu' in self.requests && 'memory' in self.limits && 'memory' in self.requests) && !quantity(self.limits['cpu']).isLessThan(quantity(self.requests['cpu'])) && !quantity(self.limits['memory']).isLessThan(quantity(self.requests['memory']))",message="CPU and memory limits/requests must be set and limits must be greater than or equal to requests"
	Resources corev1.ResourceRequirements `json:"resources"`
}

// HealthCheck configures the HTTP probes of an App.
type HealthCheck struct {
	// path is the HTTP path that answers with a 2xx or 3xx status while the app
	// is ready to receive traffic, for example "/healthz". It is requested on
	// the App's port and is used for the readiness probe only. It may check
	// dependencies such as a database: a failing readiness probe only takes the
	// pod out of the Service, it does not restart it.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Pattern=`^/\S*$`
	Path string `json:"path"`

	// livenessPath adds a liveness probe on this HTTP path. Keep it cheap and
	// independent of other services (for example "/livez"): a failing liveness
	// probe restarts the container, so if it checked a database, a database
	// outage would restart every pod at once. Without it there is no liveness
	// probe.
	// +optional
	// +kubebuilder:validation:Pattern=`^/\S*$`
	LivenessPath string `json:"livenessPath,omitempty"`
}

// AppStatus defines the observed state of App.
type AppStatus struct {
	// url is the public address of the App, set once its Ingress exists.
	// +optional
	URL string `json:"url,omitempty"`

	// INSERT ADDITIONAL STATUS FIELD - define observed state of cluster
	// Important: Run "make" to regenerate code after modifying this file

	// For Kubernetes API conventions, see:
	// https://github.com/kubernetes/community/blob/master/contributors/devel/sig-architecture/api-conventions.md#typical-status-properties

	// conditions represent the current state of the App resource.
	// Each condition has a unique type and reflects the status of a specific aspect of the resource.
	//
	// Standard condition types include:
	// - "Available": the resource is fully functional
	// - "Progressing": the resource is being created or updated
	// - "Degraded": the resource failed to reach or maintain its desired state
	//
	// The status of each condition is one of True, False, or Unknown.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Reason",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].reason`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
// +kubebuilder:printcolumn:name="Image",type=string,priority=1,JSONPath=`.spec.image`
// +kubebuilder:printcolumn:name="URL",type=string,priority=1,JSONPath=`.status.url`
// +kubebuilder:printcolumn:name="Reconciled",type=string,priority=1,JSONPath=`.status.conditions[?(@.type=="Reconciled")].status`
// +kubebuilder:printcolumn:name="TLS",type=string,priority=1,JSONPath=`.status.conditions[?(@.type=="CertificateReady")].status`
// +kubebuilder:validation:XValidation:rule="self.metadata.name.matches('^[a-z]([-a-z0-9]*[a-z0-9])?$') && size(self.metadata.name) <= 63",message="name must be a DNS label (lowercase letters, digits and dashes, starting with a letter, at most 63 characters) because it becomes the Service name and the hostname"

// App is the Schema for the apps API
type App struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of App
	// +required
	Spec AppSpec `json:"spec"`

	// status defines the observed state of App
	// +optional
	Status AppStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// AppList contains a list of App
type AppList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []App `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &App{}, &AppList{})
		return nil
	})
}
