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

// TODO: Add comments for describing each of the structs
type AuthType string

const (
	AuthTypeSA            AuthType = "gcp-sa-key"
	AuthTypeImpersonation AuthType = "impersonation"
	AuthTypeADC           AuthType = "adc"
)

type ComputeType string

const (
	ComputeTypeNode ComputeType = "nodes"
	ComputeTypePU   ComputeType = "processing-units"
)

type TargetInstance struct {
	ProjectID  string `json:"projectId"`
	InstanceID string `json:"instanceId"`
}

type Authentication struct {
	Type AuthType `json:"type"`

	// This is a pointer because structs with string slices can not be compared for zero values
	ImpersonateConfig *ImpersonateConfig `json:"impersonateConfig,omitempty"`
	IAMKeySecret      *IAMKeySecret      `json:"iamKeySecret,omitempty"`
}

type ImpersonateConfig struct {
	TargetServiceAccount string   `json:"targetServiceAccount"`
	Delegates            []string `json:"delegates,omitempty"`
}

type IAMKeySecret struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace,omitempty"`
	Key       string `json:"key"`
}

type ScaleConfig struct {
	ComputeType          ComputeType          `json:"computeType"`
	Nodes                ScaleConfigNodes     `json:"nodes,omitempty"`
	ProcessingUnits      ScaleConfigPUs       `json:"processingUnits,omitempty"`
	ScaledownStepSize    int                  `json:"scaledownStepSize,omitempty"`
	TargetCPUUtilization TargetCPUUtilization `json:"targetCPUUtilization"`
}

type ScaleConfigNodes struct {
	Min int `json:"min"`
	Max int `json:"max"`
}

type ScaleConfigPUs struct {
	Min int `json:"min"`
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
