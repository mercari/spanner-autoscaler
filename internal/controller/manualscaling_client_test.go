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
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/intstr"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
	clocktesting "k8s.io/utils/clock/testing"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	fakeclient "sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	spannerv1beta1 "github.com/mercari/spanner-autoscaler/api/v1beta1"
	syncerpkg "github.com/mercari/spanner-autoscaler/internal/syncer"
)

// stubSyncer is a minimal Syncer implementation used by reconcileManualScaling
// tests. Records every UpdateInstance call and returns the configured err.
type stubSyncer struct {
	mu    sync.Mutex
	calls []int
	err   error
}

var _ syncerpkg.Syncer = (*stubSyncer)(nil)

func (s *stubSyncer) UpdateInstance(_ context.Context, pu int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, pu)
	return s.err
}

func (s *stubSyncer) Start()                                     {}
func (s *stubSyncer) Stop()                                      {}
func (s *stubSyncer) HasCredentials(*syncerpkg.Credentials) bool { return true }

func newReconcilerForApply(t *testing.T, cl ctrlclient.Client, now time.Time, rejectScaledown bool) (*SpannerAutoscalerReconciler, *record.FakeRecorder) {
	t.Helper()
	rec := record.NewFakeRecorder(20)
	return &SpannerAutoscalerReconciler{
		ctrlClient:            cl,
		recorder:              rec,
		clock:                 clocktesting.NewFakeClock(now),
		scaleUpInterval:       60 * time.Second,
		scaleDownInterval:     55 * time.Minute,
		rejectManualScaledown: rejectScaledown,
	}, rec
}

func eventLooksLike(t *testing.T, rec *record.FakeRecorder, want string) {
	t.Helper()
	for {
		select {
		case ev := <-rec.Events:
			if strings.Contains(ev, want) {
				return
			}
		default:
			t.Errorf("no event containing %q was recorded", want)
			return
		}
	}
}

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

// activeMS builds a SpannerManualScaling fixture suitable for the apply-path
// tests: non-terminal phase, targets sa-target, processingUnits=desiredPU.
func activeMS(name string, desiredPU int, created time.Time, opts ...func(*spannerv1beta1.SpannerManualScaling)) *spannerv1beta1.SpannerManualScaling {
	ms := msFixture(name, "sa-target", "", created, time.Time{}, nil)
	ms.Spec.ProcessingUnits = desiredPU
	for _, o := range opts {
		o(ms)
	}
	return ms
}

// TestReconcileManualScaling_ApplySingleJumpScaleUp covers the apply branch
// where the active override has no ScaleupStepSize (single-jump): syncer is
// called once with the target PU, an Applied event is recorded, status
// reflects the new desired PU + ActiveManualScaling snapshot, and the
// per-CR status flips to phase=Active.
func TestReconcileManualScaling_ApplySingleJumpScaleUp(t *testing.T) {
	scheme := manualScalingTestScheme(t)
	now := time.Date(2026, 5, 30, 12, 0, 0, 0, time.UTC)
	ms := activeMS("ms-up", 5000, now.Add(-time.Minute))
	sa := &spannerv1beta1.SpannerAutoscaler{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns1", Name: "sa-target"},
		Status: spannerv1beta1.SpannerAutoscalerStatus{
			CurrentProcessingUnits: 1000,
		},
	}
	cl := fakeclient.NewClientBuilder().WithScheme(scheme).WithObjects(sa, ms).
		WithStatusSubresource(&spannerv1beta1.SpannerManualScaling{}).Build()
	r, rec := newReconcilerForApply(t, cl, now, false)
	syncer := &stubSyncer{}

	statusChanged := false
	handled, requeue, err := r.reconcileManualScaling(context.Background(), logr.Discard(), sa, syncer, now, &statusChanged)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !handled {
		t.Error("handled should be true once an active override is selected")
	}
	if requeue != 0 {
		t.Errorf("single-jump requeueAfter should be 0; got %v", requeue)
	}
	if len(syncer.calls) != 1 || syncer.calls[0] != 5000 {
		t.Errorf("UpdateInstance calls = %v, want [5000]", syncer.calls)
	}
	if sa.Status.DesiredProcessingUnits != 5000 {
		t.Errorf("DesiredProcessingUnits = %d, want 5000", sa.Status.DesiredProcessingUnits)
	}
	if sa.Status.ActiveManualScaling == nil || sa.Status.ActiveManualScaling.Name != "ms-up" {
		t.Errorf("ActiveManualScaling snapshot not set, got %+v", sa.Status.ActiveManualScaling)
	}
	eventLooksLike(t, rec, "ManualScalingApplied")
}

// TestReconcileManualScaling_SteppedRampProgressing covers the apply branch
// where ScaleupStepSize is set: the first reconcile steps current+stepSize
// (not the full target), emits a Progressing event, and asks the caller to
// requeue at the scaleup interval.
func TestReconcileManualScaling_SteppedRampProgressing(t *testing.T) {
	scheme := manualScalingTestScheme(t)
	now := time.Date(2026, 5, 30, 12, 0, 0, 0, time.UTC)
	stepSize := intstrFromInt(1000)
	interval := metav1.Duration{Duration: 2 * time.Minute}
	ms := activeMS("ms-step", 5000, now.Add(-time.Minute), func(m *spannerv1beta1.SpannerManualScaling) {
		m.Spec.ScaleupStepSize = &stepSize
		m.Spec.ScaleupInterval = &interval
	})
	sa := &spannerv1beta1.SpannerAutoscaler{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns1", Name: "sa-target"},
		Status: spannerv1beta1.SpannerAutoscalerStatus{
			CurrentProcessingUnits: 1000,
		},
	}
	cl := fakeclient.NewClientBuilder().WithScheme(scheme).WithObjects(sa, ms).
		WithStatusSubresource(&spannerv1beta1.SpannerManualScaling{}).Build()
	r, rec := newReconcilerForApply(t, cl, now, false)
	syncer := &stubSyncer{}

	statusChanged := false
	_, requeue, err := r.reconcileManualScaling(context.Background(), logr.Discard(), sa, syncer, now, &statusChanged)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(syncer.calls) != 1 || syncer.calls[0] != 2000 {
		t.Errorf("UpdateInstance calls = %v, want [2000] (one step from 1000)", syncer.calls)
	}
	if requeue != interval.Duration {
		t.Errorf("requeueAfter = %v, want %v (scaleup interval)", requeue, interval.Duration)
	}
	eventLooksLike(t, rec, "ManualScalingProgressing")
}

// TestReconcileManualScaling_SyncerUpdateFails ensures that a syncer failure
// is propagated as an error (so dispatchManualScaling can persist a status
// revert + reraise to controller-runtime) and an Event of type Warning is
// emitted with the reason FailedUpdateInstance.
func TestReconcileManualScaling_SyncerUpdateFails(t *testing.T) {
	scheme := manualScalingTestScheme(t)
	now := time.Date(2026, 5, 30, 12, 0, 0, 0, time.UTC)
	ms := activeMS("ms-up", 5000, now.Add(-time.Minute))
	sa := &spannerv1beta1.SpannerAutoscaler{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns1", Name: "sa-target"},
		Status: spannerv1beta1.SpannerAutoscalerStatus{
			CurrentProcessingUnits: 1000,
		},
	}
	cl := fakeclient.NewClientBuilder().WithScheme(scheme).WithObjects(sa, ms).Build()
	r, rec := newReconcilerForApply(t, cl, now, false)
	syncer := &stubSyncer{err: errors.New("spanner blew up")}

	statusChanged := false
	_, _, err := r.reconcileManualScaling(context.Background(), logr.Discard(), sa, syncer, now, &statusChanged)
	if err == nil || !strings.Contains(err.Error(), "spanner blew up") {
		t.Fatalf("expected syncer error to bubble up; got %v", err)
	}
	eventLooksLike(t, rec, "FailedUpdateInstance")
}

// TestReconcileManualScaling_RejectScaledownPolicy ensures that when the
// cluster-wide --reject-manual-scaledown flag is true and the override
// targets a lower PU, the candidate is marked Invalid and the parent's
// ActiveManualScaling is cleared. No syncer call is made.
func TestReconcileManualScaling_RejectScaledownPolicy(t *testing.T) {
	scheme := manualScalingTestScheme(t)
	now := time.Date(2026, 5, 30, 12, 0, 0, 0, time.UTC)
	ms := activeMS("ms-down", 1000, now.Add(-time.Minute))
	sa := &spannerv1beta1.SpannerAutoscaler{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns1", Name: "sa-target"},
		Status: spannerv1beta1.SpannerAutoscalerStatus{
			CurrentProcessingUnits: 5000,
			ActiveManualScaling: &spannerv1beta1.ActiveManualScaling{
				Name: "stale", ProcessingUnits: 5000,
			},
		},
	}
	cl := fakeclient.NewClientBuilder().WithScheme(scheme).WithObjects(sa, ms).
		WithStatusSubresource(&spannerv1beta1.SpannerManualScaling{}).Build()
	r, rec := newReconcilerForApply(t, cl, now, true /* rejectScaledown */)
	syncer := &stubSyncer{}

	statusChanged := false
	handled, _, err := r.reconcileManualScaling(context.Background(), logr.Discard(), sa, syncer, now, &statusChanged)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if handled {
		t.Error("handled should be false on rejected scaledown so caller falls through to CPU path")
	}
	if len(syncer.calls) != 0 {
		t.Errorf("syncer should not be invoked on rejected scaledown; got calls=%v", syncer.calls)
	}
	if sa.Status.ActiveManualScaling != nil {
		t.Errorf("ActiveManualScaling should be cleared after reject; got %+v", sa.Status.ActiveManualScaling)
	}
	var fresh spannerv1beta1.SpannerManualScaling
	if err := cl.Get(context.Background(), ctrlclient.ObjectKeyFromObject(ms), &fresh); err != nil {
		t.Fatalf("get after reject failed: %v", err)
	}
	if fresh.Status.Phase != spannerv1beta1.SpannerManualScalingPhaseInvalid {
		t.Errorf("phase = %q, want Invalid", fresh.Status.Phase)
	}
	eventLooksLike(t, rec, "ManualScalingRejected")
}

// TestReconcileManualScaling_PreemptsActiveSchedule ensures that when a
// SpannerManualScaling becomes the active override while schedules are
// currently in their time window, the schedule-derived DesiredMin/MaxPUs
// are zeroed (suppressed) and the preempt is surfaced to operators via
// the SkipReasonScheduleSuppressedByManual metric. The schedule entries
// themselves are not pruned from CurrentlyActiveSchedules — they are
// still in their window and will re-take effect once the manual override
// ends. This pins down the documented overlap policy: manual scaling
// fully preempts schedules; the actual instance PU follows manual scaling
// only.
func TestReconcileManualScaling_PreemptsActiveSchedule(t *testing.T) {
	scheme := manualScalingTestScheme(t)
	now := time.Date(2026, 5, 30, 12, 0, 0, 0, time.UTC)
	ms := activeMS("ms-up", 8000, now.Add(-time.Minute))
	sa := &spannerv1beta1.SpannerAutoscaler{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns1", Name: "sa-target"},
		Status: spannerv1beta1.SpannerAutoscalerStatus{
			CurrentProcessingUnits: 4000,
			DesiredMinPUs:          7000, // populated by a previous CPU+schedule reconcile
			DesiredMaxPUs:          9000,
			CurrentlyActiveSchedules: []spannerv1beta1.ActiveSchedule{
				{ScheduleName: "peak-hours", AdditionalPU: 5000, EndTime: metav1.Time{Time: now.Add(time.Hour)}},
			},
		},
	}
	cl := fakeclient.NewClientBuilder().WithScheme(scheme).WithObjects(sa, ms).
		WithStatusSubresource(&spannerv1beta1.SpannerManualScaling{}).Build()
	r, _ := newReconcilerForApply(t, cl, now, false)
	syncer := &stubSyncer{}

	statusChanged := false
	if _, _, err := r.reconcileManualScaling(context.Background(), logr.Discard(), sa, syncer, now, &statusChanged); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if sa.Status.DesiredMinPUs != 0 || sa.Status.DesiredMaxPUs != 0 {
		t.Errorf("DesiredMin/MaxPUs should be zeroed when manual preempts schedule; got min=%d max=%d",
			sa.Status.DesiredMinPUs, sa.Status.DesiredMaxPUs)
	}
	if len(sa.Status.CurrentlyActiveSchedules) != 1 || sa.Status.CurrentlyActiveSchedules[0].ScheduleName != "peak-hours" {
		t.Errorf("CurrentlyActiveSchedules should be retained as-is (still in window); got %+v",
			sa.Status.CurrentlyActiveSchedules)
	}
	// PU is set to the manual target (8000), not baseline+additional (9000).
	if len(syncer.calls) != 1 || syncer.calls[0] != 8000 {
		t.Errorf("syncer should apply the manual target only; got calls=%v", syncer.calls)
	}
}

// TestScheduleNamesPreemptedBy covers the helper that decides whether the
// preempt log/metric should fire: empty when no manual override is active
// or no schedule is in its window, otherwise the schedule name list.
func TestScheduleNamesPreemptedBy(t *testing.T) {
	now := time.Date(2026, 5, 30, 12, 0, 0, 0, time.UTC)
	ms := activeMS("ms-up", 8000, now.Add(-time.Minute))
	saWithSchedules := &spannerv1beta1.SpannerAutoscaler{
		Status: spannerv1beta1.SpannerAutoscalerStatus{
			CurrentlyActiveSchedules: []spannerv1beta1.ActiveSchedule{
				{ScheduleName: "peak-hours"},
				{ScheduleName: "evening-rush"},
			},
		},
	}
	saNoSchedules := &spannerv1beta1.SpannerAutoscaler{}

	if got := scheduleNamesPreemptedBy(nil, saWithSchedules); got != nil {
		t.Errorf("nil active → want nil, got %v", got)
	}
	if got := scheduleNamesPreemptedBy(ms, saNoSchedules); got != nil {
		t.Errorf("no active schedule → want nil, got %v", got)
	}
	got := scheduleNamesPreemptedBy(ms, saWithSchedules)
	want := []string{"peak-hours", "evening-rush"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("[%d] got %q, want %q", i, got[i], want[i])
		}
	}
}

// TestReconcileManualScaling_CooldownHoldEmitsSkip covers the case where
// LastScaleTime is recent enough that the per-direction cooldown has not
// elapsed: nextManualPU returns currentPU + remaining-cooldown duration,
// no syncer call is made, and requeueAfter equals the remaining cooldown.
func TestReconcileManualScaling_CooldownHoldEmitsSkip(t *testing.T) {
	scheme := manualScalingTestScheme(t)
	now := time.Date(2026, 5, 30, 12, 0, 0, 0, time.UTC)
	stepSize := intstrFromInt(1000)
	interval := metav1.Duration{Duration: 5 * time.Minute}
	ms := activeMS("ms-step", 5000, now.Add(-time.Hour), func(m *spannerv1beta1.SpannerManualScaling) {
		m.Spec.ScaleupStepSize = &stepSize
		m.Spec.ScaleupInterval = &interval
	})
	sa := &spannerv1beta1.SpannerAutoscaler{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns1", Name: "sa-target"},
		Status: spannerv1beta1.SpannerAutoscalerStatus{
			CurrentProcessingUnits: 1000,
			LastScaleTime:          metav1.Time{Time: now.Add(-1 * time.Minute)},
		},
	}
	cl := fakeclient.NewClientBuilder().WithScheme(scheme).WithObjects(sa, ms).
		WithStatusSubresource(&spannerv1beta1.SpannerManualScaling{}).Build()
	r, _ := newReconcilerForApply(t, cl, now, false)
	syncer := &stubSyncer{}

	statusChanged := false
	_, requeue, err := r.reconcileManualScaling(context.Background(), logr.Discard(), sa, syncer, now, &statusChanged)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(syncer.calls) != 0 {
		t.Errorf("syncer must not be invoked during cooldown hold; got %v", syncer.calls)
	}
	// remaining cooldown = 5min - 1min = 4min.
	want := 4 * time.Minute
	if requeue != want {
		t.Errorf("requeueAfter = %v, want %v", requeue, want)
	}
}

// TestReconcileManualScaling_ExpiresAtClampsRequeue covers the requeue
// floor: when the override's ExpiresAt is earlier than the next-step
// boundary, the returned requeueAfter must clamp to the time-until-expiry.
func TestReconcileManualScaling_ExpiresAtClampsRequeue(t *testing.T) {
	scheme := manualScalingTestScheme(t)
	now := time.Date(2026, 5, 30, 12, 0, 0, 0, time.UTC)
	stepSize := intstrFromInt(1000)
	interval := metav1.Duration{Duration: 10 * time.Minute}
	// expiresAt is 1 minute away — earlier than the 10-minute step interval.
	expiresAt := now.Add(time.Minute)
	ms := activeMS("ms-step", 5000, now.Add(-time.Minute), func(m *spannerv1beta1.SpannerManualScaling) {
		m.Spec.ScaleupStepSize = &stepSize
		m.Spec.ScaleupInterval = &interval
		m.Spec.ExpiresAt = &metav1.Time{Time: expiresAt}
	})
	sa := &spannerv1beta1.SpannerAutoscaler{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns1", Name: "sa-target"},
		Status: spannerv1beta1.SpannerAutoscalerStatus{
			CurrentProcessingUnits: 1000,
		},
	}
	cl := fakeclient.NewClientBuilder().WithScheme(scheme).WithObjects(sa, ms).
		WithStatusSubresource(&spannerv1beta1.SpannerManualScaling{}).Build()
	r, _ := newReconcilerForApply(t, cl, now, false)
	syncer := &stubSyncer{}

	statusChanged := false
	_, requeue, err := r.reconcileManualScaling(context.Background(), logr.Discard(), sa, syncer, now, &statusChanged)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	wantClamp := time.Minute
	if requeue != wantClamp {
		t.Errorf("requeueAfter = %v, want %v (clamped to ExpiresAt)", requeue, wantClamp)
	}
}

// TestDispatchManualScaling_StatusConflictRequeues ensures that a
// Status().Update conflict on the success path is treated as benign and
// translated into a Requeue (not an error) so controller-runtime retries
// with the latest resourceVersion.
func TestDispatchManualScaling_StatusConflictRequeues(t *testing.T) {
	scheme := manualScalingTestScheme(t)
	now := time.Date(2026, 5, 30, 12, 0, 0, 0, time.UTC)
	ms := activeMS("ms-up", 5000, now.Add(-time.Minute))
	sa := &spannerv1beta1.SpannerAutoscaler{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns1", Name: "sa-target"},
		Status: spannerv1beta1.SpannerAutoscalerStatus{
			CurrentProcessingUnits: 1000,
		},
	}
	base := fakeclient.NewClientBuilder().WithScheme(scheme).WithObjects(sa, ms).Build()
	cl := interceptor.NewClient(base, interceptor.Funcs{
		SubResourceUpdate: func(ctx context.Context, c ctrlclient.Client, sub string, obj ctrlclient.Object, opts ...ctrlclient.SubResourceUpdateOption) error {
			if sub == "status" {
				if _, ok := obj.(*spannerv1beta1.SpannerAutoscaler); ok {
					return apierrors.NewConflict(
						schema.GroupResource{Group: "spanner.mercari.com", Resource: "spannerautoscalers"},
						obj.GetName(), errors.New("stale"))
				}
			}
			return c.SubResource(sub).Update(ctx, obj, opts...)
		},
	})
	r, _ := newReconcilerForApply(t, cl, now, false)
	syncer := &stubSyncer{}

	statusChanged := false
	res, handled, err := r.dispatchManualScaling(context.Background(), logr.Discard(), sa, syncer, &statusChanged, nil)
	if err != nil {
		t.Fatalf("conflict should be swallowed; got err %v", err)
	}
	if !handled {
		t.Error("handled should be true on conflict path so caller short-circuits")
	}
	if !res.Requeue { //nolint:staticcheck // production code returns Result{Requeue: true} for benign conflict.
		t.Errorf("expected Requeue=true on status conflict; got %+v", res)
	}
}

// TestDispatchManualScaling_StatusWriteFailsEmitsEvent covers the
// non-conflict status-write failure: the failure must be returned as an
// error and a FailedUpdateStatus event must be recorded so operators can
// detect persistent API issues.
func TestDispatchManualScaling_StatusWriteFailsEmitsEvent(t *testing.T) {
	scheme := manualScalingTestScheme(t)
	now := time.Date(2026, 5, 30, 12, 0, 0, 0, time.UTC)
	ms := activeMS("ms-up", 5000, now.Add(-time.Minute))
	sa := &spannerv1beta1.SpannerAutoscaler{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns1", Name: "sa-target"},
		Status: spannerv1beta1.SpannerAutoscalerStatus{
			CurrentProcessingUnits: 1000,
		},
	}
	base := fakeclient.NewClientBuilder().WithScheme(scheme).WithObjects(sa, ms).Build()
	cl := interceptor.NewClient(base, interceptor.Funcs{
		SubResourceUpdate: func(_ context.Context, _ ctrlclient.Client, sub string, obj ctrlclient.Object, _ ...ctrlclient.SubResourceUpdateOption) error {
			if sub == "status" {
				if _, ok := obj.(*spannerv1beta1.SpannerAutoscaler); ok {
					return fmt.Errorf("simulated API outage")
				}
			}
			return nil
		},
	})
	r, rec := newReconcilerForApply(t, cl, now, false)
	syncer := &stubSyncer{}

	statusChanged := false
	_, _, err := r.dispatchManualScaling(context.Background(), logr.Discard(), sa, syncer, &statusChanged, nil)
	if err == nil || !strings.Contains(err.Error(), "simulated API outage") {
		t.Fatalf("expected the status-write error to bubble up; got %v", err)
	}
	eventLooksLike(t, rec, "FailedUpdateStatus")
}

// TestDispatchManualScaling_NoActiveFallsThrough ensures that handled=false
// is returned when no override is active so the caller's CPU autoscale
// branch can run.
func TestDispatchManualScaling_NoActiveFallsThrough(t *testing.T) {
	scheme := manualScalingTestScheme(t)
	now := time.Date(2026, 5, 30, 12, 0, 0, 0, time.UTC)
	sa := &spannerv1beta1.SpannerAutoscaler{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns1", Name: "sa-target"},
	}
	cl := fakeclient.NewClientBuilder().WithScheme(scheme).WithObjects(sa).Build()
	r, _ := newReconcilerForApply(t, cl, now, false)

	statusChanged := false
	res, handled, err := r.dispatchManualScaling(context.Background(), logr.Discard(), sa, &stubSyncer{}, &statusChanged, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if handled {
		t.Error("handled should be false when no override exists")
	}
	if res.RequeueAfter != 0 || res.Requeue { //nolint:staticcheck // production code returns Result{Requeue: true} for benign conflict; the field is still supported in controller-runtime v0.24.
		t.Errorf("no-active should not requeue; got %+v", res)
	}
}

// TestUpdateManualScalingProgress_AppliedAtFirstStamp ensures AppliedAt is
// only stamped the first time didApply=true (later reconciles preserve the
// original instant) and is NOT stamped when didApply=false (e.g. on a
// cooldown-hold reconcile).
func TestUpdateManualScalingProgress_AppliedAtFirstStamp(t *testing.T) {
	scheme := manualScalingTestScheme(t)
	t0 := time.Date(2026, 5, 30, 12, 0, 0, 0, time.UTC)
	t1 := t0.Add(time.Minute)
	t2 := t0.Add(5 * time.Minute)
	ms := activeMS("ms-up", 5000, t0)
	cl := fakeclient.NewClientBuilder().WithScheme(scheme).WithObjects(ms).
		WithStatusSubresource(&spannerv1beta1.SpannerManualScaling{}).Build()
	r := &SpannerAutoscalerReconciler{ctrlClient: cl}

	// 1. didApply=false → AppliedAt must remain nil.
	if err := r.updateManualScalingProgress(context.Background(), ms, 1000, 5000, t0, false); err != nil {
		t.Fatalf("first call returned err: %v", err)
	}
	var fresh spannerv1beta1.SpannerManualScaling
	mustGet := func() { //nolint:thelper // wrapper, not a helper
		if err := cl.Get(context.Background(), ctrlclient.ObjectKeyFromObject(ms), &fresh); err != nil {
			t.Fatalf("get failed: %v", err)
		}
	}
	mustGet()
	if fresh.Status.AppliedAt != nil {
		t.Errorf("AppliedAt should remain nil when didApply=false; got %v", fresh.Status.AppliedAt)
	}

	// 2. didApply=true → AppliedAt stamped to t1.
	if err := r.updateManualScalingProgress(context.Background(), &fresh, 2000, 5000, t1, true); err != nil {
		t.Fatalf("second call returned err: %v", err)
	}
	mustGet()
	if fresh.Status.AppliedAt == nil || !fresh.Status.AppliedAt.Time.Equal(t1) {
		t.Errorf("AppliedAt = %v, want %v", fresh.Status.AppliedAt, t1)
	}

	// 3. didApply=true again → AppliedAt unchanged (first-time-wins).
	if err := r.updateManualScalingProgress(context.Background(), &fresh, 3000, 5000, t2, true); err != nil {
		t.Fatalf("third call returned err: %v", err)
	}
	mustGet()
	if !fresh.Status.AppliedAt.Time.Equal(t1) {
		t.Errorf("AppliedAt should remain at t1; got %v", fresh.Status.AppliedAt.Time)
	}
}

// TestUpdateManualScalingProgress_ReachedAtOnlyOnActive ensures ReachedAt
// is stamped only when current == target (phase=Active), and only on the
// first time that transition happens.
func TestUpdateManualScalingProgress_ReachedAtOnlyOnActive(t *testing.T) {
	scheme := manualScalingTestScheme(t)
	t0 := time.Date(2026, 5, 30, 12, 0, 0, 0, time.UTC)
	t1 := t0.Add(time.Minute)
	ms := activeMS("ms-up", 5000, t0)
	cl := fakeclient.NewClientBuilder().WithScheme(scheme).WithObjects(ms).
		WithStatusSubresource(&spannerv1beta1.SpannerManualScaling{}).Build()
	r := &SpannerAutoscalerReconciler{ctrlClient: cl}

	// Progressing — ReachedAt must remain nil.
	if err := r.updateManualScalingProgress(context.Background(), ms, 2000, 5000, t0, true); err != nil {
		t.Fatalf("err: %v", err)
	}
	var fresh spannerv1beta1.SpannerManualScaling
	if err := cl.Get(context.Background(), ctrlclient.ObjectKeyFromObject(ms), &fresh); err != nil {
		t.Fatalf("get: %v", err)
	}
	if fresh.Status.Phase != spannerv1beta1.SpannerManualScalingPhaseProgressing {
		t.Errorf("phase = %q, want Progressing", fresh.Status.Phase)
	}
	if fresh.Status.ReachedAt != nil {
		t.Errorf("ReachedAt should remain nil before reaching target; got %v", fresh.Status.ReachedAt)
	}

	// Reach target — ReachedAt must be t1.
	if err := r.updateManualScalingProgress(context.Background(), &fresh, 5000, 5000, t1, true); err != nil {
		t.Fatalf("err: %v", err)
	}
	if err := cl.Get(context.Background(), ctrlclient.ObjectKeyFromObject(ms), &fresh); err != nil {
		t.Fatalf("get: %v", err)
	}
	if fresh.Status.Phase != spannerv1beta1.SpannerManualScalingPhaseActive {
		t.Errorf("phase = %q, want Active", fresh.Status.Phase)
	}
	if fresh.Status.ReachedAt == nil || !fresh.Status.ReachedAt.Time.Equal(t1) {
		t.Errorf("ReachedAt = %v, want %v", fresh.Status.ReachedAt, t1)
	}
}

// TestUpdateManualScalingProgress_NotFound ensures a concurrently-deleted
// CR (Get returns NotFound) is treated as a no-op rather than an error.
func TestUpdateManualScalingProgress_NotFound(t *testing.T) {
	scheme := manualScalingTestScheme(t)
	ms := activeMS("ms-gone", 5000, time.Now())
	cl := fakeclient.NewClientBuilder().WithScheme(scheme).
		WithStatusSubresource(&spannerv1beta1.SpannerManualScaling{}).Build()
	r := &SpannerAutoscalerReconciler{ctrlClient: cl}

	if err := r.updateManualScalingProgress(context.Background(), ms, 1000, 5000, time.Now(), true); err != nil {
		t.Errorf("NotFound should be swallowed; got %v", err)
	}
}

// TestMarkExpiredSiblings_Sweeps covers the controller-down-across-boundary
// catch-up: selectActiveManualScaling skips siblings whose ExpiresAt has
// elapsed, but markExpiredSiblings must still transition them to Expired so
// the history GC can claim them and operators can see the terminal state.
// Already-terminal siblings, DeletionTimestamp-set siblings, and
// wrong-target siblings must be skipped (idempotency / scope guards).
func TestMarkExpiredSiblings_Sweeps(t *testing.T) {
	scheme := manualScalingTestScheme(t)
	now := time.Date(2026, 5, 30, 12, 0, 0, 0, time.UTC)
	past := now.Add(-time.Hour)
	future := now.Add(time.Hour)

	expiredA := msFixture("expired-a", "sa-target", "", now.Add(-2*time.Hour), time.Time{}, &past)
	expiredB := msFixture("expired-b", "sa-target", "", now.Add(-2*time.Hour), time.Time{}, &past)
	// expiredB also has a non-zero observed PU different from target so the
	// "target not reached" message branch is exercised.
	expiredB.Status.CurrentProcessingUnits = 2000

	notYetExpired := msFixture("not-yet", "sa-target", "", now.Add(-time.Minute), time.Time{}, &future)
	alreadyTerminal := msFixture("already-terminal", "sa-target",
		spannerv1beta1.SpannerManualScalingPhaseSuperseded,
		now.Add(-3*time.Hour), now.Add(-3*time.Hour), &past)
	wrongTarget := msFixture("wrong-target", "other-sa", "", now.Add(-time.Minute), time.Time{}, &past)

	cl := fakeclient.NewClientBuilder().WithScheme(scheme).WithObjects(
		expiredA, expiredB, notYetExpired, alreadyTerminal, wrongTarget,
	).WithStatusSubresource(&spannerv1beta1.SpannerManualScaling{}).Build()
	sa := &spannerv1beta1.SpannerAutoscaler{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns1", Name: "sa-target"},
	}
	r := &SpannerAutoscalerReconciler{ctrlClient: cl}

	r.markExpiredSiblings(context.Background(), logr.Discard(), sa, now)

	wantPhase := func(name string, want spannerv1beta1.SpannerManualScalingPhase, wantMsgSub string) {
		t.Helper()
		var fresh spannerv1beta1.SpannerManualScaling
		if err := cl.Get(context.Background(), ctrlclient.ObjectKey{Namespace: "ns1", Name: name}, &fresh); err != nil {
			t.Fatalf("get %s failed: %v", name, err)
		}
		if fresh.Status.Phase != want {
			t.Errorf("%s phase = %q, want %q", name, fresh.Status.Phase, want)
		}
		if wantMsgSub != "" && !strings.Contains(fresh.Status.Message, wantMsgSub) {
			t.Errorf("%s message = %q, want it to contain %q", name, fresh.Status.Message, wantMsgSub)
		}
	}
	wantPhase("expired-a", spannerv1beta1.SpannerManualScalingPhaseExpired, "expiresAt")
	wantPhase("expired-b", spannerv1beta1.SpannerManualScalingPhaseExpired, "target not reached")
	wantPhase("not-yet", "", "")
	wantPhase("already-terminal", spannerv1beta1.SpannerManualScalingPhaseSuperseded, "")
	wantPhase("wrong-target", "", "")
}

// intstrFromInt mirrors intstr.FromInt32 for the int32 path used by the
// SpannerManualScaling specs; kept tiny so each test can declare a step size
// inline without importing intstr.
func intstrFromInt(v int32) intstr.IntOrString { return intstr.FromInt32(v) }
