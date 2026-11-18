package scaling

import (
	"testing"

	"k8s.io/apimachinery/pkg/util/intstr"

	spannerv1beta1 "github.com/mercari/spanner-autoscaler/api/v1beta1"
)

func TestDesiredPUFromCPUStepsDownToMax(t *testing.T) {
	newSA := func(currentPU, minPU, maxPU, desiredMinPU, desiredMaxPU int, step intstr.IntOrString) spannerv1beta1.SpannerAutoscaler {
		return spannerv1beta1.SpannerAutoscaler{
			Spec: spannerv1beta1.SpannerAutoscalerSpec{ScaleConfig: spannerv1beta1.ScaleConfig{
				ProcessingUnits:   spannerv1beta1.ScaleConfigPUs{Min: minPU, Max: maxPU},
				ScaledownStepSize: step,
			}},
			Status: spannerv1beta1.SpannerAutoscalerStatus{
				CurrentProcessingUnits: currentPU,
				DesiredMinPUs:          desiredMinPU,
				DesiredMaxPUs:          desiredMaxPU,
			},
		}
	}

	tests := []struct {
		name       string
		sa         spannerv1beta1.SpannerAutoscaler
		currentCPU int
		want       int
	}{
		{
			name:       "spec max lowered below current",
			sa:         newSA(30000, 1000, 20000, 0, 0, intstr.FromString("10%")),
			currentCPU: 20,
			want:       27000,
		},
		{
			name:       "high CPU above a lowered max still steps down",
			sa:         newSA(30000, 1000, 20000, 0, 0, intstr.FromString("10%")),
			currentCPU: 90,
			want:       27000,
		},
		{
			name:       "last step reaches the max",
			sa:         newSA(22000, 1000, 20000, 0, 0, intstr.FromInt(3000)),
			currentCPU: 90,
			want:       20000,
		},
		{
			name:       "exceed schedule ended",
			sa:         newSA(40000, 1000, 30000, 1000, 30000, intstr.FromInt(2000)),
			currentCPU: 20,
			want:       38000,
		},
		{
			name:       "CPU-driven scale down is unchanged",
			sa:         newSA(30000, 1000, 80000, 0, 0, intstr.FromString("10%")),
			currentCPU: 10,
			want:       27000,
		},
		{
			name:       "raised min still scales up at once",
			sa:         newSA(10000, 20000, 80000, 0, 0, intstr.FromInt(2000)),
			currentCPU: 10,
			want:       20000,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := DesiredPUFromCPU(tt.currentCPU, 50, tt.sa); got != tt.want {
				t.Errorf("DesiredPUFromCPU() = %d; want %d", got, tt.want)
			}
		})
	}
}

func TestDesiredProcessingUnitsDualModeStepsDownToMax(t *testing.T) {
	high, total := 50, 70
	newSA := func(currentPU, maxPU, highCPU, totalCPU int) spannerv1beta1.SpannerAutoscaler {
		return spannerv1beta1.SpannerAutoscaler{
			Spec: spannerv1beta1.SpannerAutoscalerSpec{ScaleConfig: spannerv1beta1.ScaleConfig{
				ProcessingUnits:      spannerv1beta1.ScaleConfigPUs{Min: 1000, Max: maxPU},
				TargetCPUUtilization: spannerv1beta1.TargetCPUUtilization{HighPriority: &high, Total: &total},
				ScaledownStepSize:    intstr.FromString("10%"),
			}},
			Status: spannerv1beta1.SpannerAutoscalerStatus{
				CurrentProcessingUnits:            currentPU,
				CurrentCPUMetricType:              spannerv1beta1.CPUMetricTypeBoth,
				CurrentHighPriorityCPUUtilization: highCPU,
				CurrentTotalCPUUtilization:        totalCPU,
			},
		}
	}

	tests := []struct {
		name string
		sa   spannerv1beta1.SpannerAutoscaler
		want int
	}{
		{name: "both metrics low", sa: newSA(30000, 20000, 10, 10), want: 27000},
		{name: "high-priority CPU above target", sa: newSA(30000, 20000, 90, 10), want: 27000},
		{name: "total CPU above target", sa: newSA(30000, 20000, 10, 95), want: 27000},
		{name: "last step reaches the max", sa: newSA(21000, 20000, 90, 95), want: 20000},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := DesiredProcessingUnits(tt.sa); got != tt.want {
				t.Errorf("DesiredProcessingUnits() = %d; want %d", got, tt.want)
			}
		})
	}
}
