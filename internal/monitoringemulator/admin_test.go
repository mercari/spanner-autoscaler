package monitoringemulator

import "testing"

func TestStaticEntryFromRequest(t *testing.T) {
	hp := 0.65

	tests := []struct {
		name    string
		req     staticSetRequest
		wantErr string
	}{
		{
			name:    "nothing set is an error",
			req:     staticSetRequest{},
			wantErr: "must set",
		},
		{
			name: "scalar and regional high_priority together is an error",
			req: staticSetRequest{
				HighPriority:        &hp,
				HighPriorityRegions: map[string]float64{"asia-northeast1": 0.31},
			},
			wantErr: "mutually exclusive",
		},
		{
			name: "scalar and regional total together is an error",
			req: staticSetRequest{
				Total:        &hp,
				TotalRegions: map[string]float64{"asia-northeast1": 0.31},
			},
			wantErr: "mutually exclusive",
		},
		{
			name: "out-of-range region value is an error",
			req: staticSetRequest{
				HighPriorityRegions: map[string]float64{"asia-northeast1": 1.5},
			},
			wantErr: "between 0.0 and 1.0",
		},
		{
			name: "scalar high_priority alone is valid",
			req:  staticSetRequest{HighPriority: &hp},
		},
		{
			name: "regional high_priority alone is valid",
			req: staticSetRequest{
				HighPriorityRegions: map[string]float64{"asia-northeast1": 0.31, "asia-northeast2": 0.14},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			entry, err := staticEntryFromRequest(tt.req)
			if tt.wantErr != "" {
				if err == nil {
					t.Fatal("expected an error, got nil")
				}
				if !contains(err.Error(), tt.wantErr) {
					t.Errorf("error %q does not contain %q", err.Error(), tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if _, ok := entry.regions(MetricKindHighPriority); !ok {
				t.Error("expected HighPriority to be set on the resulting entry")
			}
		})
	}
}
