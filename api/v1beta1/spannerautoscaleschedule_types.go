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
	// The recurring frequency of the schedule in [standard cron](https://en.wikipedia.org/wiki/Cron) format. Examples and verification utility: https://crontab.guru
	Cron string `json:"cron"`

	// The length of time for which this schedule will remain active each time the cron is triggered.
	Duration string `json:"duration"`
}

// SpannerAutoscaleScheduleSpec defines the desired state of SpannerAutoscaleSchedule
type SpannerAutoscaleScheduleSpec struct {
	// The `SpannerAutoscaler` resource name with which this schedule will be registered
	TargetResource string `json:"targetResource"`

	// The extra compute capacity which will be added when this schedule is active
	AdditionalProcessingUnits int `json:"additionalProcessingUnits"`

	// The details of when and for how long this schedule will be active
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
