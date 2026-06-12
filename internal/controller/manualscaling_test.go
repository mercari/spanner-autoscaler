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

package controller

import (
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	spannerv1beta1 "github.com/mercari/spanner-autoscaler/api/v1beta1"
)

// fakeNextPUInput bundles the SpannerAutoscaler / SpannerManualScaling
// fixture state that nextManualPU reads.
type fakeNextPUInput struct {
	currentPU      int
	lastScaleTime  time.Time
	target         int
	scaleupStep    *intstr.IntOrString
	scaleupIntv    *metav1.Duration
	scaledownStep  *intstr.IntOrString
	scaledownIntv  *metav1.Duration
	now            time.Time
	defScaleupIntv time.Duration
	defScaledownIv time.Duration
}

func nextPUFromFixture(in fakeNextPUInput) (int, time.Duration) {
	r := &SpannerAutoscalerReconciler{
		scaleUpInterval:   in.defScaleupIntv,
		scaleDownInterval: in.defScaledownIv,
	}
	sa := &spannerv1beta1.SpannerAutoscaler{
		Status: spannerv1beta1.SpannerAutoscalerStatus{
			CurrentProcessingUnits: in.currentPU,
			LastScaleTime:          metav1.Time{Time: in.lastScaleTime},
		},
	}
	ms := &spannerv1beta1.SpannerManualScaling{
		Spec: spannerv1beta1.SpannerManualScalingSpec{
			ProcessingUnits:   in.target,
			ScaleupStepSize:   in.scaleupStep,
			ScaleupInterval:   in.scaleupIntv,
			ScaledownStepSize: in.scaledownStep,
			ScaledownInterval: in.scaledownIntv,
		},
	}
	return r.nextManualPU(sa, ms, in.now)
}

func intstrPtr(v intstr.IntOrString) *intstr.IntOrString { return &v }
func durPtr(d time.Duration) *metav1.Duration            { return &metav1.Duration{Duration: d} }

// TestNextManualPU covers the gating rules around single-jump vs stepped
// vs cooldown-held in both directions, plus the interval fallback to the
// controller-wide flag default. The fallback case is the design's key
// invariant: scaleupStepSize set with scaleupInterval omitted must use the
// controller's --scale-up-interval, not zero.
func TestNextManualPU(t *testing.T) {
	now := time.Date(2026, 5, 30, 12, 0, 0, 0, time.UTC)
	last := now.Add(-10 * time.Minute) // far enough back that any sane interval has elapsed
	scaleupDefault := 60 * time.Second
	scaledownDefault := 55 * time.Minute

	cases := []struct {
		name        string
		in          fakeNextPUInput
		wantNextPU  int
		wantRequeue time.Duration
	}{
		{
			name: "no-op when current already equals target",
			in: fakeNextPUInput{
				currentPU: 5000, lastScaleTime: last, target: 5000,
				now: now, defScaleupIntv: scaleupDefault, defScaledownIv: scaledownDefault,
			},
			wantNextPU: 5000, wantRequeue: 0,
		},
		{
			name: "single-jump scale-up: scaleupStepSize unset",
			in: fakeNextPUInput{
				currentPU: 2000, lastScaleTime: last, target: 5000,
				now: now, defScaleupIntv: scaleupDefault, defScaledownIv: scaledownDefault,
			},
			wantNextPU: 5000, wantRequeue: 0,
		},
		{
			name: "single-jump scale-down: scaledownStepSize unset",
			in: fakeNextPUInput{
				currentPU: 8000, lastScaleTime: last, target: 3000,
				now: now, defScaleupIntv: scaleupDefault, defScaledownIv: scaledownDefault,
			},
			wantNextPU: 3000, wantRequeue: 0,
		},
		{
			name: "stepped scale-up with step+interval, cooldown elapsed",
			in: fakeNextPUInput{
				currentPU: 2000, lastScaleTime: last, target: 7000,
				scaleupStep: intstrPtr(intstr.FromInt(1000)),
				scaleupIntv: durPtr(5 * time.Minute),
				now:         now, defScaleupIntv: scaleupDefault, defScaledownIv: scaledownDefault,
			},
			wantNextPU: 3000, wantRequeue: 5 * time.Minute,
		},
		{
			name: "stepped scale-up: cooldown not yet elapsed",
			in: fakeNextPUInput{
				currentPU: 2000, lastScaleTime: now.Add(-30 * time.Second), target: 7000,
				scaleupStep: intstrPtr(intstr.FromInt(1000)),
				scaleupIntv: durPtr(5 * time.Minute),
				now:         now, defScaleupIntv: scaleupDefault, defScaledownIv: scaledownDefault,
			},
			// current returned unchanged, requeue at remaining cooldown
			wantNextPU: 2000, wantRequeue: 4*time.Minute + 30*time.Second,
		},
		{
			name: "stepped scale-up: step would overshoot, clamped to target",
			in: fakeNextPUInput{
				currentPU: 6500, lastScaleTime: last, target: 7000,
				scaleupStep: intstrPtr(intstr.FromInt(1000)),
				scaleupIntv: durPtr(5 * time.Minute),
				now:         now, defScaleupIntv: scaleupDefault, defScaledownIv: scaledownDefault,
			},
			wantNextPU: 7000, wantRequeue: 5 * time.Minute,
		},
		{
			name: "stepped scale-up: scaleupInterval unset falls back to controller default",
			in: fakeNextPUInput{
				currentPU: 2000, lastScaleTime: last, target: 7000,
				scaleupStep: intstrPtr(intstr.FromInt(1000)),
				// scaleupIntv intentionally nil
				now: now, defScaleupIntv: scaleupDefault, defScaledownIv: scaledownDefault,
			},
			wantNextPU: 3000, wantRequeue: scaleupDefault,
		},
		{
			name: "stepped scale-down with step+interval, cooldown elapsed",
			in: fakeNextPUInput{
				currentPU: 7000, lastScaleTime: last, target: 2000,
				scaledownStep: intstrPtr(intstr.FromInt(1000)),
				scaledownIntv: durPtr(10 * time.Minute),
				now:           now, defScaleupIntv: scaleupDefault, defScaledownIv: scaledownDefault,
			},
			wantNextPU: 6000, wantRequeue: 10 * time.Minute,
		},
		{
			// Demonstrates that the scaledown direction falls back to the
			// controller's --scale-down-interval (55m by default). Since the
			// shared `last` (now-10m) is less than that default, cooldown has
			// NOT elapsed, so the step is held until 45m later — the test
			// asserts the fallback duration is actually consulted.
			name: "stepped scale-down: scaledownInterval unset falls back to controller default (cooldown held)",
			in: fakeNextPUInput{
				currentPU: 7000, lastScaleTime: last, target: 2000,
				scaledownStep: intstrPtr(intstr.FromInt(1000)),
				// scaledownIntv intentionally nil
				now: now, defScaleupIntv: scaleupDefault, defScaledownIv: scaledownDefault,
			},
			wantNextPU: 7000, wantRequeue: scaledownDefault - 10*time.Minute,
		},
		{
			name: "stepped scale-down: scaledownInterval unset, lastScaleTime old enough that fallback elapsed",
			in: fakeNextPUInput{
				currentPU: 7000, lastScaleTime: now.Add(-2 * time.Hour), target: 2000,
				scaledownStep: intstrPtr(intstr.FromInt(1000)),
				// scaledownIntv intentionally nil
				now: now, defScaleupIntv: scaleupDefault, defScaledownIv: scaledownDefault,
			},
			wantNextPU: 6000, wantRequeue: scaledownDefault,
		},
		{
			name: "scale-down direction with scaleupStep set is single-jump (wrong-direction step ignored)",
			in: fakeNextPUInput{
				currentPU: 7000, lastScaleTime: last, target: 2000,
				scaleupStep: intstrPtr(intstr.FromInt(1000)),
				scaleupIntv: durPtr(5 * time.Minute),
				// scaledownStep / scaledownIntv intentionally nil → single-jump
				now: now, defScaleupIntv: scaleupDefault, defScaledownIv: scaledownDefault,
			},
			wantNextPU: 2000, wantRequeue: 0,
		},
		{
			name: "scale-up direction with scaledownStep set is single-jump (wrong-direction step ignored)",
			in: fakeNextPUInput{
				currentPU: 2000, lastScaleTime: last, target: 7000,
				scaledownStep: intstrPtr(intstr.FromInt(1000)),
				scaledownIntv: durPtr(10 * time.Minute),
				now:           now, defScaleupIntv: scaleupDefault, defScaledownIv: scaledownDefault,
			},
			wantNextPU: 7000, wantRequeue: 0,
		},
		{
			name: "stepped scale-up: lastScaleTime zero (fresh) applies step immediately",
			in: fakeNextPUInput{
				currentPU: 2000, lastScaleTime: time.Time{}, target: 7000,
				scaleupStep: intstrPtr(intstr.FromInt(1000)),
				scaleupIntv: durPtr(5 * time.Minute),
				now:         now, defScaleupIntv: scaleupDefault, defScaledownIv: scaledownDefault,
			},
			wantNextPU: 3000, wantRequeue: 5 * time.Minute,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotPU, gotRequeue := nextPUFromFixture(tc.in)
			if gotPU != tc.wantNextPU {
				t.Errorf("nextPU = %d, want %d", gotPU, tc.wantNextPU)
			}
			if gotRequeue != tc.wantRequeue {
				t.Errorf("requeueAfter = %s, want %s", gotRequeue, tc.wantRequeue)
			}
		})
	}
}

// TestManualScalingActiveRamp asserts the direction-aware ramp helper used
// for the scale_events_total `driver` label and ActiveManualScaling.Ramp.
// A naive "any step size set in the spec" rule would misattribute
// single-jump scale-ups as ramped when the spec also had a scaledown step
// size set; this helper resolves that by consulting only the direction
// implied by target vs current.
func TestManualScalingActiveRamp(t *testing.T) {
	stepUp := intstrPtr(intstr.FromInt(1000))
	stepDown := intstrPtr(intstr.FromInt(1000))

	cases := []struct {
		name      string
		spec      spannerv1beta1.SpannerManualScalingSpec
		currentPU int
		want      bool
	}{
		{
			name:      "scale-up direction, scaleupStepSize set → true (stepped)",
			spec:      spannerv1beta1.SpannerManualScalingSpec{ProcessingUnits: 7000, ScaleupStepSize: stepUp},
			currentPU: 3000,
			want:      true,
		},
		{
			name:      "scale-up direction, only scaledownStepSize set → false (single-jump; wrong-direction step is ignored)",
			spec:      spannerv1beta1.SpannerManualScalingSpec{ProcessingUnits: 7000, ScaledownStepSize: stepDown},
			currentPU: 3000,
			want:      false,
		},
		{
			name:      "scale-down direction, scaledownStepSize set → true",
			spec:      spannerv1beta1.SpannerManualScalingSpec{ProcessingUnits: 3000, ScaledownStepSize: stepDown},
			currentPU: 7000,
			want:      true,
		},
		{
			name:      "scale-down direction, only scaleupStepSize set → false",
			spec:      spannerv1beta1.SpannerManualScalingSpec{ProcessingUnits: 3000, ScaleupStepSize: stepUp},
			currentPU: 7000,
			want:      false,
		},
		{
			name:      "no scaling (target == current) → false",
			spec:      spannerv1beta1.SpannerManualScalingSpec{ProcessingUnits: 5000, ScaleupStepSize: stepUp, ScaledownStepSize: stepDown},
			currentPU: 5000,
			want:      false,
		},
		{
			name:      "scale-up direction with both step sizes set → true",
			spec:      spannerv1beta1.SpannerManualScalingSpec{ProcessingUnits: 7000, ScaleupStepSize: stepUp, ScaledownStepSize: stepDown},
			currentPU: 3000,
			want:      true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := manualScalingActiveRamp(&tc.spec, tc.currentPU); got != tc.want {
				t.Errorf("manualScalingActiveRamp(target=%d, current=%d) = %v, want %v",
					tc.spec.ProcessingUnits, tc.currentPU, got, tc.want)
			}
		})
	}
}

// TestActiveManualScalingEqual exercises the status-write-avoidance helper:
// nil/nil is equal; nil/non-nil isn't; otherwise field-by-field with
// ExpiresAt compared by absolute instant so timezone re-encoding does not
// produce spurious diffs (k8s normalizes metav1.Time to UTC on round-trip).
func TestActiveManualScalingEqual(t *testing.T) {
	t0 := time.Date(2026, 5, 30, 12, 0, 0, 0, time.UTC)
	t0JST := t0.In(time.FixedZone("JST", 9*3600))

	cases := []struct {
		name string
		a, b *spannerv1beta1.ActiveManualScaling
		want bool
	}{
		{
			name: "both nil",
			a:    nil,
			b:    nil,
			want: true,
		},
		{
			name: "nil vs non-nil",
			a:    nil,
			b:    &spannerv1beta1.ActiveManualScaling{Name: "x", ProcessingUnits: 5000},
			want: false,
		},
		{
			name: "identical",
			a:    &spannerv1beta1.ActiveManualScaling{Name: "x", ProcessingUnits: 5000, Ramp: true},
			b:    &spannerv1beta1.ActiveManualScaling{Name: "x", ProcessingUnits: 5000, Ramp: true},
			want: true,
		},
		{
			name: "name differs",
			a:    &spannerv1beta1.ActiveManualScaling{Name: "x"},
			b:    &spannerv1beta1.ActiveManualScaling{Name: "y"},
			want: false,
		},
		{
			name: "PU differs",
			a:    &spannerv1beta1.ActiveManualScaling{ProcessingUnits: 5000},
			b:    &spannerv1beta1.ActiveManualScaling{ProcessingUnits: 7000},
			want: false,
		},
		{
			name: "ramp differs",
			a:    &spannerv1beta1.ActiveManualScaling{Ramp: true},
			b:    &spannerv1beta1.ActiveManualScaling{Ramp: false},
			want: false,
		},
		{
			name: "expiresAt equal across timezones",
			a:    &spannerv1beta1.ActiveManualScaling{ExpiresAt: &metav1.Time{Time: t0}},
			b:    &spannerv1beta1.ActiveManualScaling{ExpiresAt: &metav1.Time{Time: t0JST}},
			want: true,
		},
		{
			name: "expiresAt one nil",
			a:    &spannerv1beta1.ActiveManualScaling{ExpiresAt: &metav1.Time{Time: t0}},
			b:    &spannerv1beta1.ActiveManualScaling{},
			want: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := activeManualScalingEqual(tc.a, tc.b); got != tc.want {
				t.Errorf("activeManualScalingEqual = %v, want %v", got, tc.want)
			}
		})
	}
}
