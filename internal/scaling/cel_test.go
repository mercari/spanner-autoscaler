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
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	spannerv1beta1 "github.com/mercari/spanner-autoscaler/api/v1beta1"
)

// celTestAutoscaler returns a dual-metric autoscaler with a 15m window whose
// aggregates are populated, currently at 3000 PU with CPU 55%/60%.
func celTestAutoscaler() *spannerv1beta1.SpannerAutoscaler {
	return &spannerv1beta1.SpannerAutoscaler{
		Spec: spannerv1beta1.SpannerAutoscalerSpec{
			ScaleConfig: spannerv1beta1.ScaleConfig{
				ProcessingUnits: spannerv1beta1.ScaleConfigPUs{Min: 1000, Max: 10000},
				TargetCPUUtilization: spannerv1beta1.TargetCPUUtilization{
					HighPriority: new(60),
					Total:        new(80),
				},
				MetricWindows: []string{"15m"},
			},
		},
		Status: spannerv1beta1.SpannerAutoscalerStatus{
			CurrentProcessingUnits:            3000,
			CurrentHighPriorityCPUUtilization: 55,
			CurrentTotalCPUUtilization:        60,
			CurrentCPUMetricType:              spannerv1beta1.CPUMetricTypeBoth,
			CurrentCPUWindowMetrics: []spannerv1beta1.CPUWindowMetric{
				{Metric: spannerv1beta1.CPUMetricTypeHighPriority, Window: "15m", Min: 52, Avg: 55, Max: 58},
				{Metric: spannerv1beta1.CPUMetricTypeTotal, Window: "15m", Min: 57, Avg: 60, Max: 65},
			},
		},
	}
}

func TestCompileCondition(t *testing.T) {
	flags := spannerv1beta1.CPUMetricFlagHighPriority | spannerv1beta1.CPUMetricFlagTotal

	tests := []struct {
		name    string
		flags   spannerv1beta1.CPUMetricFlags
		windows []string
		expr    string
		wantErr bool
	}{
		{
			name:    "window aggregate comparison",
			flags:   flags,
			windows: []string{"15m"},
			expr:    "cpu.highPriority.min15m >= 50",
		},
		{
			name:    "double literal against int variable",
			flags:   flags,
			windows: []string{"15m"},
			expr:    "cpu.total.avg15m > 32.5",
		},
		{
			name:  "timestamp functions and schedules",
			flags: flags,
			expr:  `now.getHours("Asia/Tokyo") >= 13 && !("boost" in activeSchedules)`,
		},
		{
			name:  "current desired and targets",
			flags: flags,
			expr:  "desired > current && cpu.highPriority > target.highPriority",
		},
		{
			name:    "undeclared window rejected",
			flags:   flags,
			windows: []string{"15m"},
			expr:    "cpu.highPriority.min30m >= 50",
			wantErr: true,
		},
		{
			name:    "inactive metric rejected",
			flags:   spannerv1beta1.CPUMetricFlagHighPriority,
			windows: []string{"15m"},
			expr:    "cpu.total.min15m >= 50",
			wantErr: true,
		},
		{
			name:    "non-bool result rejected",
			flags:   flags,
			expr:    "current + 100",
			wantErr: true,
		},
		{
			name:    "syntax error rejected",
			flags:   flags,
			expr:    "cpu.highPriority >=",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := CompileCondition(tt.flags, tt.windows, tt.expr)
			if (err != nil) != tt.wantErr {
				t.Fatalf("CompileCondition(%q) error = %v, wantErr %v", tt.expr, err, tt.wantErr)
			}
		})
	}
}

func TestEvaluateScalingRules(t *testing.T) {
	now := time.Date(2026, 9, 16, 10, 0, 0, 0, time.UTC)

	tests := []struct {
		name        string
		mutate      func(sa *spannerv1beta1.SpannerAutoscaler)
		builtin     int
		wantDesired int
		wantErrIs   error
	}{
		{
			name: "triggered percent scaleUp beats builtin",
			mutate: func(sa *spannerv1beta1.SpannerAutoscaler) {
				sa.Spec.ScaleConfig.ScalingRules = []spannerv1beta1.ScalingRule{
					{When: "cpu.highPriority.min15m >= 50", ScaleUp: intstr.FromString("25%")},
				}
			},
			builtin: 3000,
			// 3000 + 25% = 3750, rounded up to 4000.
			wantDesired: 4000,
		},
		{
			name: "triggered fixed scaleUp",
			mutate: func(sa *spannerv1beta1.SpannerAutoscaler) {
				sa.Spec.ScaleConfig.ScalingRules = []spannerv1beta1.ScalingRule{
					{When: "cpu.highPriority.min15m >= 50", ScaleUp: intstr.FromInt(3000)},
				}
			},
			builtin:     3000,
			wantDesired: 6000,
		},
		{
			name: "not triggered keeps builtin",
			mutate: func(sa *spannerv1beta1.SpannerAutoscaler) {
				sa.Spec.ScaleConfig.ScalingRules = []spannerv1beta1.ScalingRule{
					{When: "cpu.highPriority.min15m >= 90", ScaleUp: intstr.FromString("25%")},
				}
			},
			builtin:     2000,
			wantDesired: 2000,
		},
		{
			name: "builtin larger than candidate wins",
			mutate: func(sa *spannerv1beta1.SpannerAutoscaler) {
				sa.Spec.ScaleConfig.ScalingRules = []spannerv1beta1.ScalingRule{
					{When: "cpu.highPriority.min15m >= 50", ScaleUp: intstr.FromInt(1000)},
				}
			},
			builtin:     8000,
			wantDesired: 8000,
		},
		{
			name: "candidate clamped to maxPU",
			mutate: func(sa *spannerv1beta1.SpannerAutoscaler) {
				sa.Spec.ScaleConfig.ScalingRules = []spannerv1beta1.ScalingRule{
					{When: "cpu.highPriority.min15m >= 50", ScaleUp: intstr.FromString("100%")},
				}
				sa.Spec.ScaleConfig.ProcessingUnits.Max = 5000
			},
			builtin:     3000,
			wantDesired: 5000,
		},
		{
			name: "missing window data skips rule fail-safe",
			mutate: func(sa *spannerv1beta1.SpannerAutoscaler) {
				sa.Spec.ScaleConfig.ScalingRules = []spannerv1beta1.ScalingRule{
					{When: "cpu.highPriority.min15m >= 50", ScaleUp: intstr.FromString("25%")},
				}
				sa.Status.CurrentCPUWindowMetrics = nil
			},
			builtin:     3000,
			wantDesired: 3000,
			wantErrIs:   ErrWindowDataNotReady,
		},
		{
			name: "no rules returns builtin without outcomes",
			mutate: func(sa *spannerv1beta1.SpannerAutoscaler) {
				sa.Spec.ScaleConfig.ScalingRules = nil
			},
			builtin:     2000,
			wantDesired: 2000,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sa := celTestAutoscaler()
			tt.mutate(sa)

			desired, outcomes := EvaluateScalingRules(sa, tt.builtin, now)
			if desired != tt.wantDesired {
				t.Errorf("EvaluateScalingRules() desired = %d, want %d", desired, tt.wantDesired)
			}
			if tt.wantErrIs != nil {
				if len(outcomes) == 0 || !errors.Is(outcomes[0].Err, tt.wantErrIs) {
					t.Errorf("EvaluateScalingRules() outcomes = %+v, want error %v", outcomes, tt.wantErrIs)
				}
			} else {
				for _, oc := range outcomes {
					if oc.Err != nil {
						t.Errorf("EvaluateScalingRules() unexpected rule error: %v", oc.Err)
					}
				}
			}
			if len(sa.Spec.ScaleConfig.ScalingRules) == 0 && outcomes != nil {
				t.Errorf("EvaluateScalingRules() outcomes = %+v, want nil without rules", outcomes)
			}
		})
	}
}

func TestDecide_Gates(t *testing.T) {
	now := time.Date(2026, 9, 16, 10, 0, 0, 0, time.UTC)
	// Last scale far in the past so cooldown intervals never interfere.
	lastScale := metav1.Time{Time: now.Add(-24 * time.Hour)}

	tests := []struct {
		name         string
		mutate       func(sa *spannerv1beta1.SpannerAutoscaler)
		desiredPU    int
		wantDecision Decision
		wantGateErr  bool
	}{
		{
			name: "scaledown gate allows",
			mutate: func(sa *spannerv1beta1.SpannerAutoscaler) {
				sa.Spec.ScaleConfig.ScaledownCondition = "cpu.total.max15m < 70"
			},
			desiredPU:    2000,
			wantDecision: DecisionScale,
		},
		{
			name: "scaledown gate denies",
			mutate: func(sa *spannerv1beta1.SpannerAutoscaler) {
				sa.Spec.ScaleConfig.ScaledownCondition = "cpu.total.max15m < 20"
			},
			desiredPU:    2000,
			wantDecision: DecisionSkipScaleDownGate,
		},
		{
			name: "scaledown gate fails closed on missing window data",
			mutate: func(sa *spannerv1beta1.SpannerAutoscaler) {
				sa.Spec.ScaleConfig.ScaledownCondition = "cpu.total.max15m < 20"
				sa.Status.CurrentCPUWindowMetrics = nil
			},
			desiredPU:    2000,
			wantDecision: DecisionSkipScaleDownGate,
			wantGateErr:  true,
		},
		{
			name: "scaleup gate allows",
			mutate: func(sa *spannerv1beta1.SpannerAutoscaler) {
				sa.Spec.ScaleConfig.ScaleupCondition = "cpu.highPriority.min15m >= 50"
			},
			desiredPU:    4000,
			wantDecision: DecisionScale,
		},
		{
			name: "scaleup gate denies",
			mutate: func(sa *spannerv1beta1.SpannerAutoscaler) {
				sa.Spec.ScaleConfig.ScaleupCondition = "cpu.highPriority.min15m >= 90"
			},
			desiredPU:    4000,
			wantDecision: DecisionSkipScaleUpGate,
		},
		{
			name: "scaleup gate fails open on missing window data",
			mutate: func(sa *spannerv1beta1.SpannerAutoscaler) {
				sa.Spec.ScaleConfig.ScaleupCondition = "cpu.highPriority.min15m >= 90"
				sa.Status.CurrentCPUWindowMetrics = nil
			},
			desiredPU:    4000,
			wantDecision: DecisionScale,
			wantGateErr:  true,
		},
		{
			name: "scaleup gate not evaluated on scaledown",
			mutate: func(sa *spannerv1beta1.SpannerAutoscaler) {
				sa.Spec.ScaleConfig.ScaleupCondition = "cpu.highPriority.min15m >= 90"
			},
			desiredPU:    2000,
			wantDecision: DecisionScale,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sa := celTestAutoscaler()
			sa.Status.LastScaleTime = lastScale
			tt.mutate(sa)

			decision, gates, err := Decide(sa, tt.desiredPU, now, time.Minute, time.Minute)
			if err != nil {
				t.Fatalf("Decide() error: %v", err)
			}
			if decision != tt.wantDecision {
				t.Errorf("Decide() = %v, want %v", decision, tt.wantDecision)
			}
			gotGateErr := false
			for _, g := range gates {
				if g.Err != nil {
					gotGateErr = true
				}
			}
			if gotGateErr != tt.wantGateErr {
				t.Errorf("Decide() gate errors = %v (gates %+v), want %v", gotGateErr, gates, tt.wantGateErr)
			}
		})
	}
}

func TestRoundUpToValidPU(t *testing.T) {
	tests := []struct {
		in, want int
	}{
		{0, 0},
		{1, 100},
		{100, 100},
		{950, 1000},
		{1000, 1000},
		{1001, 2000},
		{3750, 4000},
		{6000, 6000},
	}
	for _, tt := range tests {
		if got := roundUpToValidPU(tt.in); got != tt.want {
			t.Errorf("roundUpToValidPU(%d) = %d, want %d", tt.in, got, tt.want)
		}
	}
}

func TestParseMetricWindow(t *testing.T) {
	valid := []string{"1m", "15m", "59m", "60m", "1h"}
	for _, w := range valid {
		if _, err := ParseMetricWindow(w); err != nil {
			t.Errorf("ParseMetricWindow(%q) unexpected error: %v", w, err)
		}
	}
	invalid := []string{"", "0m", "90m", "2h", "1h30m", "15", "15s", "-15m", "15M"}
	for _, w := range invalid {
		if _, err := ParseMetricWindow(w); err == nil {
			t.Errorf("ParseMetricWindow(%q) expected error, got nil", w)
		}
	}
}
