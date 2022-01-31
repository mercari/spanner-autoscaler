/*
Copyright 2022.

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

package v1beta1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Type for specifying authentication methods
// +kubebuilder:validation:Enum=gcp-sa-key;impersonation;adc
type AuthType string

const (
	AuthTypeSA            AuthType = "gcp-sa-key"
	AuthTypeImpersonation AuthType = "impersonation"
	AuthTypeADC           AuthType = "adc"
)

// Type for specifying compute capacity categories
// +kubebuilder:validation:Enum=nodes;processing-units
type ComputeType string

const (
	ComputeTypeNode ComputeType = "nodes"
	ComputeTypePU   ComputeType = "processing-units"
)

// The Spanner instance which will be managed for autoscaling
type TargetInstance struct {
	// The GCP Project id of the Spanner instance
	ProjectID string `json:"projectId"`

	// The instance id of the Spanner instance
	InstanceID string `json:"instanceId"`
}

// Authentication details for the Spanner instance
type Authentication struct {
	// Authentication method to be used for GCP authentication.
	// If `ImpersonateConfig` as well as `IAMKeySecret` is nil, this will be set to use ADC be default.
	Type AuthType `json:"type,omitempty"`

	// Details of the GCP service account which will be impersonated, for authentication to GCP.
	// This can used only on GKE clusters, when workload identity is enabled.
	// Ref: https://cloud.google.com/kubernetes-engine/docs/how-to/workload-identity
	// This is a pointer because structs with string slices can not be compared for zero values
	ImpersonateConfig *ImpersonateConfig `json:"impersonateConfig,omitempty"`

	// Details of the k8s secret which contains the GCP service account authentication key (in JSON).
	// Ref: https://cloud.google.com/kubernetes-engine/docs/tutorials/authenticating-to-cloud-platform
	// This is a pointer because structs with string slices can not be compared for zero values
	IAMKeySecret *IAMKeySecret `json:"iamKeySecret,omitempty"`
}

// Details of the impersonation service account for GCP authentication
type ImpersonateConfig struct {
	TargetServiceAccount string   `json:"targetServiceAccount"`
	Delegates            []string `json:"delegates,omitempty"`
}

// Details of the secret which has the GCP service account key for authentication
type IAMKeySecret struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace,omitempty"`
	Key       string `json:"key"`
}

// Details of the autoscaling parameters for the Spanner instance
type ScaleConfig struct {
	// Whether to use `nodes` or `processing-units` for scaling.
	// This is only used at the time of CustomResource creation. If compute capacity is provided in `nodes`, then it is automatically converted to `processing-units` at the time of resource creation, and internally, only `ProcessingUnits` are used for computations and scaling.
	ComputeType ComputeType `json:"computeType,omitempty"`

	// If `nodes` are provided at the time of resource creation, then they are automatically converted to `processing-units`. So it is recommended to use only the processing units.
	Nodes ScaleConfigNodes `json:"nodes,omitempty"`

	// ProcessingUnits for scaling of the Spanner instance: https://cloud.google.com/spanner/docs/compute-capacity#compute_capacity
	ProcessingUnits ScaleConfigPUs `json:"processingUnits,omitempty"`

	// The maximum number of processing units which can be deleted in one scale-down operation
	// +kubebuilder:default=2000
	ScaledownStepSize int `json:"scaledownStepSize,omitempty"`

	// The CPU utilization which the autoscaling will try to achieve
	TargetCPUUtilization TargetCPUUtilization `json:"targetCPUUtilization"`
}

type ScaleConfigNodes struct {
	Min int `json:"min,omitempty"`
	Max int `json:"max,omitempty"`
}

type ScaleConfigPUs struct {
	// +kubebuilder:validation:MultipleOf=100
	Min int `json:"min"`

	// +kubebuilder:validation:MultipleOf=100
	Max int `json:"max"`
}

type TargetCPUUtilization struct {
	HighPriority int `json:"highPriority"`
}

// SpannerAutoscalerSpec defines the desired state of SpannerAutoscaler
type SpannerAutoscalerSpec struct {
	TargetInstance TargetInstance `json:"targetInstance"`
	Authentication Authentication `json:"authentication"`
	ScaleConfig    ScaleConfig    `json:"scaleConfig"`
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
	Schedules                []string `json:"schedules,omitempty"`
	CurrentlyActiveSchedules []string `json:"currentlyActiveSchedules,omitempty"`

	// Last time the SpannerAutoscaler scaled the number of Spanner nodes
	// Used by the autoscaler to control how often the number of nodes are changed
	LastScaleTime metav1.Time `json:"lastScaleTime,omitempty"`

	// Last time the SpannerAutoscaler fetched and synced this status
	LastSyncTime metav1.Time `json:"lastSyncTime,omitempty"`

	// Current number of nodes in the Spanner instance
	CurrentNodes int `json:"currentNodes,omitempty"`

	// Current number of processing-units in the Spanner instance
	CurrentProcessingUnits int `json:"currentProcessingUnits,omitempty"`

	// Desired number of nodes in the Spanner instance
	DesiredNodes int `json:"desiredNodes,omitempty"`

	// Desired number of processing-units in the Spanner instance
	DesiredProcessingUnits int `json:"desiredProcessingUnits,omitempty"`

	InstanceState InstanceState `json:"instanceState"`

	// Current average CPU utilization for high priority task, represented as a percentage
	CurrentHighPriorityCPUUtilization int `json:"currentHighPriorityCPUUtilization,omitempty"`
}

//+kubebuilder:object:root=true
//+kubebuilder:subresource:status
//+kubebuilder:storageversion
//+kubebuilder:printcolumn:name="Project Id",type="string",JSONPath=".spec.targetInstance.projectId"
//+kubebuilder:printcolumn:name="Instance Id",type="string",JSONPath=".spec.targetInstance.instanceId"
//+kubebuilder:printcolumn:name="Min Nodes",type="integer",JSONPath=".spec.scaleConfig.nodes.min"
//+kubebuilder:printcolumn:name="Max Nodes",type="integer",JSONPath=".spec.scaleConfig.nodes.max"
//+kubebuilder:printcolumn:name="Current Nodes",type="integer",JSONPath=".status.currentNodes"
//+kubebuilder:printcolumn:name="Min PUs",type="integer",JSONPath=".spec.scaleConfig.processingUnits.min"
//+kubebuilder:printcolumn:name="Max PUs",type="integer",JSONPath=".spec.scaleConfig.processingUnits.max"
//+kubebuilder:printcolumn:name="Current PUs",type="integer",JSONPath=".status.currentProcessingUnits"
//+kubebuilder:printcolumn:name="Target CPU",type="integer",JSONPath=".spec.scaleConfig.targetCPUUtilization.highPriority"
//+kubebuilder:printcolumn:name="Current CPU",type="integer",JSONPath=".status.currentHighPriorityCPUUtilization"
//+kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

// SpannerAutoscaler is the Schema for the spannerautoscalers API
type SpannerAutoscaler struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   SpannerAutoscalerSpec   `json:"spec,omitempty"`
	Status SpannerAutoscalerStatus `json:"status,omitempty"`
}

//+kubebuilder:object:root=true

// SpannerAutoscalerList contains a list of SpannerAutoscaler
type SpannerAutoscalerList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []SpannerAutoscaler `json:"items"`
}

func init() {
	SchemeBuilder.Register(&SpannerAutoscaler{}, &SpannerAutoscalerList{})
}
