package v1beta1

import (
	"fmt"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	spannerv1beta1 "github.com/mercari/spanner-autoscaler/api/v1beta1"
)

func TestValidateTimeBasedOverrides(t *testing.T) {
	base := func() spannerv1beta1.ScaleConfig {
		return spannerv1beta1.ScaleConfig{
			ProcessingUnits:      spannerv1beta1.ScaleConfigPUs{Min: 12000, Max: 80000},
			TargetCPUUtilization: spannerv1beta1.TargetCPUUtilization{HighPriority: new(50)},
		}
	}
	override := func(f func(*spannerv1beta1.TimeBasedOverride)) spannerv1beta1.TimeBasedOverride {
		o := spannerv1beta1.TimeBasedOverride{
			Name:            "o",
			Times:           []string{"CRON_TZ=Asia/Tokyo * 1-6 * * *"},
			ScaledownPolicy: spannerv1beta1.ScaledownPolicyDeny,
		}
		if f != nil {
			f(&o)
		}
		return o
	}

	tests := []struct {
		name     string
		modify   func(*spannerv1beta1.ScaleConfig)
		maxCount int
		wantErrs []string
	}{
		{
			name: "valid example",
			modify: func(sc *spannerv1beta1.ScaleConfig) {
				sc.TimeBasedOverrides = []spannerv1beta1.TimeBasedOverride{
					{
						Name:                 "night",
						Times:                []string{"CRON_TZ=Asia/Tokyo * 1-6 * * *"},
						ProcessingUnits:      &spannerv1beta1.TimeBasedOverridePUs{Min: new(8000)},
						TargetCPUUtilization: &spannerv1beta1.TargetCPUUtilization{HighPriority: new(65)},
						ScaledownStepSize:    new(intstr.FromString("30%")),
						ScaledownInterval:    &metav1.Duration{Duration: 10 * time.Minute},
					},
					{Name: "others", Times: []string{"* * * * *"}, ScaledownPolicy: spannerv1beta1.ScaledownPolicyDeny},
				}
			},
		},
		{
			name: "allow-only override is valid",
			modify: func(sc *spannerv1beta1.ScaleConfig) {
				sc.TimeBasedOverrides = []spannerv1beta1.TimeBasedOverride{override(func(o *spannerv1beta1.TimeBasedOverride) {
					o.ScaledownPolicy = spannerv1beta1.ScaledownPolicyAllow
				})}
			},
		},
		{
			name: "used together with scaledownAllowedTimes",
			modify: func(sc *spannerv1beta1.ScaleConfig) {
				sc.ScaledownAllowedTimes = []string{"* 1-7 * * *"}
				sc.TimeBasedOverrides = []spannerv1beta1.TimeBasedOverride{override(nil)}
			},
			wantErrs: []string{"cannot be used together with scaledownAllowedTimes or scaledownNotAllowedTimes"},
		},
		{
			name: "used together with scaledownNotAllowedTimes",
			modify: func(sc *spannerv1beta1.ScaleConfig) {
				sc.ScaledownNotAllowedTimes = []string{"* 8-23 * * *"}
				sc.TimeBasedOverrides = []spannerv1beta1.TimeBasedOverride{override(nil)}
			},
			wantErrs: []string{"cannot be used together with scaledownAllowedTimes or scaledownNotAllowedTimes"},
		},
		{
			name: "too many overrides",
			modify: func(sc *spannerv1beta1.ScaleConfig) {
				for i := range 3 {
					sc.TimeBasedOverrides = append(sc.TimeBasedOverrides, override(func(o *spannerv1beta1.TimeBasedOverride) {
						o.Name = fmt.Sprintf("o%d", i)
					}))
				}
			},
			maxCount: 2,
			wantErrs: []string{"must have at most 2 items"},
		},
		{
			name: "limit disabled",
			modify: func(sc *spannerv1beta1.ScaleConfig) {
				for i := range 20 {
					sc.TimeBasedOverrides = append(sc.TimeBasedOverrides, override(func(o *spannerv1beta1.TimeBasedOverride) {
						o.Name = fmt.Sprintf("o%d", i)
					}))
				}
			},
		},
		{
			name: "missing and duplicate names",
			modify: func(sc *spannerv1beta1.ScaleConfig) {
				sc.TimeBasedOverrides = []spannerv1beta1.TimeBasedOverride{
					override(nil),
					override(nil),
					override(func(o *spannerv1beta1.TimeBasedOverride) { o.Name = "" }),
				}
			},
			wantErrs: []string{`timeBasedOverrides[1].name: Duplicate value: "o"`, "timeBasedOverrides[2].name: Required value"},
		},
		{
			name: "missing and invalid times",
			modify: func(sc *spannerv1beta1.ScaleConfig) {
				sc.TimeBasedOverrides = []spannerv1beta1.TimeBasedOverride{
					override(func(o *spannerv1beta1.TimeBasedOverride) { o.Name = "a"; o.Times = nil }),
					override(func(o *spannerv1beta1.TimeBasedOverride) { o.Name = "b"; o.Times = []string{"* * * * *", "bad"} }),
				}
			},
			wantErrs: []string{"timeBasedOverrides[0].times: Required value", "timeBasedOverrides[1].times[1]: Invalid value"},
		},
		{
			name: "nothing to override",
			modify: func(sc *spannerv1beta1.ScaleConfig) {
				sc.TimeBasedOverrides = []spannerv1beta1.TimeBasedOverride{override(func(o *spannerv1beta1.TimeBasedOverride) { o.ScaledownPolicy = "" })}
			},
			wantErrs: []string{"at least one field other than name and times must be specified"},
		},
		{
			name: "invalid processing units",
			modify: func(sc *spannerv1beta1.ScaleConfig) {
				sc.TimeBasedOverrides = []spannerv1beta1.TimeBasedOverride{
					override(func(o *spannerv1beta1.TimeBasedOverride) {
						o.Name = "a"
						o.ProcessingUnits = &spannerv1beta1.TimeBasedOverridePUs{Min: new(1500)}
					}),
					override(func(o *spannerv1beta1.TimeBasedOverride) {
						o.Name = "b"
						o.ProcessingUnits = &spannerv1beta1.TimeBasedOverridePUs{Max: new(10000)}
					}),
				}
			},
			wantErrs: []string{
				"timeBasedOverrides[0].processingUnits.min: Invalid value: 1500",
				"timeBasedOverrides[1].processingUnits: Invalid value: \"min=12000, max=10000\"",
			},
		},
		{
			name: "processing units below 100",
			modify: func(sc *spannerv1beta1.ScaleConfig) {
				sc.ProcessingUnits.Min = 100
				sc.TimeBasedOverrides = []spannerv1beta1.TimeBasedOverride{override(func(o *spannerv1beta1.TimeBasedOverride) {
					o.ProcessingUnits = &spannerv1beta1.TimeBasedOverridePUs{Min: new(-100), Max: new(0)}
				})}
			},
			wantErrs: []string{
				"timeBasedOverrides[0].processingUnits.min: Invalid value: -100: must be at least 100",
				"timeBasedOverrides[0].processingUnits.max: Invalid value: 0: must be at least 100",
			},
		},
		{
			name: "invalid targets",
			modify: func(sc *spannerv1beta1.ScaleConfig) {
				sc.TimeBasedOverrides = []spannerv1beta1.TimeBasedOverride{
					override(func(o *spannerv1beta1.TimeBasedOverride) {
						o.Name = "a"
						o.TargetCPUUtilization = &spannerv1beta1.TargetCPUUtilization{Total: new(90)}
					}),
					override(func(o *spannerv1beta1.TimeBasedOverride) {
						o.Name = "b"
						o.TargetCPUUtilization = &spannerv1beta1.TargetCPUUtilization{HighPriority: new(100)}
					}),
					override(func(o *spannerv1beta1.TimeBasedOverride) {
						o.Name = "c"
						o.TargetCPUUtilization = &spannerv1beta1.TargetCPUUtilization{}
					}),
				}
			},
			wantErrs: []string{
				"timeBasedOverrides[0].targetCPUUtilization.total: Forbidden",
				"timeBasedOverrides[1].targetCPUUtilization.highPriority: Invalid value: 100",
				"timeBasedOverrides[2].targetCPUUtilization: Required value",
			},
		},
		{
			name: "invalid step sizes, intervals and policy",
			modify: func(sc *spannerv1beta1.ScaleConfig) {
				sc.TimeBasedOverrides = []spannerv1beta1.TimeBasedOverride{override(func(o *spannerv1beta1.TimeBasedOverride) {
					o.ScaledownStepSize = new(intstr.FromInt(1500))
					o.ScaleupStepSize = new(intstr.FromString("abc"))
					o.ScaledownInterval = &metav1.Duration{}
					o.ScaleupInterval = &metav1.Duration{Duration: -time.Minute}
					o.ScaledownPolicy = "Sometimes"
				})}
			},
			wantErrs: []string{
				"timeBasedOverrides[0].scaledownStepSize: Invalid value",
				"timeBasedOverrides[0].scaleupStepSize: Invalid value",
				"timeBasedOverrides[0].scaledownInterval: Invalid value",
				"timeBasedOverrides[0].scaleupInterval: Invalid value",
				"timeBasedOverrides[0].scaledownPolicy: Unsupported value",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sc := base()
			tt.modify(&sc)
			errs := validateTimeBasedOverrides(sc, tt.maxCount)

			if len(errs) != len(tt.wantErrs) {
				t.Fatalf("got %d errors, want %d: %v", len(errs), len(tt.wantErrs), errs)
			}
			for i, want := range tt.wantErrs {
				if got := errs[i].Error(); !strings.Contains(got, want) {
					t.Errorf("error[%d] = %q, want it to contain %q", i, got, want)
				}
			}
		})
	}
}
