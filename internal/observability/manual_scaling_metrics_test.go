// Copyright 2026 Mercari, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package observability

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

// TestRecordManualScalingActive_ZeroesOtherRamp asserts the helper sets the
// active ramp series to 1 and zeroes the opposite ramp series, so dashboards
// never see two ramp variants both reporting 1 for the same autoscaler.
func TestRecordManualScalingActive_ZeroesOtherRamp(t *testing.T) {
	l := labelsFor("ms-active-ramp")
	teardown(t, l)

	RecordManualScalingActive(l, true, true /* ramp */)

	gotRamp := testutil.ToFloat64(manualScalingActive.WithLabelValues(
		l.Namespace, l.Name, l.ProjectID, l.InstanceID, "true",
	))
	if gotRamp != 1 {
		t.Errorf("manual_scaling_active{ramp=true} = %v, want 1", gotRamp)
	}
	gotSingle := testutil.ToFloat64(manualScalingActive.WithLabelValues(
		l.Namespace, l.Name, l.ProjectID, l.InstanceID, "false",
	))
	if gotSingle != 0 {
		t.Errorf("manual_scaling_active{ramp=false} = %v, want 0", gotSingle)
	}

	// Switching to single-jump should zero the ramp series and set the
	// single-jump series.
	RecordManualScalingActive(l, true, false /* not ramp */)
	gotRamp = testutil.ToFloat64(manualScalingActive.WithLabelValues(
		l.Namespace, l.Name, l.ProjectID, l.InstanceID, "true",
	))
	if gotRamp != 0 {
		t.Errorf("after single-jump apply, manual_scaling_active{ramp=true} = %v, want 0", gotRamp)
	}
	gotSingle = testutil.ToFloat64(manualScalingActive.WithLabelValues(
		l.Namespace, l.Name, l.ProjectID, l.InstanceID, "false",
	))
	if gotSingle != 1 {
		t.Errorf("after single-jump apply, manual_scaling_active{ramp=false} = %v, want 1", gotSingle)
	}
}

// TestRecordManualScalingActive_Deactivate ensures Set(false) drops the
// gauge to 0 (used when the override is deactivated and the autoscaler
// returns to CPU-driven mode).
func TestRecordManualScalingActive_Deactivate(t *testing.T) {
	l := labelsFor("ms-deactivate")
	teardown(t, l)

	RecordManualScalingActive(l, true, true)
	RecordManualScalingActive(l, false, false)

	gotRamp := testutil.ToFloat64(manualScalingActive.WithLabelValues(
		l.Namespace, l.Name, l.ProjectID, l.InstanceID, "true",
	))
	gotSingle := testutil.ToFloat64(manualScalingActive.WithLabelValues(
		l.Namespace, l.Name, l.ProjectID, l.InstanceID, "false",
	))
	if gotRamp != 0 || gotSingle != 0 {
		t.Errorf("after deactivate: ramp=%v single=%v; want both 0", gotRamp, gotSingle)
	}
}

// TestRecordManualScalingTarget covers the target PU gauge: non-zero sets the
// requested ramp variant; zero clears both variants (mirroring deactivation).
func TestRecordManualScalingTarget(t *testing.T) {
	l := labelsFor("ms-target")
	teardown(t, l)

	RecordManualScalingTarget(l, 7000, true /* ramp */)
	got := testutil.ToFloat64(manualScalingTargetPU.WithLabelValues(
		l.Namespace, l.Name, l.ProjectID, l.InstanceID, "true",
	))
	if got != 7000 {
		t.Errorf("target_pu{ramp=true} = %v, want 7000", got)
	}

	RecordManualScalingTarget(l, 0, false)
	for _, ramp := range []string{"true", "false"} {
		v := testutil.ToFloat64(manualScalingTargetPU.WithLabelValues(
			l.Namespace, l.Name, l.ProjectID, l.InstanceID, ramp,
		))
		if v != 0 {
			t.Errorf("after deactivate: target_pu{ramp=%s} = %v, want 0", ramp, v)
		}
	}
}

// TestRecordManualScalingHistoryEvicted asserts the per-namespace counter
// increments on each call (the GC fires this once per deleted CR).
func TestRecordManualScalingHistoryEvicted(t *testing.T) {
	ns := "ns-evict-test"
	// Reset only this namespace's series.
	t.Cleanup(func() {
		manualScalingHistoryEvicted.DeleteLabelValues(ns)
	})

	RecordManualScalingHistoryEvicted(ns)
	RecordManualScalingHistoryEvicted(ns)
	RecordManualScalingHistoryEvicted(ns)

	got := testutil.ToFloat64(manualScalingHistoryEvicted.WithLabelValues(ns))
	if got != 3 {
		t.Errorf("history_evicted_total{namespace=%q} = %v, want 3", ns, got)
	}
}

// TestDriverForManualScaling locks down the driver-value mapping (ramp ->
// manual_ramp, no ramp -> manual_immediate). The reconciler relies on this
// to keep scale_events_total label-shape backwards compatible with the
// existing driver enum.
func TestDriverForManualScaling(t *testing.T) {
	if got := DriverForManualScaling(true); got != DriverManualRamp {
		t.Errorf("ramp=true -> %q, want %q", got, DriverManualRamp)
	}
	if got := DriverForManualScaling(false); got != DriverManualImmediate {
		t.Errorf("ramp=false -> %q, want %q", got, DriverManualImmediate)
	}
}

// TestRampLabelValue locks down the canonical "true"/"false" string the
// manual_scaling_* gauges use. Centralized to avoid label-literal drift
// across the codebase.
func TestRampLabelValue(t *testing.T) {
	if RampLabelValue(true) != "true" {
		t.Errorf("RampLabelValue(true) = %q, want %q", RampLabelValue(true), "true")
	}
	if RampLabelValue(false) != "false" {
		t.Errorf("RampLabelValue(false) = %q, want %q", RampLabelValue(false), "false")
	}
}
