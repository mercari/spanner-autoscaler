package controller

import (
	"testing"

	spannerv1beta1 "github.com/mercari/spanner-autoscaler/api/v1beta1"
	"github.com/mercari/spanner-autoscaler/internal/observability"
)

func TestScaleDriverMinFloor(t *testing.T) {
	newSA := func(effectiveMin, desiredMin int) *spannerv1beta1.SpannerAutoscaler {
		return &spannerv1beta1.SpannerAutoscaler{
			Spec: spannerv1beta1.SpannerAutoscalerSpec{ScaleConfig: spannerv1beta1.ScaleConfig{
				ProcessingUnits:      spannerv1beta1.ScaleConfigPUs{Min: effectiveMin, Max: 80000},
				TargetCPUUtilization: spannerv1beta1.TargetCPUUtilization{HighPriority: new(50)},
			}},
			Status: spannerv1beta1.SpannerAutoscalerStatus{DesiredMinPUs: desiredMin},
		}
	}

	tests := []struct {
		name         string
		effectiveMin int
		desiredMin   int
		after        int
		want         string
	}{
		{"override raised the min", 14000, 14000, 14000, observability.DriverTimeBasedOverride},
		{"schedule on top of an override", 14000, 26000, 26000, observability.DriverSchedule},
		{"schedule without override", 12000, 24000, 24000, observability.DriverSchedule},
		{"override lowered the min", 8000, 8000, 8000, observability.DriverCPUHighPriority},
		{"above the floor", 14000, 14000, 16000, observability.DriverCPUHighPriority},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := scaleDriver(newSA(tt.effectiveMin, tt.desiredMin), 12000, tt.after); got != tt.want {
				t.Errorf("scaleDriver() = %q; want %q", got, tt.want)
			}
		})
	}
}
