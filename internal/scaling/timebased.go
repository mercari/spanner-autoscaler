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

package scaling

import (
	"errors"
	"fmt"
	"time"

	spannerv1beta1 "github.com/mercari/spanner-autoscaler/api/v1beta1"
	"github.com/mercari/spanner-autoscaler/internal/cron"
)

// ResolvedScaleConfig is the scale config in effect at a given time.
type ResolvedScaleConfig struct {
	// ScaleConfig is the base scale config with the matching override applied.
	// TimeBasedOverrides is cleared so the result cannot be resolved twice.
	ScaleConfig spannerv1beta1.ScaleConfig
	// OverrideName is the name of the applied override, or "" when none matches.
	OverrideName string
	// ScaledownPolicy is the policy of the applied override, or "" when none matches.
	ScaledownPolicy spannerv1beta1.ScaledownPolicy
}

// ResolveScaleConfig applies the first entry of sc.TimeBasedOverrides whose
// times match now.
//
// On error the returned value is still usable and errs on the side of
// capacity: it carries the base scale config with scale down denied, because
// an override that cannot be evaluated may be the one that denies scale down.
func ResolveScaleConfig(sc spannerv1beta1.ScaleConfig, now time.Time) (ResolvedScaleConfig, error) {
	overrides := sc.TimeBasedOverrides
	base := sc
	base.TimeBasedOverrides = nil

	if len(overrides) == 0 {
		return ResolvedScaleConfig{ScaleConfig: base}, nil
	}

	failSafe := ResolvedScaleConfig{ScaleConfig: base, ScaledownPolicy: spannerv1beta1.ScaledownPolicyDeny}

	if len(sc.ScaledownAllowedTimes) > 0 || len(sc.ScaledownNotAllowedTimes) > 0 {
		return failSafe, errors.New("timeBasedOverrides cannot be used together with scaledownAllowedTimes or scaledownNotAllowedTimes")
	}

	for _, o := range overrides {
		matched, err := matchesAnyCron(o.Times, now)
		if err != nil {
			return failSafe, fmt.Errorf("timeBasedOverrides %q: %w", o.Name, err)
		}
		if !matched {
			continue
		}
		resolved, err := applyOverride(base, o)
		if err != nil {
			return failSafe, fmt.Errorf("timeBasedOverrides %q: %w", o.Name, err)
		}
		return ResolvedScaleConfig{ScaleConfig: resolved, OverrideName: o.Name, ScaledownPolicy: o.ScaledownPolicy}, nil
	}

	return ResolvedScaleConfig{ScaleConfig: base}, nil
}

func applyOverride(sc spannerv1beta1.ScaleConfig, o spannerv1beta1.TimeBasedOverride) (spannerv1beta1.ScaleConfig, error) {
	if pu := o.ProcessingUnits; pu != nil {
		if pu.Min != nil {
			sc.ProcessingUnits.Min = *pu.Min
		}
		if pu.Max != nil {
			sc.ProcessingUnits.Max = *pu.Max
		}
		if sc.ProcessingUnits.Min > sc.ProcessingUnits.Max {
			return sc, fmt.Errorf("processingUnits.min %d is greater than processingUnits.max %d", sc.ProcessingUnits.Min, sc.ProcessingUnits.Max)
		}
	}

	if t := o.TargetCPUUtilization; t != nil {
		// The syncer picks the metrics to fetch from the base config, so an
		// override must not change which metrics are active.
		if t.HighPriority != nil {
			if sc.TargetCPUUtilization.HighPriority == nil {
				return sc, errors.New("targetCPUUtilization.highPriority is not set in the scale config")
			}
			sc.TargetCPUUtilization.HighPriority = new(*t.HighPriority)
		}
		if t.Total != nil {
			if sc.TargetCPUUtilization.Total == nil {
				return sc, errors.New("targetCPUUtilization.total is not set in the scale config")
			}
			sc.TargetCPUUtilization.Total = new(*t.Total)
		}
	}

	if o.ScaledownStepSize != nil {
		sc.ScaledownStepSize = *o.ScaledownStepSize
	}
	if o.ScaledownInterval != nil {
		sc.ScaledownInterval = o.ScaledownInterval.DeepCopy()
	}
	if o.ScaleupStepSize != nil {
		sc.ScaleupStepSize = *o.ScaleupStepSize
	}
	if o.ScaleupInterval != nil {
		sc.ScaleupInterval = o.ScaleupInterval.DeepCopy()
	}

	return sc, nil
}

// matchesAnyCron is isTimeInCronSchedules without skipping invalid
// expressions: an override silently not matching would fall through to a
// later, possibly less restrictive, one.
func matchesAnyCron(exprs []string, now time.Time) (bool, error) {
	if len(exprs) == 0 {
		return false, errors.New("times is empty")
	}
	for _, expr := range exprs {
		schedule, err := cron.Parse(expr)
		if err != nil {
			return false, fmt.Errorf("invalid cron expression %q: %w", expr, err)
		}
		if schedule.Next(now.Add(-time.Minute)).Truncate(time.Minute).Equal(now.Truncate(time.Minute)) {
			return true, nil
		}
	}
	return false, nil
}

// WithScaleConfig returns a shallow copy of sa whose spec.scaleConfig is
// replaced by sc, for passing a resolved config to the functions in this
// package without touching the object read from the API server.
func WithScaleConfig(sa spannerv1beta1.SpannerAutoscaler, sc spannerv1beta1.ScaleConfig) spannerv1beta1.SpannerAutoscaler {
	sa.Spec.ScaleConfig = sc
	return sa
}
