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

// The recurring frequency and the length of time for which a schedule will remain active
type Schedule struct {
	// The recurring frequency of the schedule in [standard cron](https://en.wikipedia.org/wiki/Cron) format.
	// Extended syntax (`L`, `L-n`, `nW`, `LW`, `DAY#n`, `DAY#L`) is also supported — see [go-cron Extended Syntax](https://pkg.go.dev/github.com/netresearch/go-cron#hdr-Extended_Syntax__Optional_).
	// Examples and verification utility: https://crontab.guru
	Cron string `json:"cron"`

	// The length of time for which this schedule will remain active each time the cron is triggered.
	Duration string `json:"duration"`
}

// MaxPUPolicy defines how additionalProcessingUnits interacts with the
// target SpannerAutoscaler's max processing units.
// +kubebuilder:validation:Enum=Exceed;Cap
type MaxPUPolicy string

const (
	// MaxPUPolicyExceed raises both ends of the autoscaling range, allowing
	// the instance to scale beyond `spec.scaleConfig.processingUnits.max`
	// (current behavior; default).
	MaxPUPolicyExceed MaxPUPolicy = "Exceed"

	// MaxPUPolicyCap raises only the lower bound of the autoscaling range.
	// `spec.scaleConfig.processingUnits.max` remains the hard ceiling.
	MaxPUPolicyCap MaxPUPolicy = "Cap"
)

// Normalized resolves the empty value to the MaxPUPolicyExceed default.
// Specs and status entries written before this field existed (or by older
// controllers) carry an empty policy; every consumer must treat them as
// Exceed, and this method is the single place that rule lives.
func (p MaxPUPolicy) Normalized() MaxPUPolicy {
	if p == "" {
		return MaxPUPolicyExceed
	}
	return p
}

// SpannerAutoscaleScheduleSpec defines the desired state of SpannerAutoscaleSchedule
type SpannerAutoscaleScheduleSpec struct {
	// The `SpannerAutoscaler` resource name with which this schedule will be registered.
	// Immutable after creation.
	TargetResource string `json:"targetResource"`

	// The extra compute capacity which will be added when this schedule is active.
	// While active, this value is added to the minimum — and, unless maxPUPolicy is
	// `Cap`, also the maximum — of the target SpannerAutoscaler's autoscaling range,
	// so by default the instance can be scaled beyond
	// `spec.scaleConfig.processingUnits.max` by this amount.
	AdditionalProcessingUnits int `json:"additionalProcessingUnits"`

	// How additionalProcessingUnits interacts with the target SpannerAutoscaler's
	// `spec.scaleConfig.processingUnits.max`. `Exceed` (default) raises both ends
	// of the autoscaling range, so the instance may scale beyond max.
	// `Cap` raises only the lower bound and never exceeds max.
	// +kubebuilder:default=Exceed
	// +optional
	MaxPUPolicy MaxPUPolicy `json:"maxPUPolicy,omitempty"`

	// The details of when and for how long this schedule will be active.
	Schedule Schedule `json:"schedule"`
}

// SpannerAutoscaleScheduleStatus defines the observed state of SpannerAutoscaleSchedule
type SpannerAutoscaleScheduleStatus struct{}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:storageversion
// +kubebuilder:printcolumn:name="Cron",type="string",JSONPath=".spec.schedule.cron"
// +kubebuilder:printcolumn:name="Duration",type="string",JSONPath=".spec.schedule.duration"
// +kubebuilder:printcolumn:name="Additional PU",type="integer",JSONPath=".spec.additionalProcessingUnits"
// +kubebuilder:printcolumn:name="Max PU Policy",type="string",JSONPath=".spec.maxPUPolicy"
// SpannerAutoscaleSchedule is the Schema for the spannerautoscaleschedules API
type SpannerAutoscaleSchedule struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   SpannerAutoscaleScheduleSpec   `json:"spec,omitempty"`
	Status SpannerAutoscaleScheduleStatus `json:"status,omitempty"`
}

//+kubebuilder:object:root=true

// SpannerAutoscaleScheduleList contains a list of SpannerAutoscaleSchedule
type SpannerAutoscaleScheduleList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []SpannerAutoscaleSchedule `json:"items"`
}

func init() {
	SchemeBuilder.Register(&SpannerAutoscaleSchedule{}, &SpannerAutoscaleScheduleList{})
}
