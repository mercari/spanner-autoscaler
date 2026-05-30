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
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

func intstrPtr(v intstr.IntOrString) *intstr.IntOrString { return &v }
func durPtr(d time.Duration) *metav1.Duration            { return &metav1.Duration{Duration: d} }

// TestValidateManualScalingSpec exercises the field-shape checks that do not
// require cross-resource lookups. These are the rules the validating webhook
// applies on every create (and the spec-immutability check on update
// short-circuits to a single error so it has no field-shape branches to
// cover here).
func TestValidateManualScalingSpec(t *testing.T) {
	future := metav1.Time{Time: time.Now().Add(time.Hour)}
	past := metav1.Time{Time: time.Now().Add(-time.Hour)}

	cases := []struct {
		name      string
		spec      SpannerManualScalingSpec
		isCreate  bool
		wantErrs  int
		wantField string // optional: substring of expected Field path
	}{
		{
			name: "valid minimal spec",
			spec: SpannerManualScalingSpec{
				TargetResource:  "sa-sample",
				ProcessingUnits: 5000,
			},
			isCreate: true,
			wantErrs: 0,
		},
		{
			name: "valid <1000 PU multiple of 100",
			spec: SpannerManualScalingSpec{
				TargetResource:  "sa-sample",
				ProcessingUnits: 300,
			},
			isCreate: true,
			wantErrs: 0,
		},
		{
			name: "targetResource missing",
			spec: SpannerManualScalingSpec{
				ProcessingUnits: 5000,
			},
			isCreate:  true,
			wantErrs:  1,
			wantField: "targetResource",
		},
		{
			name: "processingUnits zero",
			spec: SpannerManualScalingSpec{
				TargetResource:  "sa-sample",
				ProcessingUnits: 0,
			},
			isCreate:  true,
			wantErrs:  1,
			wantField: "processingUnits",
		},
		{
			name: "processingUnits <1000 not multiple of 100",
			spec: SpannerManualScalingSpec{
				TargetResource:  "sa-sample",
				ProcessingUnits: 350,
			},
			isCreate:  true,
			wantErrs:  1,
			wantField: "processingUnits",
		},
		{
			name: "processingUnits >=1000 not multiple of 1000",
			spec: SpannerManualScalingSpec{
				TargetResource:  "sa-sample",
				ProcessingUnits: 1500,
			},
			isCreate:  true,
			wantErrs:  1,
			wantField: "processingUnits",
		},
		{
			name: "scaleupStepSize valid (int 1000)",
			spec: SpannerManualScalingSpec{
				TargetResource:  "sa-sample",
				ProcessingUnits: 5000,
				ScaleupStepSize: intstrPtr(intstr.FromInt(1000)),
			},
			isCreate: true,
			wantErrs: 0,
		},
		{
			name: "scaleupStepSize valid (50%)",
			spec: SpannerManualScalingSpec{
				TargetResource:  "sa-sample",
				ProcessingUnits: 5000,
				ScaleupStepSize: intstrPtr(intstr.FromString("50%")),
			},
			isCreate: true,
			wantErrs: 0,
		},
		{
			name: "scaleupStepSize invalid (0%)",
			spec: SpannerManualScalingSpec{
				TargetResource:  "sa-sample",
				ProcessingUnits: 5000,
				ScaleupStepSize: intstrPtr(intstr.FromString("0%")),
			},
			isCreate:  true,
			wantErrs:  1,
			wantField: "scaleupStepSize",
		},
		{
			name: "scaleupStepSize invalid (1500 int)",
			spec: SpannerManualScalingSpec{
				TargetResource:  "sa-sample",
				ProcessingUnits: 5000,
				ScaleupStepSize: intstrPtr(intstr.FromInt(1500)),
			},
			isCreate:  true,
			wantErrs:  1,
			wantField: "scaleupStepSize",
		},
		{
			name: "scaleupInterval zero is rejected",
			spec: SpannerManualScalingSpec{
				TargetResource:  "sa-sample",
				ProcessingUnits: 5000,
				ScaleupInterval: durPtr(0),
			},
			isCreate:  true,
			wantErrs:  1,
			wantField: "scaleupInterval",
		},
		{
			name: "scaleupInterval negative is rejected",
			spec: SpannerManualScalingSpec{
				TargetResource:  "sa-sample",
				ProcessingUnits: 5000,
				ScaleupInterval: durPtr(-1 * time.Minute),
			},
			isCreate:  true,
			wantErrs:  1,
			wantField: "scaleupInterval",
		},
		{
			name: "expiresAt in future is OK on create",
			spec: SpannerManualScalingSpec{
				TargetResource:  "sa-sample",
				ProcessingUnits: 5000,
				ExpiresAt:       &future,
			},
			isCreate: true,
			wantErrs: 0,
		},
		{
			name: "expiresAt in past is rejected on create",
			spec: SpannerManualScalingSpec{
				TargetResource:  "sa-sample",
				ProcessingUnits: 5000,
				ExpiresAt:       &past,
			},
			isCreate:  true,
			wantErrs:  1,
			wantField: "expiresAt",
		},
		{
			name: "scaledownStepSize and scaledownInterval valid",
			spec: SpannerManualScalingSpec{
				TargetResource:    "sa-sample",
				ProcessingUnits:   2000,
				ScaledownStepSize: intstrPtr(intstr.FromInt(1000)),
				ScaledownInterval: durPtr(10 * time.Minute),
			},
			isCreate: true,
			wantErrs: 0,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			errs := validateManualScalingSpec(&tc.spec, tc.isCreate)
			if len(errs) != tc.wantErrs {
				t.Fatalf("got %d errors, want %d: %v", len(errs), tc.wantErrs, errs)
			}
			if tc.wantField != "" {
				found := false
				for _, e := range errs {
					if strings.Contains(e.Field, tc.wantField) {
						found = true
						break
					}
				}
				if !found {
					t.Errorf("expected an error on field containing %q, got: %v", tc.wantField, errs)
				}
			}
		})
	}
}

// TestIntervalWithoutStepSizeWarnings asserts that the typo pattern of
// setting an interval without the corresponding step size produces an
// admission warning (not a rejection — the override still operates, just as
// single-jump). The warning text is the operator's main signal that they
// likely meant to set the step size too.
func TestIntervalWithoutStepSizeWarnings(t *testing.T) {
	cases := []struct {
		name     string
		spec     SpannerManualScalingSpec
		wantWarn int
	}{
		{
			name:     "no warnings when nothing is set",
			spec:     SpannerManualScalingSpec{},
			wantWarn: 0,
		},
		{
			name: "no warning when both step and interval set",
			spec: SpannerManualScalingSpec{
				ScaleupStepSize: intstrPtr(intstr.FromInt(1000)),
				ScaleupInterval: durPtr(5 * time.Minute),
			},
			wantWarn: 0,
		},
		{
			name: "no warning when only step is set (interval falls back to flag default)",
			spec: SpannerManualScalingSpec{
				ScaleupStepSize: intstrPtr(intstr.FromInt(1000)),
			},
			wantWarn: 0,
		},
		{
			name: "warning when scaleupInterval set without scaleupStepSize",
			spec: SpannerManualScalingSpec{
				ScaleupInterval: durPtr(5 * time.Minute),
			},
			wantWarn: 1,
		},
		{
			name: "warning when scaledownInterval set without scaledownStepSize",
			spec: SpannerManualScalingSpec{
				ScaledownInterval: durPtr(10 * time.Minute),
			},
			wantWarn: 1,
		},
		{
			name: "two warnings when both interval-only patterns are present",
			spec: SpannerManualScalingSpec{
				ScaleupInterval:   durPtr(5 * time.Minute),
				ScaledownInterval: durPtr(10 * time.Minute),
			},
			wantWarn: 2,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := intervalWithoutStepSizeWarnings(&tc.spec)
			if len(w) != tc.wantWarn {
				t.Errorf("got %d warnings, want %d: %v", len(w), tc.wantWarn, w)
			}
		})
	}
}

// TestIsTerminalPhase locks down the set of phases the webhook's
// duplicate-active filter treats as terminal. Adding a new terminal phase
// requires updating this test.
func TestIsTerminalPhase(t *testing.T) {
	terminal := []SpannerManualScalingPhase{
		SpannerManualScalingPhaseExpired,
		SpannerManualScalingPhaseSuperseded,
		SpannerManualScalingPhaseInvalid,
	}
	nonTerminal := []SpannerManualScalingPhase{
		SpannerManualScalingPhasePending,
		SpannerManualScalingPhaseProgressing,
		SpannerManualScalingPhaseActive,
		SpannerManualScalingPhase(""), // empty = newly created
	}
	for _, p := range terminal {
		if !isTerminalPhase(p) {
			t.Errorf("phase %q should be terminal", p)
		}
	}
	for _, p := range nonTerminal {
		if isTerminalPhase(p) {
			t.Errorf("phase %q should not be terminal", p)
		}
	}
}
