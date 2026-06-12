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
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	clocktesting "k8s.io/utils/clock/testing"
	ctrl "sigs.k8s.io/controller-runtime"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	fakeclient "sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	spannerv1beta1 "github.com/mercari/spanner-autoscaler/api/v1beta1"
)

func newSMSR(t *testing.T, cl ctrlclient.Client, now time.Time) (*SpannerManualScalingReconciler, *record.FakeRecorder) {
	t.Helper()
	rec := record.NewFakeRecorder(10)
	return &SpannerManualScalingReconciler{
		ctrlClient: cl,
		scheme:     manualScalingTestScheme(t),
		recorder:   rec,
		clock:      clocktesting.NewFakeClock(now),
		log:        logr.Discard(),
	}, rec
}

func saFixture(name string, uid types.UID) *spannerv1beta1.SpannerAutoscaler {
	return &spannerv1beta1.SpannerAutoscaler{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "ns1",
			Name:      name,
			UID:       uid,
		},
	}
}

// TestSpannerManualScalingReconciler_Orphan covers the case where the target
// SpannerAutoscaler does not exist: the resource is transitioned to phase
// Invalid with FinishedAt + Message set, and an Invalid event is emitted.
func TestSpannerManualScalingReconciler_Orphan(t *testing.T) {
	scheme := manualScalingTestScheme(t)
	now := time.Date(2026, 5, 30, 12, 0, 0, 0, time.UTC)

	ms := msFixture("ms1", "no-such-sa", "", now.Add(-time.Minute), time.Time{}, nil)
	cl := fakeclient.NewClientBuilder().WithScheme(scheme).WithObjects(ms).
		WithStatusSubresource(&spannerv1beta1.SpannerManualScaling{}).Build()
	r, rec := newSMSR(t, cl, now)

	res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{
		Namespace: "ns1", Name: "ms1",
	}})
	if err != nil {
		t.Fatalf("Reconcile returned err: %v", err)
	}
	if res.Requeue || res.RequeueAfter != 0 { //nolint:staticcheck // production code returns Result{Requeue: true} for benign conflict; the field is still supported in controller-runtime v0.24.
		t.Errorf("orphan should not requeue; got %+v", res)
	}

	var fresh spannerv1beta1.SpannerManualScaling
	if err := cl.Get(context.Background(), ctrlclient.ObjectKeyFromObject(ms), &fresh); err != nil {
		t.Fatalf("get after Reconcile failed: %v", err)
	}
	if fresh.Status.Phase != spannerv1beta1.SpannerManualScalingPhaseInvalid {
		t.Errorf("phase = %q, want Invalid", fresh.Status.Phase)
	}
	if fresh.Status.FinishedAt == nil || !fresh.Status.FinishedAt.Time.Equal(now) {
		t.Errorf("finishedAt = %v, want %v", fresh.Status.FinishedAt, now)
	}
	if !strings.Contains(fresh.Status.Message, "targetResource") {
		t.Errorf("message should mention targetResource; got %q", fresh.Status.Message)
	}

	select {
	case ev := <-rec.Events:
		if !strings.Contains(ev, "Invalid") {
			t.Errorf("event = %q, want one containing %q", ev, "Invalid")
		}
	default:
		t.Error("expected an Invalid event but none was recorded")
	}
}

// TestSpannerManualScalingReconciler_OwnerRefFirstTime covers the happy path
// where the target exists but the resource has no controller-typed
// ownerReference yet: the reconciler sets one and persists it.
func TestSpannerManualScalingReconciler_OwnerRefFirstTime(t *testing.T) {
	scheme := manualScalingTestScheme(t)
	now := time.Date(2026, 5, 30, 12, 0, 0, 0, time.UTC)
	target := saFixture("sa-target", "uid-target")
	ms := msFixture("ms1", "sa-target", "", now.Add(-time.Minute), time.Time{}, nil)

	cl := fakeclient.NewClientBuilder().WithScheme(scheme).WithObjects(target, ms).Build()
	r, _ := newSMSR(t, cl, now)

	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{
		Namespace: "ns1", Name: "ms1",
	}}); err != nil {
		t.Fatalf("Reconcile returned err: %v", err)
	}

	var fresh spannerv1beta1.SpannerManualScaling
	if err := cl.Get(context.Background(), ctrlclient.ObjectKeyFromObject(ms), &fresh); err != nil {
		t.Fatalf("get after Reconcile failed: %v", err)
	}
	ref := metav1.GetControllerOf(&fresh)
	if ref == nil {
		t.Fatal("expected controller-typed ownerReference; got nil")
		return // unreachable; satisfies staticcheck SA5011
	}
	if ref.UID != target.UID {
		t.Errorf("ownerRef.UID = %q, want %q", ref.UID, target.UID)
	}
	if ref.Name != target.Name {
		t.Errorf("ownerRef.Name = %q, want %q", ref.Name, target.Name)
	}
}

// TestSpannerManualScalingReconciler_OwnerRefAlreadyCorrect ensures the
// reconciler does NOT call Update when the controller-typed ownerReference
// already points at the target's current UID — keeping reconciles cheap on
// the steady-state path.
func TestSpannerManualScalingReconciler_OwnerRefAlreadyCorrect(t *testing.T) {
	scheme := manualScalingTestScheme(t)
	now := time.Date(2026, 5, 30, 12, 0, 0, 0, time.UTC)
	target := saFixture("sa-target", "uid-target")
	ms := msFixture("ms1", "sa-target", "", now.Add(-time.Minute), time.Time{}, nil)
	isTrue := true
	ms.OwnerReferences = []metav1.OwnerReference{{
		APIVersion: spannerv1beta1.GroupVersion.String(),
		Kind:       "SpannerAutoscaler",
		Name:       target.Name,
		UID:        target.UID,
		Controller: &isTrue,
	}}

	var updateCount int
	base := fakeclient.NewClientBuilder().WithScheme(scheme).WithObjects(target, ms).Build()
	cl := interceptor.NewClient(base, interceptor.Funcs{
		Update: func(ctx context.Context, c ctrlclient.WithWatch, obj ctrlclient.Object, opts ...ctrlclient.UpdateOption) error {
			updateCount++
			return c.Update(ctx, obj, opts...)
		},
	})
	r, _ := newSMSR(t, cl, now)

	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{
		Namespace: "ns1", Name: "ms1",
	}}); err != nil {
		t.Fatalf("Reconcile returned err: %v", err)
	}
	if updateCount != 0 {
		t.Errorf("Update should not be called when ownerRef already correct; got %d calls", updateCount)
	}
}

// TestSpannerManualScalingReconciler_OwnerRefStaleUID covers the case where
// the existing controller-typed ownerReference points at the same SA name
// but a stale UID (typical after the parent SA was deleted and recreated):
// the reconciler must re-set the ownerReference to the new UID.
func TestSpannerManualScalingReconciler_OwnerRefStaleUID(t *testing.T) {
	scheme := manualScalingTestScheme(t)
	now := time.Date(2026, 5, 30, 12, 0, 0, 0, time.UTC)
	target := saFixture("sa-target", "uid-new")
	ms := msFixture("ms1", "sa-target", "", now.Add(-time.Minute), time.Time{}, nil)
	isTrue := true
	ms.OwnerReferences = []metav1.OwnerReference{{
		APIVersion: spannerv1beta1.GroupVersion.String(),
		Kind:       "SpannerAutoscaler",
		Name:       target.Name,
		UID:        "uid-stale",
		Controller: &isTrue,
	}}
	cl := fakeclient.NewClientBuilder().WithScheme(scheme).WithObjects(target, ms).Build()
	r, _ := newSMSR(t, cl, now)

	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{
		Namespace: "ns1", Name: "ms1",
	}}); err != nil {
		t.Fatalf("Reconcile returned err: %v", err)
	}

	var fresh spannerv1beta1.SpannerManualScaling
	if err := cl.Get(context.Background(), ctrlclient.ObjectKeyFromObject(ms), &fresh); err != nil {
		t.Fatalf("get after Reconcile failed: %v", err)
	}
	ref := metav1.GetControllerOf(&fresh)
	if ref == nil || ref.UID != target.UID {
		t.Errorf("ownerRef should be updated to new UID %q; got %+v", target.UID, ref)
	}
}

// TestSpannerManualScalingReconciler_UpdateConflictRequeues ensures that a
// conflict on the ownerReference Update is reported as a benign Requeue,
// not a hard error — controller-runtime will retry with a fresh
// resourceVersion.
func TestSpannerManualScalingReconciler_UpdateConflictRequeues(t *testing.T) {
	scheme := manualScalingTestScheme(t)
	now := time.Date(2026, 5, 30, 12, 0, 0, 0, time.UTC)
	target := saFixture("sa-target", "uid-target")
	ms := msFixture("ms1", "sa-target", "", now.Add(-time.Minute), time.Time{}, nil)
	base := fakeclient.NewClientBuilder().WithScheme(scheme).WithObjects(target, ms).Build()
	cl := interceptor.NewClient(base, interceptor.Funcs{
		Update: func(_ context.Context, _ ctrlclient.WithWatch, obj ctrlclient.Object, _ ...ctrlclient.UpdateOption) error {
			return apierrors.NewConflict(
				schema.GroupResource{Group: "spanner.mercari.com", Resource: "spannermanualscalings"},
				obj.GetName(), apierrors.NewBadRequest("stale"),
			)
		},
	})
	r, _ := newSMSR(t, cl, now)

	res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{
		Namespace: "ns1", Name: "ms1",
	}})
	if err != nil {
		t.Fatalf("conflict should be swallowed; got err %v", err)
	}
	if !res.Requeue { //nolint:staticcheck // production code returns Result{Requeue: true} for benign conflict; the field is still supported in controller-runtime v0.24.
		t.Errorf("expected Requeue=true on conflict; got %+v", res)
	}
}

// TestSpannerManualScalingReconciler_NotFound covers the case where the
// SpannerManualScaling was deleted between watch event and Get: the
// reconciler must not error and must not requeue.
func TestSpannerManualScalingReconciler_NotFound(t *testing.T) {
	scheme := manualScalingTestScheme(t)
	cl := fakeclient.NewClientBuilder().WithScheme(scheme).Build()
	r, _ := newSMSR(t, cl, time.Now())

	res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{
		Namespace: "ns1", Name: "gone",
	}})
	if err != nil {
		t.Fatalf("NotFound on Get should be swallowed; got err %v", err)
	}
	if res.Requeue || res.RequeueAfter != 0 { //nolint:staticcheck // production code returns Result{Requeue: true} for benign conflict; the field is still supported in controller-runtime v0.24.
		t.Errorf("NotFound should not requeue; got %+v", res)
	}
}

// TestSpannerManualScalingReconciler_TerminalShortCircuit ensures a resource
// already in a terminal phase (Invalid / Expired / Superseded) returns
// immediately without touching the API for the parent SA or persisting any
// changes.
func TestSpannerManualScalingReconciler_TerminalShortCircuit(t *testing.T) {
	scheme := manualScalingTestScheme(t)
	now := time.Date(2026, 5, 30, 12, 0, 0, 0, time.UTC)
	// targetResource intentionally orphaned to prove the reconciler never
	// consulted it.
	ms := msFixture("ms1", "no-such-sa",
		spannerv1beta1.SpannerManualScalingPhaseExpired,
		now.Add(-time.Hour), now.Add(-time.Hour), nil)
	cl := fakeclient.NewClientBuilder().WithScheme(scheme).WithObjects(ms).
		WithStatusSubresource(&spannerv1beta1.SpannerManualScaling{}).Build()
	r, rec := newSMSR(t, cl, now)

	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{
		Namespace: "ns1", Name: "ms1",
	}}); err != nil {
		t.Fatalf("Reconcile returned err: %v", err)
	}
	// Phase must remain unchanged (no markInvalid even though target is missing).
	var fresh spannerv1beta1.SpannerManualScaling
	if err := cl.Get(context.Background(), ctrlclient.ObjectKeyFromObject(ms), &fresh); err != nil {
		t.Fatalf("get after Reconcile failed: %v", err)
	}
	if fresh.Status.Phase != spannerv1beta1.SpannerManualScalingPhaseExpired {
		t.Errorf("phase changed despite terminal short-circuit: %q", fresh.Status.Phase)
	}
	select {
	case ev := <-rec.Events:
		t.Errorf("no event expected; got %q", ev)
	default:
	}
}

// TestSpannerManualScalingReconciler_DeletionTimestamp ensures a resource
// with DeletionTimestamp set short-circuits (the garbage collector owns
// the lifecycle at that point).
func TestSpannerManualScalingReconciler_DeletionTimestamp(t *testing.T) {
	scheme := manualScalingTestScheme(t)
	now := time.Date(2026, 5, 30, 12, 0, 0, 0, time.UTC)
	target := saFixture("sa-target", "uid-target")
	ms := msFixture("ms1", "sa-target", "", now.Add(-time.Minute), time.Time{}, nil)
	deleting := metav1.Time{Time: now.Add(-time.Second)}
	ms.DeletionTimestamp = &deleting
	// fake client requires a finalizer when DeletionTimestamp is set so the
	// object is not garbage-collected on apply.
	ms.Finalizers = []string{"test/keep"}

	var updateCount int
	base := fakeclient.NewClientBuilder().WithScheme(scheme).WithObjects(target, ms).Build()
	cl := interceptor.NewClient(base, interceptor.Funcs{
		Update: func(ctx context.Context, c ctrlclient.WithWatch, obj ctrlclient.Object, opts ...ctrlclient.UpdateOption) error {
			updateCount++
			return c.Update(ctx, obj, opts...)
		},
	})
	r, _ := newSMSR(t, cl, now)

	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{
		Namespace: "ns1", Name: "ms1",
	}}); err != nil {
		t.Fatalf("Reconcile returned err: %v", err)
	}
	if updateCount != 0 {
		t.Errorf("Update should not be called when DeletionTimestamp set; got %d calls", updateCount)
	}
}

// TestSpannerManualScalingReconciler_MarkInvalidIdempotent ensures that if
// markInvalid is reached on a resource that is somehow already terminal (a
// race where the parent SA marked it first), it does not overwrite the
// existing terminal state.
func TestSpannerManualScalingReconciler_MarkInvalidIdempotent(t *testing.T) {
	scheme := manualScalingTestScheme(t)
	now := time.Date(2026, 5, 30, 12, 0, 0, 0, time.UTC)
	earlier := now.Add(-time.Hour)
	ms := msFixture("ms1", "sa", spannerv1beta1.SpannerManualScalingPhaseSuperseded,
		earlier, earlier, nil)
	ms.Status.Message = "set by parent SA"
	cl := fakeclient.NewClientBuilder().WithScheme(scheme).WithObjects(ms).
		WithStatusSubresource(&spannerv1beta1.SpannerManualScaling{}).Build()
	r, _ := newSMSR(t, cl, now)

	res, err := r.markInvalid(context.Background(), logr.Discard(), ms, "should not be applied")
	if err != nil {
		t.Fatalf("markInvalid returned err: %v", err)
	}
	if res.Requeue || res.RequeueAfter != 0 { //nolint:staticcheck // production code returns Result{Requeue: true} for benign conflict; the field is still supported in controller-runtime v0.24.
		t.Errorf("idempotent markInvalid should be no-op; got %+v", res)
	}

	var fresh spannerv1beta1.SpannerManualScaling
	if err := cl.Get(context.Background(), ctrlclient.ObjectKeyFromObject(ms), &fresh); err != nil {
		t.Fatalf("get after markInvalid failed: %v", err)
	}
	if fresh.Status.Phase != spannerv1beta1.SpannerManualScalingPhaseSuperseded {
		t.Errorf("phase should remain Superseded; got %q", fresh.Status.Phase)
	}
	if fresh.Status.Message != "set by parent SA" {
		t.Errorf("message should remain unchanged; got %q", fresh.Status.Message)
	}
	if !fresh.Status.FinishedAt.Time.Equal(earlier) {
		t.Errorf("finishedAt should remain at original instant; got %v", fresh.Status.FinishedAt.Time)
	}
}
