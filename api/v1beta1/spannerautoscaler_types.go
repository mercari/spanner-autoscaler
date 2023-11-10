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
	// [[Ref](https://cloud.google.com/kubernetes-engine/docs/how-to/workload-identity)].
	// This is a pointer because structs with string slices can not be compared for zero values
	ImpersonateConfig *ImpersonateConfig `json:"impersonateConfig,omitempty"`

	// Details of the k8s secret which contains the GCP service account authentication key (in JSON).
	// [[Ref](https://cloud.google.com/kubernetes-engine/docs/tutorials/authenticating-to-cloud-platform)].
	// This is a pointer because structs with string slices can not be compared for zero values
	IAMKeySecret *IAMKeySecret `json:"iamKeySecret,omitempty"`
}

// Details of the impersonation service account for GCP authentication
type ImpersonateConfig struct {
	// The service account which will be impersonated
	TargetServiceAccount string `json:"targetServiceAccount"`

	// Delegation chain for the service account impersonation.
	// [[Ref](https://pkg.go.dev/google.golang.org/api/impersonate#hdr-Required_IAM_roles)]
	Delegates []string `json:"delegates,omitempty"`
}

// Details of the secret which has the GCP service account key for authentication
type IAMKeySecret struct {
	// Name of the secret which contains the authentication key
	Name string `json:"name"`

	// Namespace of the secret which contains the authentication key
	Namespace string `json:"namespace,omitempty"`

	// Name of the yaml 'key' under which the authentication value is stored
	Key string `json:"key"`
}

// Details of the autoscaling parameters for the Spanner instance
type ScaleConfig struct {
	// Whether to use `nodes` or `processing-units` for scaling.
	// This is only used at the time of CustomResource creation. If compute capacity is provided in `nodes`, then it is automatically converted to `processing-units` at the time of resource creation, and internally, only `ProcessingUnits` are used for computations and scaling.
	ComputeType ComputeType `json:"computeType,omitempty"`

	// If `nodes` are provided at the time of resource creation, then they are automatically converted to `processing-units`. So it is recommended to use only the processing units. Ref: [Spanner Compute Capacity](https://cloud.google.com/spanner/docs/compute-capacity#compute_capacity)
	Nodes ScaleConfigNodes `json:"nodes,omitempty"`

	// ProcessingUnits for scaling of the Spanner instance. Ref: [Spanner Compute Capacity](https://cloud.google.com/spanner/docs/compute-capacity#compute_capacity)
	ProcessingUnits ScaleConfigPUs `json:"processingUnits,omitempty"`

	// The maximum number of processing units which can be deleted in one scale-down operation. It can be a multiple of 100 for values < 1000, or a multiple of 1000 otherwise.
	// +kubebuilder:default=2000
	ScaledownStepSize int `json:"scaledownStepSize,omitempty"`

	// How often autoscaler is reevaluated for scale down.
	// The cool down period between two consecutive scaledown operations. If this option is omitted, the value of the `--scale-down-interval` command line option is taken as the default value.
	ScaledownInterval *metav1.Duration `json:"scaledownInterval,omitempty"`

	// The maximum number of processing units which can be added in one scale-up operation. It can be a multiple of 100 for values < 1000, or a multiple of 1000 otherwise.
	// +kubebuilder:default=2000
	ScaleupStepSize int `json:"scaleupStepSize,omitempty"`

	// How often autoscaler is reevaluated for scale up.
	// The warm up period between two consecutive scaleup operations. If this option is omitted, the value of the `--scale-up-interval` command line option is taken as the default value.
	ScaleupInterval *metav1.Duration `json:"scaleupInterval,omitempty"`

	// The CPU utilization which the autoscaling will try to achieve. Ref: [Spanner CPU utilization](https://cloud.google.com/spanner/docs/cpu-utilization#task-priority)
	TargetCPUUtilization TargetCPUUtilization `json:"targetCPUUtilization"`
}

// Compute capacity in terms of Nodes
type ScaleConfigNodes struct {
	// Minimum number of Nodes for the autoscaling range
	Min int `json:"min,omitempty"`

	// Maximum number of Nodes for the autoscaling range
	Max int `json:"max,omitempty"`
}

// Compute capacity in terms of Processing Units
type ScaleConfigPUs struct {
	// Minimum number of Processing Units for the autoscaling range
	// +kubebuilder:validation:MultipleOf=100
	Min int `json:"min"`

	// Maximum number of Processing Units for the autoscaling range
	// +kubebuilder:validation:MultipleOf=100
	Max int `json:"max"`
}

type TargetCPUUtilization struct {
	// Desired CPU utilization for 'High Priority' CPU consumption category. Ref: [Spanner CPU utilization](https://cloud.google.com/spanner/docs/cpu-utilization#task-priority)
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=100
	// +kubebuilder:validation:ExclusiveMinimum=true
	// +kubebuilder:validation:ExclusiveMaximum=true
	HighPriority int `json:"highPriority"`
}

// SpannerAutoscalerSpec defines the desired state of SpannerAutoscaler
type SpannerAutoscalerSpec struct {
	// The Spanner instance which will be managed for autoscaling
	TargetInstance TargetInstance `json:"targetInstance"`

	// Authentication details for the Spanner instance
	Authentication Authentication `json:"authentication,omitempty"`

	// Details of the autoscaling parameters for the Spanner instance
	ScaleConfig ScaleConfig `json:"scaleConfig"`
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

// A `SpannerAutoscaleSchedule` which is currently active and will be used for calculating the autoscaling range.
type ActiveSchedule struct {
	// Name of the `SpannerAutoscaleSchedule`
	ScheduleName string `json:"name"`

	// The time until when this schedule will remain active
	EndTime metav1.Time `json:"endTime"`

	// The extra compute capacity which will be added because of this schedule
	AdditionalPU int `json:"additionalPU"`
}

// SpannerAutoscalerStatus defines the observed state of SpannerAutoscaler
type SpannerAutoscalerStatus struct {
	// List of schedules which are registered with this spanner-autoscaler instance
	Schedules []string `json:"schedules,omitempty"`

	// List of all the schedules which are currently active and will be used in calculating compute capacity
	CurrentlyActiveSchedules []ActiveSchedule `json:"currentlyActiveSchedules,omitempty"`

	// Last time the `SpannerAutoscaler` scaled the number of Spanner nodes.
	// Used by the autoscaler to control how often the number of nodes are changed
	LastScaleTime metav1.Time `json:"lastScaleTime,omitempty"`

	// Last time the `SpannerAutoscaler` fetched and synced the metrics from Spanner
	LastSyncTime metav1.Time `json:"lastSyncTime,omitempty"`

	// Current number of processing-units in the Spanner instance
	CurrentProcessingUnits int `json:"currentProcessingUnits,omitempty"`

	// Desired number of processing-units in the Spanner instance
	DesiredProcessingUnits int `json:"desiredProcessingUnits,omitempty"`

	// Minimum number of processing units based on the currently active schedules
	DesiredMinPUs int `json:"desiredMinPUs,omitempty"`

	// Maximum number of processing units based on the currently active schedules
	DesiredMaxPUs int `json:"desiredMaxPUs,omitempty"`

	// State of the Cloud Spanner instance
	InstanceState InstanceState `json:"instanceState,omitempty"`

	// Current average CPU utilization for high priority task, represented as a percentage
	CurrentHighPriorityCPUUtilization int `json:"currentHighPriorityCPUUtilization,omitempty"`
}

//+kubebuilder:object:root=true
//+kubebuilder:subresource:status
//+kubebuilder:storageversion
//+kubebuilder:printcolumn:name="Project Id",type="string",JSONPath=".spec.targetInstance.projectId"
//+kubebuilder:printcolumn:name="Instance Id",type="string",JSONPath=".spec.targetInstance.instanceId"
//+kubebuilder:printcolumn:name="Current PUs",type="integer",JSONPath=".status.currentProcessingUnits"
//+kubebuilder:printcolumn:name="Desired PUs",type="integer",JSONPath=".status.desiredProcessingUnits"
//+kubebuilder:printcolumn:name="Desired Min PUs",type="integer",JSONPath=".status.desiredMinPUs"
//+kubebuilder:printcolumn:name="Desired Max PUs",type="integer",JSONPath=".status.desiredMaxPUs"
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
