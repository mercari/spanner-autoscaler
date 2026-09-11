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

// Package scaling holds the pure decision logic of the autoscaler: how many
// processing units an instance should have given the observed CPU metrics,
// and whether that change may be applied at a given time. The controller and
// the offline simulator (internal/simulator) share this package so that
// simulated replays use exactly the code paths that run in production.
//
// Every function here is deterministic: all inputs (spec, status, current
// time) are explicit and no I/O is performed.
package scaling

import (
	"fmt"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	spannerv1beta1 "github.com/mercari/spanner-autoscaler/api/v1beta1"
	"github.com/mercari/spanner-autoscaler/internal/cron"
)

// DesiredProcessingUnits calculates the values needed to keep CPU utilization
// below the configured targets, based on the metrics currently recorded in
// sa.Status.
func DesiredProcessingUnits(sa spannerv1beta1.SpannerAutoscaler) int {
	switch sa.Spec.ScaleConfig.TargetCPUUtilization.ActiveMetricFlags() {
	case spannerv1beta1.CPUMetricFlagHighPriority | spannerv1beta1.CPUMetricFlagTotal:
		// Dual mode: scale-out when either threshold is exceeded (OR condition).
		// Guard: wait until syncer has populated both metrics in status.
		if sa.Status.CurrentCPUMetricType != spannerv1beta1.CPUMetricTypeBoth {
			return sa.Status.CurrentProcessingUnits
		}
		desiredByHigh := DesiredPUFromCPU(
			sa.Status.CurrentHighPriorityCPUUtilization,
			*sa.Spec.ScaleConfig.TargetCPUUtilization.HighPriority,
			sa,
		)
		desiredByTotal := DesiredPUFromCPU(
			sa.Status.CurrentTotalCPUUtilization,
			*sa.Spec.ScaleConfig.TargetCPUUtilization.Total,
			sa,
		)
		return max(desiredByHigh, desiredByTotal)

	case spannerv1beta1.CPUMetricFlagTotal:
		// If the status was last synced for a different metric type, skip this reconcile.
		if sa.Status.CurrentCPUMetricType != spannerv1beta1.CPUMetricTypeTotal {
			return sa.Status.CurrentProcessingUnits
		}
		return DesiredPUFromCPU(
			sa.Status.CurrentTotalCPUUtilization,
			*sa.Spec.ScaleConfig.TargetCPUUtilization.Total,
			sa,
		)

	case spannerv1beta1.CPUMetricFlagHighPriority:
		// If the status was last synced for a different metric type, the CPU value
		// in status belongs to the old metric and cannot be used for scaling decisions.
		// Skip this reconcile; the syncer will update CurrentCPUMetricType within one
		// sync cycle (≤1 minute).
		if sa.Status.CurrentCPUMetricType != spannerv1beta1.CPUMetricTypeHighPriority {
			return sa.Status.CurrentProcessingUnits
		}
		return DesiredPUFromCPU(
			sa.Status.CurrentHighPriorityCPUUtilization,
			*sa.Spec.ScaleConfig.TargetCPUUtilization.HighPriority,
			sa,
		)

	default:
		// No metric configured — invalid spec that bypassed webhook validation,
		// or malformed objects already existing in-cluster. Hold current PU.
		return sa.Status.CurrentProcessingUnits
	}
}

// DesiredPUFromCPU calculates the desired PU from a single CPU metric,
// applying step size limits and min/max clamping. Used in dual CPU scaling mode
// to compute desired PU for each metric independently.
func DesiredPUFromCPU(currentCPU, targetCPU int, sa spannerv1beta1.SpannerAutoscaler) int {
	if targetCPU == 0 {
		return sa.Status.CurrentProcessingUnits
	}

	totalCPU := currentCPU * sa.Status.CurrentProcessingUnits
	requiredPU := totalCPU / targetCPU

	// https://cloud.google.com/spanner/docs/compute-capacity?hl=en
	// Valid values for processing units are:
	// If processingUnits < 1000, processing units must be multiples of 100.
	// If processingUnits >= 1000, processing units must be multiples of 1000.
	//
	// Round up the requiredPU value to make it valid.
	// If it is already a valid PU, increment to next unit to keep CPU usage below desired threshold.
	var desiredPU int
	if requiredPU < 1000 {
		desiredPU = ((requiredPU / 100) + 1) * 100
	} else {
		desiredPU = ((requiredPU / 1000) + 1) * 1000
	}

	// Step size resolution is shared with the SpannerManualScaling path via
	// ResolveStepSize (see stepsize.go). The helper preserves the legacy
	// rounding rules and the asymmetric "0 = no cap" semantics for scaleup;
	// the controller's stepsize_test.go differential test locks this
	// equivalence down.
	sdStepSize := ResolveStepSize(&sa.Spec.ScaleConfig.ScaledownStepSize, sa.Status.CurrentProcessingUnits, StepDirectionScaledown)
	suStepSize := ResolveStepSize(&sa.Spec.ScaleConfig.ScaleupStepSize, sa.Status.CurrentProcessingUnits, StepDirectionScaleup)

	// in case of scaling down, check that we don't scale down beyond the ScaledownStepSize
	if scaledDownPU := sa.Status.CurrentProcessingUnits - sdStepSize; desiredPU < scaledDownPU {
		desiredPU = scaledDownPU
	}
	// in case of scaling up, check that we don't scale up beyond the ScaleupStepSize
	if scaledUpPU := sa.Status.CurrentProcessingUnits + suStepSize; suStepSize != 0 && scaledUpPU < desiredPU {
		desiredPU = scaledUpPU
		if 1000 < desiredPU && desiredPU%1000 != 0 {
			desiredPU = ((desiredPU / 1000) + 1) * 1000
		}
	}

	// keep the scaling between the specified min/max range
	minPU := sa.Spec.ScaleConfig.ProcessingUnits.Min
	maxPU := sa.Spec.ScaleConfig.ProcessingUnits.Max
	// fetch min/max range from status, in case any schedules have updated the range
	if sa.Status.DesiredMinPUs > 0 {
		minPU = sa.Status.DesiredMinPUs
	}
	if sa.Status.DesiredMaxPUs > 0 {
		maxPU = sa.Status.DesiredMaxPUs
	}
	if desiredPU < minPU {
		desiredPU = minPU
	}
	if desiredPU > maxPU {
		desiredPU = maxPU
	}

	return desiredPU
}

// DesiredPURange computes the effective autoscaling range from the spec
// range and the currently active schedules. Every active schedule raises the
// lower bound by its AdditionalPU; whether it also raises the upper bound
// depends on its MaxPUPolicy: Exceed extends the max beyond
// spec.processingUnits.max, Cap keeps the spec max as the hard ceiling. An
// empty policy is treated as Exceed for backward compatibility with entries
// written by older controllers.
//
// The returned range always satisfies desiredMin <= desiredMax: the min is
// clamped down to the max after rounding (rounding first, so the 1000-unit
// round-up cannot push the min back above the max). capped reports whether
// this clamp trimmed part of the Cap schedules' contribution, so the caller
// can surface the reduction to operators.
func DesiredPURange(sa spannerv1beta1.SpannerAutoscaler) (minPU, maxPU int, changed, capped bool) {
	// Start from the spec range; active schedules only ever add to it.
	// A Cap schedule skips the max addition, so the max never drops below
	// spec.processingUnits.max.
	desiredMin := sa.Spec.ScaleConfig.ProcessingUnits.Min
	desiredMax := sa.Spec.ScaleConfig.ProcessingUnits.Max
	for _, sched := range sa.Status.CurrentlyActiveSchedules {
		desiredMin += sched.AdditionalPU
		if sched.MaxPUPolicy.Normalized() == spannerv1beta1.MaxPUPolicyExceed {
			desiredMax += sched.AdditionalPU
		}
	}

	// round up, in case any schedule adds small number of PUs
	if remainder := desiredMin % 1000; desiredMin > 1000 && remainder != 0 {
		desiredMin = ((desiredMin / 1000) + 1) * 1000
	}

	if remainder := desiredMax % 1000; desiredMax > 1000 && remainder != 0 {
		desiredMax = ((desiredMax / 1000) + 1) * 1000
	}

	// Cap schedules do not extend the max, so the summed min can exceed it.
	// Never publish an inverted range: clamp the min down to the max.
	if desiredMin > desiredMax {
		desiredMin = desiredMax
		capped = true
	}

	if desiredMin != sa.Status.DesiredMinPUs || desiredMax != sa.Status.DesiredMaxPUs {
		changed = true
	}

	return desiredMin, desiredMax, changed, capped
}

// Decision is the outcome of Decide: either the desired PU should be applied
// now, or the change is skipped for one of the reasons below. The skip
// reasons mirror the controller's scale-skipped observability labels.
type Decision int

const (
	// DecisionScale means the desired PU should be applied now.
	DecisionScale Decision = iota
	// DecisionSkipSame means desired == current; nothing to do.
	DecisionSkipSame
	// DecisionSkipScaleUpInterval means a scale-up is wanted but the
	// scale-up cooldown since the last scale event has not elapsed.
	DecisionSkipScaleUpInterval
	// DecisionSkipScaleDownInterval means a scale-down is wanted but the
	// scale-down cooldown since the last scale event has not elapsed.
	DecisionSkipScaleDownInterval
	// DecisionSkipScaleDownWindow means a scale-down is wanted but the
	// current time is outside the allowed scale-down windows.
	DecisionSkipScaleDownWindow
)

// Decide reports whether desiredPU may be applied at now, reproducing the
// guard conditions of the controller's needUpdateProcessingUnits: equal-value
// short circuit, per-direction cooldown intervals since
// sa.Status.LastScaleTime, and the scale-down time-window restrictions.
// defaultScaleUpInterval / defaultScaleDownInterval are the controller-level
// defaults used when the spec does not override them.
//
// A non-nil error indicates an invalid time-restriction configuration; the
// returned Decision is DecisionSkipScaleDownWindow in that case (the change
// must not be applied).
func Decide(sa *spannerv1beta1.SpannerAutoscaler, desiredPU int, now time.Time, defaultScaleUpInterval, defaultScaleDownInterval time.Duration) (Decision, error) {
	currentPU := sa.Status.CurrentProcessingUnits

	switch {
	case desiredPU == currentPU:
		return DecisionSkipSame, nil

	case currentPU < desiredPU && now.Before(sa.Status.LastScaleTime.Time.Add(DurationOr(sa.Spec.ScaleConfig.ScaleupInterval, defaultScaleUpInterval))):
		return DecisionSkipScaleUpInterval, nil

	case desiredPU < currentPU && now.Before(sa.Status.LastScaleTime.Time.Add(DurationOr(sa.Spec.ScaleConfig.ScaledownInterval, defaultScaleDownInterval))):
		return DecisionSkipScaleDownInterval, nil

	case desiredPU < currentPU:
		allowed, err := IsScaledownAllowed(sa.Spec.ScaleConfig.ScaledownAllowedTimes, sa.Spec.ScaleConfig.ScaledownNotAllowedTimes, now)
		if err != nil {
			return DecisionSkipScaleDownWindow, err
		}
		if !allowed {
			return DecisionSkipScaleDownWindow, nil
		}
	}

	return DecisionScale, nil
}

// DurationOr returns customDuration when set, defaultDuration otherwise.
func DurationOr(customDuration *metav1.Duration, defaultDuration time.Duration) time.Duration {
	if customDuration != nil {
		return customDuration.Duration
	}

	return defaultDuration
}

// IsScaledownAllowed checks if scale down is allowed at the current time based on configured time restrictions.
// Returns true if scale down is allowed, false otherwise, and an error for invalid configurations.
// Supports both scaledownAllowedTimes (allowlist) and scaledownNotAllowedTimes (blocklist) patterns.
func IsScaledownAllowed(allowedTimes []string, notAllowedTimes []string, currentTime time.Time) (bool, error) {
	// Both allowedTimes and notAllowedTimes cannot be specified together
	if len(allowedTimes) > 0 && len(notAllowedTimes) > 0 {
		return false, fmt.Errorf("scaledownAllowedTimes and scaledownNotAllowedTimes cannot be specified together")
	}

	// If scaledownAllowedTimes is specified, check if current time matches any allowed period
	if len(allowedTimes) > 0 {
		return isTimeInCronSchedules(allowedTimes, currentTime), nil
	}

	// If scaledownNotAllowedTimes is specified, check if current time matches any forbidden period
	if len(notAllowedTimes) > 0 {
		return !isTimeInCronSchedules(notAllowedTimes, currentTime), nil
	}

	// If no time restrictions are specified, allow scale down anytime
	return true, nil
}

// isTimeInCronSchedules checks if the current time matches any of the provided cron schedules.
func isTimeInCronSchedules(cronSchedules []string, currentTime time.Time) bool {
	for _, cronExpr := range cronSchedules {
		schedule, err := cron.Parse(cronExpr)
		if err != nil {
			// If cron expression is invalid, continue checking other expressions
			continue
		}

		// Get the next scheduled time from the current time
		nextTime := schedule.Next(currentTime.Add(-time.Minute))

		// If the next scheduled time is within the current minute, then we're in a matching period
		if nextTime.Truncate(time.Minute).Equal(currentTime.Truncate(time.Minute)) {
			return true
		}
	}

	return false
}
