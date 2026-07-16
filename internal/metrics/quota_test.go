package metrics

import (
	"errors"
	"testing"
)

func TestResolveQuotaTarget(t *testing.T) {
	tests := []struct {
		name            string
		instanceConfig  string
		wantQuotaMetric string
		wantLocation    string
		wantLimitName   string
		wantUnsupported bool
	}{
		{
			name:            "regional",
			instanceConfig:  "projects/p/instanceConfigs/regional-asia-northeast1",
			wantQuotaMetric: "spanner.googleapis.com/nodes",
			wantLocation:    "asia-northeast1",
			wantLimitName:   "SpannerNodesPerProject",
		},
		{
			name:            "multi region",
			instanceConfig:  "projects/p/instanceConfigs/nam6",
			wantQuotaMetric: "spanner.googleapis.com/nodes_nam6",
		},
		{
			name:            "multi region with hyphens",
			instanceConfig:  "projects/p/instanceConfigs/nam-eur-asia1",
			wantQuotaMetric: "spanner.googleapis.com/nodes_nam_eur_asia1",
		},
		{
			name:            "dual region",
			instanceConfig:  "projects/p/instanceConfigs/dual-region-japan1",
			wantQuotaMetric: "spanner.googleapis.com/nodes_dual_region_japan1",
		},
		{
			name:            "custom unsupported",
			instanceConfig:  "projects/p/instanceConfigs/custom-foo",
			wantUnsupported: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := resolveQuotaTarget(tt.instanceConfig)
			if tt.wantUnsupported {
				if !errors.Is(err, ErrUnsupportedInstanceConfig) {
					t.Fatalf("resolveQuotaTarget() error = %v, want ErrUnsupportedInstanceConfig", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolveQuotaTarget() error = %v", err)
			}
			if got.quotaMetric != tt.wantQuotaMetric {
				t.Errorf("quotaMetric = %q, want %q", got.quotaMetric, tt.wantQuotaMetric)
			}
			if got.location != tt.wantLocation {
				t.Errorf("location = %q, want %q", got.location, tt.wantLocation)
			}
			if got.limitName != tt.wantLimitName {
				t.Errorf("limitName = %q, want %q", got.limitName, tt.wantLimitName)
			}
		})
	}
}

func TestQuotaFilter(t *testing.T) {
	got := quotaFilter(quotaLimitMetricType)
	want := `metric.type = "serviceruntime.googleapis.com/quota/limit" AND resource.type = "consumer_quota" AND resource.labels.service = "spanner.googleapis.com"`
	if got != want {
		t.Errorf("quotaFilter() = %q, want %q", got, want)
	}
}
