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
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/util/intstr"

	spannerv1beta1 "github.com/mercari/spanner-autoscaler/api/v1beta1"
)

// celTestResource returns a minimal valid autoscaler with dual CPU targets,
// for exercising validateCELScaleConfig directly (no envtest needed).
func celTestResource() *spannerv1beta1.SpannerAutoscaler {
	return &spannerv1beta1.SpannerAutoscaler{
		Spec: spannerv1beta1.SpannerAutoscalerSpec{
			ScaleConfig: spannerv1beta1.ScaleConfig{
				ProcessingUnits: spannerv1beta1.ScaleConfigPUs{Min: 1000, Max: 10000},
				TargetCPUUtilization: spannerv1beta1.TargetCPUUtilization{
					HighPriority: new(60),
					Total:        new(80),
				},
			},
		},
	}
}

func TestValidateCELScaleConfig(t *testing.T) {
	tests := []struct {
		name        string
		mutate      func(sc *spannerv1beta1.ScaleConfig)
		wantErrPart string // empty means no error expected
	}{
		{
			name: "valid windows rules and conditions",
			mutate: func(sc *spannerv1beta1.ScaleConfig) {
				sc.MetricWindows = []string{"15m", "1h"}
				sc.ScalingRules = []spannerv1beta1.ScalingRule{
					{When: "cpu.highPriority.min15m >= 50", ScaleUp: intstr.FromString("25%")},
					{When: "cpu.total.avg1h > target.total", ScaleUp: intstr.FromInt(3000)},
				}
				sc.ScaleupCondition = "cpu.highPriority.min15m >= 45"
				sc.ScaledownCondition = "cpu.total.max1h < 20"
			},
		},
		{
			name: "invalid window spelling",
			mutate: func(sc *spannerv1beta1.ScaleConfig) {
				sc.MetricWindows = []string{"1h30m"}
			},
			wantErrPart: "whole number of minutes or hours",
		},
		{
			name: "window above one hour",
			mutate: func(sc *spannerv1beta1.ScaleConfig) {
				sc.MetricWindows = []string{"2h"}
			},
			wantErrPart: "exceeds the maximum",
		},
		{
			name: "duplicate window",
			mutate: func(sc *spannerv1beta1.ScaleConfig) {
				sc.MetricWindows = []string{"15m", "15m"}
			},
			wantErrPart: "Duplicate",
		},
		{
			name: "too many windows",
			mutate: func(sc *spannerv1beta1.ScaleConfig) {
				sc.MetricWindows = []string{"5m", "10m", "15m", "30m", "1h"}
			},
			wantErrPart: "must have at most 4 items",
		},
		{
			name: "rule references undeclared window",
			mutate: func(sc *spannerv1beta1.ScaleConfig) {
				sc.MetricWindows = []string{"15m"}
				sc.ScalingRules = []spannerv1beta1.ScalingRule{
					{When: "cpu.highPriority.min30m >= 50", ScaleUp: intstr.FromString("25%")},
				}
			},
			wantErrPart: "does not support field selection",
		},
		{
			name: "rule references metric without target",
			mutate: func(sc *spannerv1beta1.ScaleConfig) {
				sc.TargetCPUUtilization.Total = nil
				sc.MetricWindows = []string{"15m"}
				sc.ScalingRules = []spannerv1beta1.ScalingRule{
					{When: "cpu.total.min15m >= 50", ScaleUp: intstr.FromString("25%")},
				}
			},
			wantErrPart: "undeclared reference",
		},
		{
			name: "rule must return bool",
			mutate: func(sc *spannerv1beta1.ScaleConfig) {
				sc.ScalingRules = []spannerv1beta1.ScalingRule{
					{When: "current + 100", ScaleUp: intstr.FromString("25%")},
				}
			},
			wantErrPart: "must evaluate to a boolean",
		},
		{
			name: "empty when rejected",
			mutate: func(sc *spannerv1beta1.ScaleConfig) {
				sc.ScalingRules = []spannerv1beta1.ScalingRule{
					{ScaleUp: intstr.FromString("25%")},
				}
			},
			wantErrPart: "Required value",
		},
		{
			name: "scaleUp percent above 100 rejected",
			mutate: func(sc *spannerv1beta1.ScaleConfig) {
				sc.ScalingRules = []spannerv1beta1.ScalingRule{
					{When: "current > 0", ScaleUp: intstr.FromString("150%")},
				}
			},
			wantErrPart: "between 1% and 100%",
		},
		{
			name: "scaleUp zero rejected",
			mutate: func(sc *spannerv1beta1.ScaleConfig) {
				sc.ScalingRules = []spannerv1beta1.ScalingRule{
					{When: "current > 0", ScaleUp: intstr.FromInt(0)},
				}
			},
			wantErrPart: "must be positive",
		},
		{
			name: "scaleUp off-grid value rejected",
			mutate: func(sc *spannerv1beta1.ScaleConfig) {
				sc.ScalingRules = []spannerv1beta1.ScalingRule{
					{When: "current > 0", ScaleUp: intstr.FromInt(1500)},
				}
			},
			wantErrPart: "multiple of 1000",
		},
		{
			name: "invalid scaledownCondition",
			mutate: func(sc *spannerv1beta1.ScaleConfig) {
				sc.ScaledownCondition = "cpu.total.max30m < 20"
			},
			wantErrPart: "does not support field selection",
		},
		{
			name: "conditions without windows can use latest values",
			mutate: func(sc *spannerv1beta1.ScaleConfig) {
				sc.ScaledownCondition = `cpu.total < 20 && now.getHours("Asia/Tokyo") >= 13`
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := celTestResource()
			tt.mutate(&r.Spec.ScaleConfig)

			errs := validateCELScaleConfig(r)
			if tt.wantErrPart == "" {
				if len(errs) != 0 {
					t.Fatalf("validateCELScaleConfig() = %v, want no errors", errs)
				}
				return
			}
			if len(errs) == 0 {
				t.Fatalf("validateCELScaleConfig() = nil, want error containing %q", tt.wantErrPart)
			}
			if !strings.Contains(errs.ToAggregate().Error(), tt.wantErrPart) {
				t.Errorf("validateCELScaleConfig() = %v, want error containing %q", errs, tt.wantErrPart)
			}
		})
	}
}
