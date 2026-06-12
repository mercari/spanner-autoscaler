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

package v1beta1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

// SpannerManualScalingSpec defines a one-shot operation that drives the parent
// SpannerAutoscaler's processing units toward a target value, bypassing
// CPU-based autoscaling, SpannerAutoscaleSchedule additions, and the
// scaledownAllowedTimes / scaledownNotAllowedTimes windows for as long as it
// is active.
//
// The target may be above or below the current processing units. The
// controller picks the direction from the sign of
// (ProcessingUnits - CurrentProcessingUnits) and reads only the step-size /
// interval fields for that direction. Pacing is inferred from the presence
// of the step-size field:
//
//   - target > current, ScaleupStepSize unset      → single-jump scale-up.
//   - target > current, ScaleupStepSize set        → stepped scale-up ramp;
//     the cadence comes from ScaleupInterval when set, otherwise from the
//     controller's --scale-up-interval flag default.
//   - target < current, ScaledownStepSize unset    → single-jump scale-down.
//   - target < current, ScaledownStepSize set      → stepped scale-down ramp;
//     the cadence comes from ScaledownInterval when set, otherwise from the
//     controller's --scale-down-interval flag default.
//
// Manual scale-down is accepted by default. Cluster operators who want to
// forbid it can run the controller with --reject-manual-scaledown=true; in
// that mode an override whose target is below the current PU lands in the
// Invalid phase rather than scaling the instance down.
//
// Interval-only specification (e.g. ScaleupInterval set but ScaleupStepSize
// unset) has no effect; the validating webhook emits an admission warning to
// flag the likely typo.
//
// Spec is IMMUTABLE after creation: the validating webhook rejects any spec
// mutation. To change the target PU, ramp parameters, or extend ExpiresAt,
// create a new SpannerManualScaling — the newest-creationTimestamp rule
// atomically supersedes the older resource with no autoscaling gap. To
// shorten / force expiration, kubectl delete the resource.
type SpannerManualScalingSpec struct {
	// The `SpannerAutoscaler` resource name (in the same namespace) this
	// override applies to. Immutable after creation.
	TargetResource string `json:"targetResource"`

	// ProcessingUnits is the target value that this override drives toward
	// while active. May be above or below the autoscaler's current PU; the
	// controller derives the scaling direction from the sign of
	// (ProcessingUnits - CurrentProcessingUnits) and consults the matching
	// step-size / interval fields below. Must be a multiple of 100 (for
	// values < 1000) or a multiple of 1000.
	// +kubebuilder:validation:Minimum=100
	ProcessingUnits int `json:"processingUnits"`

	// ScaleupStepSize, when set, activates stepped scale-up: each reconcile
	// applies at most this delta until ProcessingUnits is reached. Accepts the
	// same int-or-percent format as the parent SpannerAutoscaler's
	// scaleConfig.scaleupStepSize. Unset means single-jump scale-up — the
	// override jumps the full upward delta in one reconcile.
	//
	// Presence of this field (when target > current) is the sole signal that
	// gates stepped vs single-jump scale-up.
	// +optional
	ScaleupStepSize *intstr.IntOrString `json:"scaleupStepSize,omitempty"`

	// ScaleupInterval is the minimum wall-clock time the controller waits
	// between successive upward steps. Only consulted when ScaleupStepSize is
	// set. Unset falls back to the controller's --scale-up-interval flag
	// value.
	//
	// Setting ScaleupInterval without ScaleupStepSize has no effect; the
	// validating webhook emits an admission warning for that combination.
	// +optional
	ScaleupInterval *metav1.Duration `json:"scaleupInterval,omitempty"`

	// ScaledownStepSize mirrors ScaleupStepSize for the downward direction
	// (ProcessingUnits < CurrentProcessingUnits).
	// +optional
	ScaledownStepSize *intstr.IntOrString `json:"scaledownStepSize,omitempty"`

	// ScaledownInterval mirrors ScaleupInterval for the downward direction.
	// Unset falls back to the controller's --scale-down-interval flag value.
	// +optional
	ScaledownInterval *metav1.Duration `json:"scaledownInterval,omitempty"`

	// ExpiresAt is the time after which this override is automatically
	// deactivated. When omitted, the override remains active until the
	// resource is deleted.
	//
	// Accepts any RFC 3339 timestamp with a timezone designator (Z, +09:00,
	// -08:00, etc.). Per Kubernetes convention (metav1.Time), the value is
	// normalized to UTC for storage in etcd and for kubectl get/describe
	// output; the absolute instant in time is preserved.
	//
	// When a step size is set, the controller does not guarantee that
	// ProcessingUnits is reached before ExpiresAt — it is the user's
	// responsibility to size ExpiresAt against ScaleupStepSize /
	// ScaleupInterval (or the scaledown counterparts) so the ramp fits in the
	// window. If ExpiresAt elapses mid-ramp, the override is deactivated
	// wherever CurrentProcessingUnits happens to be and normal autoscaling
	// resumes from there.
	// +optional
	ExpiresAt *metav1.Time `json:"expiresAt,omitempty"`
}

// SpannerManualScalingPhase indicates the lifecycle stage of a manual
// override.
type SpannerManualScalingPhase string

const (
	// SpannerManualScalingPhasePending: created but not yet picked up by the
	// controller. Transient (typically resolved on the first reconcile).
	SpannerManualScalingPhasePending SpannerManualScalingPhase = "Pending"

	// SpannerManualScalingPhaseProgressing: the step-size field for the
	// required direction is set and CurrentProcessingUnits has not yet
	// reached spec.processingUnits. The controller is taking step-bounded,
	// interval-gated updates toward the target. A single-jump override
	// (step size unset) skips this phase and goes Pending -> Active.
	SpannerManualScalingPhaseProgressing SpannerManualScalingPhase = "Progressing"

	// SpannerManualScalingPhaseActive: CurrentProcessingUnits equals
	// spec.processingUnits and the override is holding the target on the
	// parent SpannerAutoscaler.
	SpannerManualScalingPhaseActive SpannerManualScalingPhase = "Active"

	// SpannerManualScalingPhaseExpired: ExpiresAt has elapsed; no longer
	// pinning PU. With a step size set, this can occur mid-ramp; in that
	// case the target may not have been reached.
	SpannerManualScalingPhaseExpired SpannerManualScalingPhase = "Expired"

	// SpannerManualScalingPhaseSuperseded: a newer SpannerManualScaling for
	// the same targetResource was created and is now active.
	SpannerManualScalingPhaseSuperseded SpannerManualScalingPhase = "Superseded"

	// SpannerManualScalingPhaseInvalid: the targetResource does not exist
	// (in the same namespace) or the override otherwise cannot be applied.
	// Terminal: the resource is retained for operator inspection until the
	// history-limit GC reclaims it. To retry after fixing the cause, create
	// a new SpannerManualScaling — the controller will not transition this
	// resource back to a non-terminal phase.
	SpannerManualScalingPhaseInvalid SpannerManualScalingPhase = "Invalid"
)

// IsTerminal reports whether the phase is one of Expired, Superseded, or
// Invalid — the three states from which the controller never transitions
// back. Used by the active-candidate selector (to skip done overrides) and
// by the history-limit GC (to identify rows eligible for deletion).
func (p SpannerManualScalingPhase) IsTerminal() bool {
	switch p {
	case SpannerManualScalingPhaseExpired,
		SpannerManualScalingPhaseSuperseded,
		SpannerManualScalingPhaseInvalid:
		return true
	}
	return false
}

// SpannerManualScalingStatus defines the observed state of
// SpannerManualScaling.
type SpannerManualScalingStatus struct {
	// Phase reflects the lifecycle stage of this override.
	// +optional
	Phase SpannerManualScalingPhase `json:"phase,omitempty"`

	// AppliedAt is the time the controller first applied this override to
	// the target.
	// +optional
	AppliedAt *metav1.Time `json:"appliedAt,omitempty"`

	// ReachedAt is the time CurrentProcessingUnits first matched
	// spec.processingUnits while this override was active. Set only when the
	// target has been reached; remains nil for stepped ramps still in
	// progress.
	// +optional
	ReachedAt *metav1.Time `json:"reachedAt,omitempty"`

	// CurrentProcessingUnits is the parent SpannerAutoscaler's PU as last
	// observed by this controller. Useful for stepped ramps to see progress
	// toward spec.processingUnits without a cross-resource lookup.
	// +optional
	CurrentProcessingUnits int `json:"currentProcessingUnits,omitempty"`

	// FinishedAt is the time this resource first transitioned to a terminal
	// phase (Expired, Superseded, or Invalid). Used as the sort key by the
	// controller's history-limit GC. Set once and not updated thereafter
	// (terminal phases do not transition back).
	// +optional
	FinishedAt *metav1.Time `json:"finishedAt,omitempty"`

	// Message is a human-readable explanation, especially when Phase is
	// Invalid or when Expired occurred mid-ramp (target not reached).
	// +optional
	Message string `json:"message,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:storageversion
// +kubebuilder:printcolumn:name="Target",type="string",JSONPath=".spec.targetResource"
// +kubebuilder:printcolumn:name="Target PU",type="integer",JSONPath=".spec.processingUnits"
// +kubebuilder:printcolumn:name="Current PU",type="integer",JSONPath=".status.currentProcessingUnits"
// +kubebuilder:printcolumn:name="Expires At",type="date",JSONPath=".spec.expiresAt"
// +kubebuilder:printcolumn:name="Phase",type="string",JSONPath=".status.phase"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

// SpannerManualScaling is the Schema for the spannermanualscalings API.
type SpannerManualScaling struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   SpannerManualScalingSpec   `json:"spec,omitempty"`
	Status SpannerManualScalingStatus `json:"status,omitempty"`
}

//+kubebuilder:object:root=true

// SpannerManualScalingList contains a list of SpannerManualScaling.
type SpannerManualScalingList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []SpannerManualScaling `json:"items"`
}

func init() {
	SchemeBuilder.Register(&SpannerManualScaling{}, &SpannerManualScalingList{})
}
