package monitoringemulator

import (
	"testing"
)

func floatPtr(f float64) *float64 { return &f }

// ---- StaticStore ----

func TestStaticStore_LegacySetBothMetrics(t *testing.T) {
	s := NewStaticStore()
	// Legacy mode: same value for both metrics.
	cpu := 0.45
	s.Set("proj", "inst", CPUEntry{HighPriority: &cpu, Total: &cpu})

	got, ok := s.Get("proj", "inst", MetricKindHighPriority)
	if !ok || got != 0.45 {
		t.Errorf("HighPriority: got (%v, %v), want (0.45, true)", got, ok)
	}
	got, ok = s.Get("proj", "inst", MetricKindTotal)
	if !ok || got != 0.45 {
		t.Errorf("Total: got (%v, %v), want (0.45, true)", got, ok)
	}
}

func TestStaticStore_PerMetricValues(t *testing.T) {
	s := NewStaticStore()
	hp, tot := 0.65, 0.40
	s.Set("proj", "inst", CPUEntry{HighPriority: &hp, Total: &tot})

	got, ok := s.Get("proj", "inst", MetricKindHighPriority)
	if !ok || got != 0.65 {
		t.Errorf("HighPriority: got (%v, %v), want (0.65, true)", got, ok)
	}
	got, ok = s.Get("proj", "inst", MetricKindTotal)
	if !ok || got != 0.40 {
		t.Errorf("Total: got (%v, %v), want (0.40, true)", got, ok)
	}
}

func TestStaticStore_OnlyHighPriority(t *testing.T) {
	s := NewStaticStore()
	hp := 0.70
	s.Set("proj", "inst", CPUEntry{HighPriority: &hp})

	if _, ok := s.Get("proj", "inst", MetricKindHighPriority); !ok {
		t.Error("expected HighPriority to be set")
	}
	if _, ok := s.Get("proj", "inst", MetricKindTotal); ok {
		t.Error("expected Total to be unset")
	}
}

func TestStaticStore_NotFound(t *testing.T) {
	s := NewStaticStore()
	if _, ok := s.Get("proj", "inst", MetricKindHighPriority); ok {
		t.Error("expected not found")
	}
}

func TestStaticStore_Delete(t *testing.T) {
	s := NewStaticStore()
	cpu := 0.5
	s.Set("proj", "inst", CPUEntry{HighPriority: &cpu})
	s.Delete("proj", "inst")
	if _, ok := s.Get("proj", "inst", MetricKindHighPriority); ok {
		t.Error("expected not found after delete")
	}
}

// ---- WorkloadStore ----

func TestWorkloadStore_PerMetricValues(t *testing.T) {
	s := NewWorkloadStore()
	hp := newWorkloadParams(0.80, 1000)
	tot := newWorkloadParams(0.50, 1000)
	s.Set("proj", "inst", WorkloadEntry{HighPriority: &hp, Total: &tot})

	got, ok := s.Get("proj", "inst", MetricKindHighPriority)
	if !ok || got.Workload != 800 {
		t.Errorf("HighPriority: got (%+v, %v), want workload=800", got, ok)
	}
	got, ok = s.Get("proj", "inst", MetricKindTotal)
	if !ok || got.Workload != 500 {
		t.Errorf("Total: got (%+v, %v), want workload=500", got, ok)
	}
}

func TestWorkloadStore_NotFound(t *testing.T) {
	s := NewWorkloadStore()
	if _, ok := s.Get("proj", "inst", MetricKindHighPriority); ok {
		t.Error("expected not found")
	}
}

// ---- ScenarioStore / ScenarioStep ----

func TestScenarioStep_MetricFor_Legacy(t *testing.T) {
	cpu := 0.50
	step := ScenarioStep{CPUUtilization: &cpu}

	for _, kind := range []MetricKind{MetricKindHighPriority, MetricKindTotal} {
		m := step.metricFor(kind)
		if m == nil || m.CPUUtilization == nil || *m.CPUUtilization != 0.50 {
			t.Errorf("kind=%v: expected legacy fallback with cpu=0.50, got %+v", kind, m)
		}
	}
}

func TestScenarioStep_MetricFor_PerMetric(t *testing.T) {
	hp := 0.70
	tot := 0.40
	step := ScenarioStep{
		HighPriority: &ScenarioMetric{CPUUtilization: &hp},
		Total:        &ScenarioMetric{CPUUtilization: &tot},
	}

	m := step.metricFor(MetricKindHighPriority)
	if m == nil || *m.CPUUtilization != 0.70 {
		t.Errorf("HighPriority: got %+v, want cpu=0.70", m)
	}
	m = step.metricFor(MetricKindTotal)
	if m == nil || *m.CPUUtilization != 0.40 {
		t.Errorf("Total: got %+v, want cpu=0.40", m)
	}
}

func TestScenarioStep_MetricFor_OnlyHighPriority_NilTotal(t *testing.T) {
	hp := 0.70
	step := ScenarioStep{
		HighPriority: &ScenarioMetric{CPUUtilization: &hp},
		// Total not set
	}
	if m := step.metricFor(MetricKindTotal); m != nil {
		t.Errorf("expected nil for Total, got %+v", m)
	}
}

func TestScenarioStore_ValidationErrors(t *testing.T) {
	tests := []struct {
		name    string
		steps   []ScenarioStep
		wantErr string
	}{
		{
			name:    "empty steps",
			steps:   []ScenarioStep{},
			wantErr: "at least one step",
		},
		{
			name: "no duration",
			steps: []ScenarioStep{
				{CPUUtilization: floatPtr(0.5)},
			},
			wantErr: "duration must be positive",
		},
		{
			name: "mix per-metric and legacy",
			steps: []ScenarioStep{
				{
					Duration:       Duration{1e9},
					CPUUtilization: floatPtr(0.5),
					HighPriority:   &ScenarioMetric{CPUUtilization: floatPtr(0.7)},
				},
			},
			wantErr: "cannot mix",
		},
		{
			name: "per-metric missing both fields",
			steps: []ScenarioStep{
				{
					Duration:     Duration{1e9},
					HighPriority: &ScenarioMetric{},
				},
			},
			wantErr: "must set cpu_utilization or workload",
		},
		{
			name: "per-metric both fields set",
			steps: []ScenarioStep{
				{
					Duration: Duration{1e9},
					HighPriority: &ScenarioMetric{
						CPUUtilization: floatPtr(0.5),
						Workload:       &WorkloadScenario{CPUUtilization: 0.5, ReferenceProcessingUnits: 1000},
					},
				},
			},
			wantErr: "mutually exclusive",
		},
	}

	store := NewScenarioStore()
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := store.Set("proj", "inst", tt.steps)
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			if tt.wantErr != "" {
				if !contains(err.Error(), tt.wantErr) {
					t.Errorf("error %q does not contain %q", err.Error(), tt.wantErr)
				}
			}
		})
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || sub == "" ||
		func() bool {
			for i := 0; i+len(sub) <= len(s); i++ {
				if s[i:i+len(sub)] == sub {
					return true
				}
			}
			return false
		}())
}
