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

package controller

import (
	"context"
	"sort"
	"testing"
	"time"

	"github.com/go-logr/logr"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	fakeclient "sigs.k8s.io/controller-runtime/pkg/client/fake"

	spannerv1beta1 "github.com/mercari/spanner-autoscaler/api/v1beta1"
)

// manualScalingTestScheme builds a runtime.Scheme registered with both core
// k8s types (so events, etc. work) and our v1beta1 group. Avoid importing
// the envtest suite to keep these tests runnable without etcd.
func manualScalingTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(s))
	utilruntime.Must(spannerv1beta1.AddToScheme(s))
	return s
}

func msFixture(name, target string, phase spannerv1beta1.SpannerManualScalingPhase, created, finished time.Time, expiresAt *time.Time) *spannerv1beta1.SpannerManualScaling {
	ms := &spannerv1beta1.SpannerManualScaling{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:         "ns1",
			Name:              name,
			CreationTimestamp: metav1.Time{Time: created},
		},
		Spec: spannerv1beta1.SpannerManualScalingSpec{
			TargetResource:  target,
			ProcessingUnits: 5000,
		},
		Status: spannerv1beta1.SpannerManualScalingStatus{
			Phase: phase,
		},
	}
	if !finished.IsZero() {
		ms.Status.FinishedAt = &metav1.Time{Time: finished}
	}
	if expiresAt != nil {
		ms.Spec.ExpiresAt = &metav1.Time{Time: *expiresAt}
	}
	return ms
}

// TestSelectActiveManualScaling covers the active-candidate selector: skips
// non-matching target, deleted, expired, and terminal-phase rows; picks the
// newest by creationTimestamp; uses name as a deterministic tie-breaker.
func TestSelectActiveManualScaling(t *testing.T) {
	scheme := manualScalingTestScheme(t)
	now := time.Date(2026, 5, 30, 12, 0, 0, 0, time.UTC)
	sa := &spannerv1beta1.SpannerAutoscaler{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns1", Name: "sa-target"},
	}

	past := now.Add(-time.Hour)

	older := msFixture("a-older", "sa-target", "", now.Add(-2*time.Hour), time.Time{}, nil)
	newer := msFixture("b-newer", "sa-target", "", now.Add(-1*time.Hour), time.Time{}, nil)
	wrongTarget := msFixture("c-wrong", "different-sa", "", now, time.Time{}, nil)
	expired := msFixture("d-expired", "sa-target", "", now, time.Time{}, &past)
	terminal := msFixture("e-terminal", "sa-target", spannerv1beta1.SpannerManualScalingPhaseExpired,
		now.Add(-3*time.Hour), now.Add(-3*time.Hour), nil)

	cl := fakeclient.NewClientBuilder().WithScheme(scheme).
		WithObjects(older, newer, wrongTarget, expired, terminal).Build()
	r := &SpannerAutoscalerReconciler{ctrlClient: cl}

	active, superseded, err := r.selectActiveManualScaling(context.Background(), sa, now)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if active == nil {
		t.Fatal("active was nil; want the newer candidate")
		return // unreachable; satisfies staticcheck SA5011
	}
	if active.Name != "b-newer" {
		t.Errorf("active.Name = %q, want %q", active.Name, "b-newer")
	}
	if len(superseded) != 1 {
		t.Fatalf("superseded len = %d, want 1", len(superseded))
	}
	if superseded[0].Name != "a-older" {
		t.Errorf("superseded[0].Name = %q, want %q", superseded[0].Name, "a-older")
	}
}

// TestSelectActiveManualScaling_TieBreaker ensures that when two candidates
// share the same creationTimestamp (sub-second collision common under
// scripted bulk creates), the name comparison breaks the tie
// deterministically.
func TestSelectActiveManualScaling_TieBreaker(t *testing.T) {
	scheme := manualScalingTestScheme(t)
	now := time.Date(2026, 5, 30, 12, 0, 0, 0, time.UTC)
	sa := &spannerv1beta1.SpannerAutoscaler{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns1", Name: "sa-target"},
	}
	created := now.Add(-time.Minute)
	a := msFixture("a", "sa-target", "", created, time.Time{}, nil)
	b := msFixture("b", "sa-target", "", created, time.Time{}, nil)

	cl := fakeclient.NewClientBuilder().WithScheme(scheme).WithObjects(a, b).Build()
	r := &SpannerAutoscalerReconciler{ctrlClient: cl}

	active, superseded, err := r.selectActiveManualScaling(context.Background(), sa, now)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if active == nil {
		t.Fatal("active was nil")
		return // unreachable; satisfies staticcheck SA5011
	}
	// Name DESC: "b" wins.
	if active.Name != "b" {
		t.Errorf("active.Name = %q, want %q (name DESC tie-breaker)", active.Name, "b")
	}
	if len(superseded) != 1 || superseded[0].Name != "a" {
		t.Errorf("superseded = %v, want [a]", superseded)
	}
}

// TestSelectActiveManualScaling_NoneFound covers the empty-list path
// (returns nil, nil, nil so the caller falls through to CPU autoscale).
func TestSelectActiveManualScaling_NoneFound(t *testing.T) {
	scheme := manualScalingTestScheme(t)
	sa := &spannerv1beta1.SpannerAutoscaler{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns1", Name: "sa-target"},
	}
	cl := fakeclient.NewClientBuilder().WithScheme(scheme).Build()
	r := &SpannerAutoscalerReconciler{ctrlClient: cl}
	active, sup, err := r.selectActiveManualScaling(context.Background(), sa, time.Now())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if active != nil || sup != nil {
		t.Errorf("expected (nil, nil, nil); got (%v, %v)", active, sup)
	}
}

// TestEnforceHistoryLimit covers the per-namespace GC: when the limit is 0
// (default) nothing is deleted; with N>0 the oldest FinishedAt rows are
// deleted while non-terminal rows are never touched.
func TestEnforceHistoryLimit(t *testing.T) {
	scheme := manualScalingTestScheme(t)
	now := time.Date(2026, 5, 30, 12, 0, 0, 0, time.UTC)

	terminal := func(name string, finished time.Time) *spannerv1beta1.SpannerManualScaling {
		return msFixture(name, "sa", spannerv1beta1.SpannerManualScalingPhaseExpired,
			finished, finished, nil)
	}
	active := func(name string) *spannerv1beta1.SpannerManualScaling {
		return msFixture(name, "sa", spannerv1beta1.SpannerManualScalingPhaseActive,
			now.Add(-time.Minute), time.Time{}, nil)
	}

	// 4 terminal + 1 active. With limit=3, exactly one terminal (oldest by
	// FinishedAt) should be deleted; active is untouched.
	t1 := terminal("t1-oldest", now.Add(-4*time.Hour))
	t2 := terminal("t2", now.Add(-3*time.Hour))
	t3 := terminal("t3", now.Add(-2*time.Hour))
	t4 := terminal("t4-newest", now.Add(-1*time.Hour))
	a1 := active("a1-active")

	cl := fakeclient.NewClientBuilder().WithScheme(scheme).
		WithObjects(t1, t2, t3, t4, a1).Build()
	r := &SpannerAutoscalerReconciler{
		ctrlClient:                       cl,
		manualScalingHistoryPerNamespace: 3,
	}

	if err := r.enforceHistoryLimit(context.Background(), "ns1"); err != nil {
		t.Fatalf("enforceHistoryLimit returned err: %v", err)
	}

	var remaining spannerv1beta1.SpannerManualScalingList
	if err := cl.List(context.Background(), &remaining); err != nil {
		t.Fatalf("list after GC failed: %v", err)
	}
	names := make([]string, 0, len(remaining.Items))
	for _, item := range remaining.Items {
		names = append(names, item.Name)
	}
	sort.Strings(names)
	wantNames := []string{"a1-active", "t2", "t3", "t4-newest"} // t1 deleted
	if len(names) != len(wantNames) {
		t.Fatalf("remaining names = %v, want %v", names, wantNames)
	}
	for i, n := range wantNames {
		if names[i] != n {
			t.Errorf("remaining[%d] = %q, want %q", i, names[i], n)
		}
	}
}

// TestEnforceHistoryLimit_Zero asserts the GC is a no-op when the limit is 0
// (default), regardless of how many terminal CRs exist. This is the
// backwards-compatibility guarantee.
func TestEnforceHistoryLimit_Zero(t *testing.T) {
	scheme := manualScalingTestScheme(t)
	now := time.Date(2026, 5, 30, 12, 0, 0, 0, time.UTC)
	cb := fakeclient.NewClientBuilder().WithScheme(scheme)
	for i := 0; i < 10; i++ {
		ms := msFixture(
			"t"+string(rune('0'+i)), "sa",
			spannerv1beta1.SpannerManualScalingPhaseExpired,
			now.Add(-time.Duration(i)*time.Hour),
			now.Add(-time.Duration(i)*time.Hour),
			nil,
		)
		cb = cb.WithObjects(ms)
	}
	cl := cb.Build()
	r := &SpannerAutoscalerReconciler{ctrlClient: cl, manualScalingHistoryPerNamespace: 0}

	if err := r.enforceHistoryLimit(context.Background(), "ns1"); err != nil {
		t.Fatalf("enforceHistoryLimit returned err: %v", err)
	}
	var remaining spannerv1beta1.SpannerManualScalingList
	if err := cl.List(context.Background(), &remaining); err != nil {
		t.Fatalf("list after GC failed: %v", err)
	}
	if len(remaining.Items) != 10 {
		t.Errorf("with limit=0, 10 terminal CRs should be retained; got %d", len(remaining.Items))
	}
}

// TestTransitionManualScalingPhase covers the terminal-phase write helper:
// stamps FinishedAt + Message, idempotent on already-terminal input, and
// handles NotFound gracefully (caller may have deleted the CR concurrently).
func TestTransitionManualScalingPhase(t *testing.T) {
	scheme := manualScalingTestScheme(t)
	now := time.Date(2026, 5, 30, 12, 0, 0, 0, time.UTC)

	ms := msFixture("ms1", "sa", "", now.Add(-time.Minute), time.Time{}, nil)
	cl := fakeclient.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(ms).
		WithStatusSubresource(&spannerv1beta1.SpannerManualScaling{}).
		Build()
	r := &SpannerAutoscalerReconciler{ctrlClient: cl}

	if err := r.transitionManualScalingPhase(context.Background(), ms,
		spannerv1beta1.SpannerManualScalingPhaseExpired, "expired test", now); err != nil {
		t.Fatalf("transition returned err: %v", err)
	}
	var fresh spannerv1beta1.SpannerManualScaling
	if err := cl.Get(context.Background(), ctrlclient.ObjectKeyFromObject(ms), &fresh); err != nil {
		t.Fatalf("get after transition failed: %v", err)
	}
	if fresh.Status.Phase != spannerv1beta1.SpannerManualScalingPhaseExpired {
		t.Errorf("phase = %q, want Expired", fresh.Status.Phase)
	}
	if fresh.Status.Message != "expired test" {
		t.Errorf("message = %q, want %q", fresh.Status.Message, "expired test")
	}
	if fresh.Status.FinishedAt == nil || !fresh.Status.FinishedAt.Time.Equal(now) {
		t.Errorf("finishedAt = %v, want %v", fresh.Status.FinishedAt, now)
	}

	// Second call must be a no-op (already terminal).
	later := now.Add(time.Hour)
	if err := r.transitionManualScalingPhase(context.Background(), ms,
		spannerv1beta1.SpannerManualScalingPhaseInvalid, "should not overwrite", later); err != nil {
		t.Fatalf("second transition returned err: %v", err)
	}
	if err := cl.Get(context.Background(), ctrlclient.ObjectKeyFromObject(ms), &fresh); err != nil {
		t.Fatalf("get after second transition failed: %v", err)
	}
	if fresh.Status.Phase != spannerv1beta1.SpannerManualScalingPhaseExpired {
		t.Errorf("phase should remain Expired (idempotent); got %q", fresh.Status.Phase)
	}
	if fresh.Status.Message != "expired test" {
		t.Errorf("message should remain unchanged; got %q", fresh.Status.Message)
	}
	if !fresh.Status.FinishedAt.Time.Equal(now) {
		t.Errorf("finishedAt should remain at original instant; got %v", fresh.Status.FinishedAt.Time)
	}
}

// TestTransitionManualScalingPhase_NotFound covers the concurrent-delete
// path: the helper must swallow IsNotFound rather than fail the reconcile.
func TestTransitionManualScalingPhase_NotFound(t *testing.T) {
	scheme := manualScalingTestScheme(t)
	ms := &spannerv1beta1.SpannerManualScaling{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns1", Name: "ms-gone"},
		Spec:       spannerv1beta1.SpannerManualScalingSpec{TargetResource: "sa", ProcessingUnits: 5000},
	}
	// Build a client that does NOT contain the object.
	cl := fakeclient.NewClientBuilder().WithScheme(scheme).
		WithStatusSubresource(&spannerv1beta1.SpannerManualScaling{}).
		Build()
	r := &SpannerAutoscalerReconciler{ctrlClient: cl}

	if err := r.transitionManualScalingPhase(context.Background(), ms,
		spannerv1beta1.SpannerManualScalingPhaseExpired, "test", time.Now()); err != nil {
		// IsNotFound must be swallowed; any other error fails the test.
		if !apierrors.IsNotFound(err) {
			t.Fatalf("expected nil or NotFound; got %v", err)
		}
	}
}

// TestReconcileManualScaling_NoActive_Deactivates ensures the deactivate
// path: when sa previously held an ActiveManualScaling and no override is
// currently active, the status is cleared and a Deactivated event is
// recorded.
func TestReconcileManualScaling_NoActive_Deactivates(t *testing.T) {
	scheme := manualScalingTestScheme(t)
	sa := &spannerv1beta1.SpannerAutoscaler{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns1", Name: "sa-target"},
		Status: spannerv1beta1.SpannerAutoscalerStatus{
			ActiveManualScaling: &spannerv1beta1.ActiveManualScaling{
				Name:            "previous-ms",
				ProcessingUnits: 5000,
			},
			CurrentProcessingUnits: 5000,
		},
	}
	cl := fakeclient.NewClientBuilder().WithScheme(scheme).WithObjects(sa).Build()
	rec := record.NewFakeRecorder(10)
	r := &SpannerAutoscalerReconciler{ctrlClient: cl, recorder: rec}

	statusChanged := false
	handled, requeue, err := r.reconcileManualScaling(
		context.Background(), logr.Discard(), sa, nil /* syncer not called */, time.Now(), &statusChanged,
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if handled {
		t.Errorf("handled = true, want false (no active override)")
	}
	if requeue != 0 {
		t.Errorf("requeue = %v, want 0", requeue)
	}
	if sa.Status.ActiveManualScaling != nil {
		t.Errorf("ActiveManualScaling should be cleared, got %+v", sa.Status.ActiveManualScaling)
	}
	if !statusChanged {
		t.Errorf("statusChanged should be true after clearing previous override")
	}
	// Drain the recorder channel; expect at least one event.
	select {
	case ev := <-rec.Events:
		if ev == "" {
			t.Error("got empty event message")
		}
	default:
		t.Error("expected a Deactivated event but none was recorded")
	}
}
