/*
Copyright 2026.

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

package v1beta1

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	fakeclient "sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

// webhookTestScheme builds a runtime.Scheme registered with the v1beta1 API
// group so the fake client can hold SpannerAutoscaler and SpannerManualScaling
// fixtures. Mirrors manualScalingTestScheme in internal/controller but kept
// local so this test file does not depend on internal/.
func webhookTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(s))
	utilruntime.Must(AddToScheme(s))
	return s
}

func saFixture(name string, currentPU int) *SpannerAutoscaler {
	return &SpannerAutoscaler{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "ns1",
			Name:      name,
		},
		Status: SpannerAutoscalerStatus{
			CurrentProcessingUnits: currentPU,
		},
	}
}

func smsForCreate(targetName string, pu int) *SpannerManualScaling {
	return &SpannerManualScaling{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "ns1",
			Name:      "ms-test",
		},
		Spec: SpannerManualScalingSpec{
			TargetResource:  targetName,
			ProcessingUnits: pu,
		},
	}
}

// TestCheckRejectScaledown exercises every documented branch of the
// reject-manual-scaledown cross-resource policy check. This is the
// security-relevant defense against scaledown via SpannerManualScaling, so
// the branch coverage is intentionally exhaustive.
func TestCheckRejectScaledown(t *testing.T) {
	cases := []struct {
		name           string
		clientObjs     []runtime.Object
		nilClient      bool
		ms             *SpannerManualScaling
		expectErr      bool
		expectContains string
	}{
		{
			name:           "nil client → Forbidden (cannot perform cross-resource lookup)",
			nilClient:      true,
			ms:             smsForCreate("sa-target", 5000),
			expectErr:      true,
			expectContains: "no client configured",
		},
		{
			name:           "target SpannerAutoscaler missing → Forbidden (fail-closed)",
			clientObjs:     nil,
			ms:             smsForCreate("sa-target", 5000),
			expectErr:      true,
			expectContains: "lookup failed",
		},
		{
			name:       "target current=0 (not yet synced) → allowed regardless of PU",
			clientObjs: []runtime.Object{saFixture("sa-target", 0)},
			ms:         smsForCreate("sa-target", 1000),
			expectErr:  false,
		},
		{
			name:           "scaledown (target PU < current) → Forbidden",
			clientObjs:     []runtime.Object{saFixture("sa-target", 5000)},
			ms:             smsForCreate("sa-target", 3000),
			expectErr:      true,
			expectContains: "forbids scaledown",
		},
		{
			name:       "equal (target PU == current) → allowed (not a scaledown)",
			clientObjs: []runtime.Object{saFixture("sa-target", 5000)},
			ms:         smsForCreate("sa-target", 5000),
			expectErr:  false,
		},
		{
			name:       "scaleup (target PU > current) → allowed",
			clientObjs: []runtime.Object{saFixture("sa-target", 5000)},
			ms:         smsForCreate("sa-target", 7000),
			expectErr:  false,
		},
	}

	scheme := webhookTestScheme(t)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := &spannerManualScalingWebhook{rejectScaledown: true}
			if !tc.nilClient {
				cb := fakeclient.NewClientBuilder().WithScheme(scheme)
				for _, o := range tc.clientObjs {
					cb = cb.WithObjects(o.(*SpannerAutoscaler))
				}
				w.client = cb.Build()
			}
			fldErr := w.checkRejectScaledown(context.Background(), tc.ms)
			if tc.expectErr {
				if fldErr == nil {
					t.Fatalf("expected an error, got nil")
				}
				if tc.expectContains != "" && !strings.Contains(fldErr.Error(), tc.expectContains) {
					t.Errorf("error %q does not contain %q", fldErr.Error(), tc.expectContains)
				}
			} else if fldErr != nil {
				t.Errorf("expected no error, got: %v", fldErr)
			}
		})
	}
}

// TestValidateUpdate_SpecImmutability locks down the contract that any spec
// mutation is rejected. The "create new SpannerManualScaling instead"
// pattern (newest-wins) is the linchpin of how operators change processing
// units, ramp parameters, or extend expiresAt — so it is important that the
// webhook reliably blocks every field-level edit attempt that would
// otherwise smuggle a scaledown through (or quietly mutate active behavior).
func TestValidateUpdate_SpecImmutability(t *testing.T) {
	t0 := metav1.Time{Time: time.Now().Add(time.Hour)}
	t1 := metav1.Time{Time: time.Now().Add(2 * time.Hour)}

	base := func() *SpannerManualScaling {
		return &SpannerManualScaling{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: "ns1",
				Name:      "ms-immut",
			},
			Spec: SpannerManualScalingSpec{
				TargetResource:    "sa-target",
				ProcessingUnits:   5000,
				ScaleupStepSize:   intstrPtr(intstr.FromInt(1000)),
				ScaleupInterval:   durPtr(5 * time.Minute),
				ScaledownStepSize: intstrPtr(intstr.FromInt(1000)),
				ScaledownInterval: durPtr(10 * time.Minute),
				ExpiresAt:         &t0,
			},
		}
	}

	cases := []struct {
		name      string
		mutate    func(o, n *SpannerManualScaling)
		expectErr bool
	}{
		{
			name:      "no spec change → accepted (e.g. label update)",
			mutate:    func(o, n *SpannerManualScaling) { n.Labels = map[string]string{"k": "v"} },
			expectErr: false,
		},
		{
			name:      "processingUnits changed → rejected",
			mutate:    func(_, n *SpannerManualScaling) { n.Spec.ProcessingUnits = 7000 },
			expectErr: true,
		},
		{
			name:      "targetResource changed → rejected",
			mutate:    func(_, n *SpannerManualScaling) { n.Spec.TargetResource = "different" },
			expectErr: true,
		},
		{
			name: "scaleupStepSize changed → rejected",
			mutate: func(_, n *SpannerManualScaling) {
				n.Spec.ScaleupStepSize = intstrPtr(intstr.FromInt(2000))
			},
			expectErr: true,
		},
		{
			name:      "scaleupStepSize set to nil → rejected",
			mutate:    func(_, n *SpannerManualScaling) { n.Spec.ScaleupStepSize = nil },
			expectErr: true,
		},
		{
			name:      "scaleupInterval changed → rejected",
			mutate:    func(_, n *SpannerManualScaling) { n.Spec.ScaleupInterval = durPtr(time.Minute) },
			expectErr: true,
		},
		{
			name:      "scaledownStepSize changed → rejected",
			mutate:    func(_, n *SpannerManualScaling) { n.Spec.ScaledownStepSize = intstrPtr(intstr.FromInt(500)) },
			expectErr: true,
		},
		{
			name:      "scaledownInterval changed → rejected",
			mutate:    func(_, n *SpannerManualScaling) { n.Spec.ScaledownInterval = durPtr(20 * time.Minute) },
			expectErr: true,
		},
		{
			name:      "expiresAt changed (extension) → rejected",
			mutate:    func(_, n *SpannerManualScaling) { n.Spec.ExpiresAt = &t1 },
			expectErr: true,
		},
		{
			name:      "expiresAt cleared → rejected",
			mutate:    func(_, n *SpannerManualScaling) { n.Spec.ExpiresAt = nil },
			expectErr: true,
		},
	}

	w := &spannerManualScalingWebhook{}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			oldObj := base()
			newObj := base()
			tc.mutate(oldObj, newObj)
			warns, err := w.ValidateUpdate(context.Background(), oldObj, newObj)
			if tc.expectErr {
				if err == nil {
					t.Fatalf("expected an error, got nil (warns=%v)", warns)
				}
				if !strings.Contains(err.Error(), "spec is immutable") {
					t.Errorf("error %q does not mention 'spec is immutable'", err.Error())
				}
			} else if err != nil {
				t.Errorf("expected no error, got: %v", err)
			}
		})
	}
}

// TestValidateUpdate_NilOldObj asserts the safety guard against a malformed
// admission request (oldObj missing). Without this guard a nil pointer
// dereference inside reflect.DeepEqual would crash the webhook. The
// assertion uses errors.Is against the package-level sentinel so future
// message rewording does not silently flip the test green.
func TestValidateUpdate_NilOldObj(t *testing.T) {
	w := &spannerManualScalingWebhook{}
	newObj := &SpannerManualScaling{}
	_, err := w.ValidateUpdate(context.Background(), nil, newObj)
	if !errors.Is(err, errNilOldObject) {
		t.Errorf("expected errNilOldObject, got %v", err)
	}
}

// dupMS builds a SpannerManualScaling fixture targeting sa-target with the
// requested phase + name; helper kept local to the duplicateActiveWarning
// tests so each case row is one line.
func dupMS(name string, phase SpannerManualScalingPhase, opts ...func(*SpannerManualScaling)) *SpannerManualScaling {
	ms := &SpannerManualScaling{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns1", Name: name},
		Spec:       SpannerManualScalingSpec{TargetResource: "sa-target", ProcessingUnits: 5000},
		Status:     SpannerManualScalingStatus{Phase: phase},
	}
	for _, o := range opts {
		o(ms)
	}
	return ms
}

// TestDuplicateActiveWarning_NilClient covers the no-client fall-through:
// the webhook must skip the warning rather than panic. List failures are
// fail-open by design, and nil-client is the limit case of that contract.
func TestDuplicateActiveWarning_NilClient(t *testing.T) {
	w := &spannerManualScalingWebhook{}
	obj := dupMS("new-ms", "")
	if warns := w.duplicateActiveWarning(context.Background(), obj); warns != nil {
		t.Errorf("nil client should skip warning, got %v", warns)
	}
}

// TestDuplicateActiveWarning_ListFailureFailsOpen ensures a List error is
// treated as fail-open (no warning emitted) so transient API errors do not
// block create. The runtime newest-wins rule still handles the duplicate
// case correctly regardless of whether the warning was emitted.
func TestDuplicateActiveWarning_ListFailureFailsOpen(t *testing.T) {
	scheme := webhookTestScheme(t)
	base := fakeclient.NewClientBuilder().WithScheme(scheme).Build()
	cl := interceptor.NewClient(base, interceptor.Funcs{
		List: func(_ context.Context, _ ctrlclient.WithWatch, _ ctrlclient.ObjectList, _ ...ctrlclient.ListOption) error {
			return errors.New("simulated API outage")
		},
	})
	w := &spannerManualScalingWebhook{client: cl}
	obj := dupMS("new-ms", "")
	if warns := w.duplicateActiveWarning(context.Background(), obj); warns != nil {
		t.Errorf("List failure should fail-open (no warning); got %v", warns)
	}
}

// TestDuplicateActiveWarning_Cases pins down the per-sibling filter rules:
// only a non-terminal, non-deleting sibling targeting the SAME SA (and not
// the resource being validated itself) triggers the warning.
func TestDuplicateActiveWarning_Cases(t *testing.T) {
	deleting := metav1.Time{Time: time.Now().Add(-time.Minute)}
	cases := []struct {
		name     string
		siblings []*SpannerManualScaling
		// obj is the resource being validated; its presence in the fake
		// client's store matters for the skip-self branch.
		objName      string
		wantWarning  bool
		warnContains string
	}{
		{
			name:        "no siblings → no warning",
			siblings:    nil,
			objName:     "new-ms",
			wantWarning: false,
		},
		{
			name: "sibling with wrong target → no warning",
			siblings: []*SpannerManualScaling{
				func() *SpannerManualScaling {
					ms := dupMS("other-ms", SpannerManualScalingPhaseActive)
					ms.Spec.TargetResource = "different-sa"
					return ms
				}(),
			},
			objName:     "new-ms",
			wantWarning: false,
		},
		{
			name: "sibling with terminal phase Superseded → no warning",
			siblings: []*SpannerManualScaling{
				dupMS("retired-ms", SpannerManualScalingPhaseSuperseded),
			},
			objName:     "new-ms",
			wantWarning: false,
		},
		{
			name: "sibling with terminal phase Expired → no warning",
			siblings: []*SpannerManualScaling{
				dupMS("retired-ms", SpannerManualScalingPhaseExpired),
			},
			objName:     "new-ms",
			wantWarning: false,
		},
		{
			name: "sibling with DeletionTimestamp set → no warning",
			siblings: []*SpannerManualScaling{
				func() *SpannerManualScaling {
					ms := dupMS("deleting-ms", SpannerManualScalingPhaseActive)
					ms.DeletionTimestamp = &deleting
					ms.Finalizers = []string{"test/keep"}
					return ms
				}(),
			},
			objName:     "new-ms",
			wantWarning: false,
		},
		{
			name: "only sibling has same name as obj (= self) → no warning",
			siblings: []*SpannerManualScaling{
				dupMS("new-ms", SpannerManualScalingPhaseActive),
			},
			objName:     "new-ms",
			wantWarning: false,
		},
		{
			name: "non-terminal sibling with different name → WARNING",
			siblings: []*SpannerManualScaling{
				dupMS("existing-ms", SpannerManualScalingPhaseActive),
			},
			objName:      "new-ms",
			wantWarning:  true,
			warnContains: "existing-ms",
		},
		{
			name: "non-terminal sibling in Progressing phase → WARNING (still active)",
			siblings: []*SpannerManualScaling{
				dupMS("existing-ms", SpannerManualScalingPhaseProgressing),
			},
			objName:      "new-ms",
			wantWarning:  true,
			warnContains: "Progressing",
		},
		{
			name: "non-terminal sibling with empty phase (newly created, not yet observed) → WARNING",
			siblings: []*SpannerManualScaling{
				dupMS("existing-ms", ""),
			},
			objName:      "new-ms",
			wantWarning:  true,
			warnContains: "existing-ms",
		},
	}

	scheme := webhookTestScheme(t)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cb := fakeclient.NewClientBuilder().WithScheme(scheme)
			for _, s := range tc.siblings {
				cb = cb.WithObjects(s)
			}
			w := &spannerManualScalingWebhook{client: cb.Build()}
			obj := dupMS(tc.objName, "")
			warns := w.duplicateActiveWarning(context.Background(), obj)
			if tc.wantWarning {
				if len(warns) == 0 {
					t.Fatalf("expected a warning, got none")
				}
				if tc.warnContains != "" && !strings.Contains(warns[0], tc.warnContains) {
					t.Errorf("warning %q does not contain %q", warns[0], tc.warnContains)
				}
			} else if len(warns) != 0 {
				t.Errorf("expected no warning, got %v", warns)
			}
		})
	}
}
