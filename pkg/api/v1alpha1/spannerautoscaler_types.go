/*

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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ScaleTargetRef defines the target reference for scaling.
type ScaleTargetRef struct {
	// +kubebuilder:validation:MinLength=1
	ProjectID *string `json:"projectId"`

	// +kubebuilder:validation:MinLength=1
	InstanceID *string `json:"instanceId"`
}

type ServiceAccountSecretRef struct {
	// +kubebuilder:validation:MinLength=1
	Name *string `json:"name"`

	Namespace *string `json:"namespace,omitempty"`

	// +kubebuilder:validation:MinLength=1
	Key *string `json:"key"`
}

// TargetCPUUtilization defines the utilization of Cloud Spanner CPU
type TargetCPUUtilization struct {
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=100
	// fraction of the requested CPU that should be utilized/used,
	// e.g. 70 means that 70% of the requested CPU should be in use.
	HighPriority *int32 `json:"highPriority"`
}

type ImpersonateConfig struct {
	TargetServiceAccount string `json:"targetServiceAccount"`

	// +kubebuilder:validation:Optional
	Delegates []string `json:"delegates,omitempty"`
}

// SpannerAutoscalerSpec defines the desired state of SpannerAutoscaler
type SpannerAutoscalerSpec struct {
	// target reference for scaling.
	ScaleTargetRef ScaleTargetRef `json:"scaleTargetRef"`

	// +kubebuilder:validation:Optional
	// reference for service account secret.
	// If not specified, use ADC of the controller.
	ServiceAccountSecretRef *ServiceAccountSecretRef `json:"serviceAccountSecretRef"`

	// +kubebuilder:validation:Optional
	ImpersonateConfig *ImpersonateConfig `json:"impersonateConfig,omitempty"`

	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Optional
	// lower limit for the number of nodes that can be set by the autoscaler.
	MinNodes *int32 `json:"minNodes,omitempty"`

	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Optional
	// upper limit for the number of nodes that can be set by the autoscaler.
	// It cannot be smaller than MinNodes.
	MaxNodes *int32 `json:"maxNodes,omitempty"`

	// +kubebuilder:validation:Minimum=100
	// +kubebuilder:validation:MultipleOf=100
	// +kubebuilder:validation:Optional
	// lower limit for the number of nodes that can be set by the autoscaler.
	MinProcessingUnits *int32 `json:"minProcessingUnits,omitempty"`

	// +kubebuilder:validation:Minimum=100
	// +kubebuilder:validation:MultipleOf=100
	// +kubebuilder:validation:Optional
	// upper limit for the number of nodes that can be set by the autoscaler.
	// It cannot be smaller than MinProcessingUnits.
	MaxProcessingUnits *int32 `json:"maxProcessingUnits,omitempty"`

	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Optional
	// upper limit for the number of nodes when autoscaler scaledown.
	MaxScaleDownNodes *int32 `json:"maxScaleDownNodes"`

	// target average CPU utilization for Spanner.
	TargetCPUUtilization TargetCPUUtilization `json:"targetCPUUtilization"`
}

type InstanceState string

const (
	InstanceStateUnspecified InstanceState = "unspecified"
	// The instance is still being created. Resources may not be
	// available yet, and operations such as database creation may not
	// work.
	InstanceStateCreating InstanceState = "creating"
	// The instance is fully created and ready to do work such as
	// creating databases.
	InstanceStateReady InstanceState = "ready"
)

// SpannerAutoscalerStatus defines the observed state of SpannerAutoscaler
type SpannerAutoscalerStatus struct {
	// last time the SpannerAutoscaler scaled the number of Spanner nodes.
	// used by the autoscaler to control how often the number of nodes is changed.
	LastScaleTime *metav1.Time `json:"lastScaleTime,omitempty"`

	// last time the SpannerAutoscaler synced the Spanner status.
	LastSyncTime *metav1.Time `json:"lastSyncTime,omitempty"`

	// current number of nodes of Spanner managed by this autoscaler.
	CurrentNodes *int32 `json:"currentNodes,omitempty"`

	// current number of nodes of Spanner managed by this autoscaler.
	CurrentProcessingUnits *int32 `json:"currentProcessingUnits,omitempty"`

	// desired number of nodes of Spanner managed by this autoscaler.
	DesiredNodes *int32 `json:"desiredNodes,omitempty"`

	// desired number of nodes of Spanner managed by this autoscaler.
	DesiredProcessingUnits *int32 `json:"desiredProcessingUnits,omitempty"`

	// +kubebuilder:validation:Type=string
	InstanceState InstanceState `json:"instanceState"`

	// current average CPU utilization for high priority task, represented as a percentage.
	CurrentHighPriorityCPUUtilization *int32 `json:"currentHighPriorityCPUUtilization,omitempty"`
}

// +kubebuilder:object:root=true
// +k8s:openapi-gen=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Project Id",type="string",JSONPath=".spec.scaleTargetRef.projectId"
// +kubebuilder:printcolumn:name="Instance Id",type="string",JSONPath=".spec.scaleTargetRef.instanceId"
// +kubebuilder:printcolumn:name="Min Nodes",type="integer",JSONPath=".spec.minNodes"
// +kubebuilder:printcolumn:name="Max Nodes",type="integer",JSONPath=".spec.maxNodes"
// +kubebuilder:printcolumn:name="Min PUs",type="integer",JSONPath=".spec.minProcessingUnits"
// +kubebuilder:printcolumn:name="Max PUs",type="integer",JSONPath=".spec.maxProcessingUnits"
// +kubebuilder:printcolumn:name="Target CPU",type="integer",JSONPath=".spec.targetCPUUtilization.highPriority"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

// SpannerAutoscaler is the Schema for the spannerautoscalers API
type SpannerAutoscaler struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   SpannerAutoscalerSpec   `json:"spec,omitempty"`
	Status SpannerAutoscalerStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// SpannerAutoscalerList contains a list of SpannerAutoscaler
type SpannerAutoscalerList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []SpannerAutoscaler `json:"items"`
}

func init() {
	SchemeBuilder.Register(&SpannerAutoscaler{}, &SpannerAutoscalerList{})
}
