package scaling

import (
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	spannerv1beta1 "github.com/mercari/spanner-autoscaler/api/v1beta1"
)

func baseScaleConfig() spannerv1beta1.ScaleConfig {
	return spannerv1beta1.ScaleConfig{
		ProcessingUnits:      spannerv1beta1.ScaleConfigPUs{Min: 12000, Max: 80000},
		TargetCPUUtilization: spannerv1beta1.TargetCPUUtilization{HighPriority: new(50), Total: new(80)},
		ScaledownStepSize:    intstr.FromString("10%"),
		ScaledownInterval:    &metav1.Duration{Duration: 30 * time.Minute},
	}
}

var jst = time.FixedZone("JST", 9*60*60)

func TestResolveScaleConfig(t *testing.T) {
	night := spannerv1beta1.TimeBasedOverride{
		Name:              "night",
		Times:             []string{"CRON_TZ=Asia/Tokyo * 1-6 * * *"},
		ProcessingUnits:   &spannerv1beta1.TimeBasedOverridePUs{Min: new(8000)},
		ScaledownStepSize: new(intstr.FromString("30%")),
	}
	morning := spannerv1beta1.TimeBasedOverride{
		Name:              "after-morning-spike",
		Times:             []string{"CRON_TZ=Asia/Tokyo 36-59 9 * * *", "CRON_TZ=Asia/Tokyo * 10 * * *"},
		ScaledownStepSize: new(intstr.FromString("25%")),
	}
	daytime := spannerv1beta1.TimeBasedOverride{
		Name:            "daytime",
		Times:           []string{"* * * * *"},
		ScaledownPolicy: spannerv1beta1.ScaledownPolicyDeny,
	}

	withOverrides := func(overrides ...spannerv1beta1.TimeBasedOverride) spannerv1beta1.ScaleConfig {
		sc := baseScaleConfig()
		sc.TimeBasedOverrides = overrides
		return sc
	}
	modified := func(f func(*spannerv1beta1.ScaleConfig)) spannerv1beta1.ScaleConfig {
		sc := baseScaleConfig()
		f(&sc)
		return sc
	}

	tests := []struct {
		name    string
		sc      spannerv1beta1.ScaleConfig
		now     time.Time
		want    ResolvedScaleConfig
		wantErr bool
	}{
		{
			name: "no overrides",
			sc:   baseScaleConfig(),
			now:  time.Date(2026, 11, 17, 3, 0, 0, 0, jst),
			want: ResolvedScaleConfig{ScaleConfig: baseScaleConfig()},
		},
		{
			name: "night override replaces min and step only",
			sc:   withOverrides(night, morning, daytime),
			now:  time.Date(2026, 11, 17, 3, 0, 0, 0, jst),
			want: ResolvedScaleConfig{
				ScaleConfig: modified(func(sc *spannerv1beta1.ScaleConfig) {
					sc.ProcessingUnits.Min = 8000
					sc.ScaledownStepSize = intstr.FromString("30%")
				}),
				OverrideName: "night",
			},
		},
		{
			name: "second expression of an override matches",
			sc:   withOverrides(night, morning, daytime),
			now:  time.Date(2026, 11, 17, 10, 59, 30, 0, jst),
			want: ResolvedScaleConfig{
				ScaleConfig:  modified(func(sc *spannerv1beta1.ScaleConfig) { sc.ScaledownStepSize = intstr.FromString("25%") }),
				OverrideName: "after-morning-spike",
			},
		},
		{
			name: "catch-all applies where earlier overrides do not match",
			sc:   withOverrides(night, morning, daytime),
			now:  time.Date(2026, 11, 17, 9, 35, 0, 0, jst),
			want: ResolvedScaleConfig{
				ScaleConfig:     baseScaleConfig(),
				OverrideName:    "daytime",
				ScaledownPolicy: spannerv1beta1.ScaledownPolicyDeny,
			},
		},
		{
			name: "first match wins",
			sc:   withOverrides(daytime, night),
			now:  time.Date(2026, 11, 17, 3, 0, 0, 0, jst),
			want: ResolvedScaleConfig{
				ScaleConfig:     baseScaleConfig(),
				OverrideName:    "daytime",
				ScaledownPolicy: spannerv1beta1.ScaledownPolicyDeny,
			},
		},
		{
			name: "no override matches",
			sc:   withOverrides(night),
			now:  time.Date(2026, 11, 17, 7, 0, 0, 0, jst),
			want: ResolvedScaleConfig{ScaleConfig: baseScaleConfig()},
		},
		{
			name: "all fields replaced",
			sc: withOverrides(spannerv1beta1.TimeBasedOverride{
				Name:                 "all",
				Times:                []string{"* * * * *"},
				ProcessingUnits:      &spannerv1beta1.TimeBasedOverridePUs{Min: new(5000), Max: new(20000)},
				TargetCPUUtilization: &spannerv1beta1.TargetCPUUtilization{HighPriority: new(65), Total: new(90)},
				ScaledownStepSize:    new(intstr.FromInt(3000)),
				ScaledownInterval:    &metav1.Duration{Duration: 10 * time.Minute},
				ScaleupStepSize:      new(intstr.FromInt(5000)),
				ScaleupInterval:      &metav1.Duration{Duration: 5 * time.Minute},
				ScaledownPolicy:      spannerv1beta1.ScaledownPolicyAllow,
			}),
			now: time.Date(2026, 11, 17, 12, 0, 0, 0, jst),
			want: ResolvedScaleConfig{
				ScaleConfig: modified(func(sc *spannerv1beta1.ScaleConfig) {
					sc.ProcessingUnits = spannerv1beta1.ScaleConfigPUs{Min: 5000, Max: 20000}
					sc.TargetCPUUtilization = spannerv1beta1.TargetCPUUtilization{HighPriority: new(65), Total: new(90)}
					sc.ScaledownStepSize = intstr.FromInt(3000)
					sc.ScaledownInterval = &metav1.Duration{Duration: 10 * time.Minute}
					sc.ScaleupStepSize = intstr.FromInt(5000)
					sc.ScaleupInterval = &metav1.Duration{Duration: 5 * time.Minute}
				}),
				OverrideName:    "all",
				ScaledownPolicy: spannerv1beta1.ScaledownPolicyAllow,
			},
		},
		{
			name: "invalid cron denies scale down with the base config",
			sc: withOverrides(spannerv1beta1.TimeBasedOverride{
				Name: "broken", Times: []string{"not a cron"}, ProcessingUnits: &spannerv1beta1.TimeBasedOverridePUs{Min: new(1000)},
			}),
			now:     time.Date(2026, 11, 17, 3, 0, 0, 0, jst),
			want:    ResolvedScaleConfig{ScaleConfig: baseScaleConfig(), ScaledownPolicy: spannerv1beta1.ScaledownPolicyDeny},
			wantErr: true,
		},
		{
			name: "used together with scaledownAllowedTimes",
			sc: modified(func(sc *spannerv1beta1.ScaleConfig) {
				sc.ScaledownAllowedTimes = []string{"* 1-7 * * *"}
				sc.TimeBasedOverrides = []spannerv1beta1.TimeBasedOverride{night}
			}),
			now: time.Date(2026, 11, 17, 3, 0, 0, 0, jst),
			want: ResolvedScaleConfig{
				ScaleConfig:     modified(func(sc *spannerv1beta1.ScaleConfig) { sc.ScaledownAllowedTimes = []string{"* 1-7 * * *"} }),
				ScaledownPolicy: spannerv1beta1.ScaledownPolicyDeny,
			},
			wantErr: true,
		},
		{
			name: "target for a metric missing in the scale config",
			sc: modified(func(sc *spannerv1beta1.ScaleConfig) {
				sc.TargetCPUUtilization.Total = nil
				sc.TimeBasedOverrides = []spannerv1beta1.TimeBasedOverride{{
					Name: "t", Times: []string{"* * * * *"}, TargetCPUUtilization: &spannerv1beta1.TargetCPUUtilization{Total: new(90)},
				}}
			}),
			now: time.Date(2026, 11, 17, 3, 0, 0, 0, jst),
			want: ResolvedScaleConfig{
				ScaleConfig:     modified(func(sc *spannerv1beta1.ScaleConfig) { sc.TargetCPUUtilization.Total = nil }),
				ScaledownPolicy: spannerv1beta1.ScaledownPolicyDeny,
			},
			wantErr: true,
		},
		{
			name: "min above max after the override",
			sc: withOverrides(spannerv1beta1.TimeBasedOverride{
				Name: "m", Times: []string{"* * * * *"}, ProcessingUnits: &spannerv1beta1.TimeBasedOverridePUs{Max: new(10000)},
			}),
			now:     time.Date(2026, 11, 17, 3, 0, 0, 0, jst),
			want:    ResolvedScaleConfig{ScaleConfig: baseScaleConfig(), ScaledownPolicy: spannerv1beta1.ScaledownPolicyDeny},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ResolveScaleConfig(tt.sc, tt.now)
			if (err != nil) != tt.wantErr {
				t.Fatalf("error = %v, wantErr %v", err, tt.wantErr)
			}
			if diff := cmp.Diff(tt.want, got); diff != "" {
				t.Errorf("ResolveScaleConfig() mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestResolveScaleConfigDoesNotAliasBase(t *testing.T) {
	sc := baseScaleConfig()
	sc.TimeBasedOverrides = []spannerv1beta1.TimeBasedOverride{{
		Name:                 "t",
		Times:                []string{"* * * * *"},
		TargetCPUUtilization: &spannerv1beta1.TargetCPUUtilization{HighPriority: new(70)},
		ScaledownInterval:    &metav1.Duration{Duration: time.Minute},
	}}

	got, err := ResolveScaleConfig(sc, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	*got.ScaleConfig.TargetCPUUtilization.HighPriority = 1
	got.ScaleConfig.ScaledownInterval.Duration = time.Hour

	if v := *sc.TimeBasedOverrides[0].TargetCPUUtilization.HighPriority; v != 70 {
		t.Errorf("override target changed to %d", v)
	}
	if v := sc.TimeBasedOverrides[0].ScaledownInterval.Duration; v != time.Minute {
		t.Errorf("override interval changed to %s", v)
	}
	if v := *sc.TargetCPUUtilization.HighPriority; v != 50 {
		t.Errorf("base target changed to %d", v)
	}
}

func TestDecideScaledownPolicy(t *testing.T) {
	now := time.Date(2026, 11, 17, 12, 0, 0, 0, jst)
	sa := &spannerv1beta1.SpannerAutoscaler{
		Spec: spannerv1beta1.SpannerAutoscalerSpec{ScaleConfig: baseScaleConfig()},
		Status: spannerv1beta1.SpannerAutoscalerStatus{
			CurrentProcessingUnits: 20000,
			LastScaleTime:          metav1.Time{Time: now.Add(-time.Hour)},
		},
	}
	recent := sa.DeepCopy()
	recent.Status.LastScaleTime = metav1.Time{Time: now.Add(-time.Minute)}

	tests := []struct {
		name    string
		sa      *spannerv1beta1.SpannerAutoscaler
		desired int
		policy  spannerv1beta1.ScaledownPolicy
		want    Decision
	}{
		{"deny skips scale down", sa, 18000, spannerv1beta1.ScaledownPolicyDeny, DecisionSkipScaleDownPolicy},
		{"deny does not affect scale up", sa, 22000, spannerv1beta1.ScaledownPolicyDeny, DecisionScale},
		{"allow permits scale down", sa, 18000, spannerv1beta1.ScaledownPolicyAllow, DecisionScale},
		{"empty policy permits scale down", sa, 18000, "", DecisionScale},
		{"interval is reported before policy", recent, 18000, spannerv1beta1.ScaledownPolicyDeny, DecisionSkipScaleDownInterval},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Decide(tt.sa, tt.desired, now, tt.policy, time.Minute, 55*time.Minute)
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Errorf("Decide() = %v, want %v", got, tt.want)
			}
		})
	}
}

// The documented migration from scaledownAllowedTimes to an Allow override
// followed by a catch-all Deny must decide identically at every minute.
func TestMigrationFromScaledownAllowedTimesIsEquivalent(t *testing.T) {
	legacy := baseScaleConfig()
	legacy.ScaledownAllowedTimes = []string{"CRON_TZ=Asia/Tokyo * 1-7 * * *"}

	migrated := baseScaleConfig()
	migrated.TimeBasedOverrides = []spannerv1beta1.TimeBasedOverride{
		{Name: "night", Times: []string{"CRON_TZ=Asia/Tokyo * 1-7 * * *"}, ScaledownPolicy: spannerv1beta1.ScaledownPolicyAllow},
		{Name: "others", Times: []string{"* * * * *"}, ScaledownPolicy: spannerv1beta1.ScaledownPolicyDeny},
	}

	decide := func(sc spannerv1beta1.ScaleConfig, now time.Time) Decision {
		t.Helper()
		resolved, err := ResolveScaleConfig(sc, now)
		if err != nil {
			t.Fatal(err)
		}
		sa := &spannerv1beta1.SpannerAutoscaler{
			Spec: spannerv1beta1.SpannerAutoscalerSpec{ScaleConfig: resolved.ScaleConfig},
			Status: spannerv1beta1.SpannerAutoscalerStatus{
				CurrentProcessingUnits: 20000,
				LastScaleTime:          metav1.Time{Time: now.Add(-time.Hour)},
			},
		}
		d, err := Decide(sa, 18000, now, resolved.ScaledownPolicy, time.Minute, 55*time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		return d
	}

	start := time.Date(2026, 11, 16, 0, 0, 0, 0, time.UTC)
	allowed := 0
	for now := start; now.Before(start.Add(7 * 24 * time.Hour)); now = now.Add(time.Minute) {
		l, m := decide(legacy, now), decide(migrated, now)
		if (l == DecisionScale) != (m == DecisionScale) {
			t.Fatalf("at %s: legacy decided %v, migrated decided %v", now, l, m)
		}
		if l == DecisionScale {
			allowed++
		}
	}
	if want := 7 * 7 * 60; allowed != want {
		t.Errorf("scale down allowed for %d minutes, want %d", allowed, want)
	}
}
