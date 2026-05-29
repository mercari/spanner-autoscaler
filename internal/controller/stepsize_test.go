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
	"fmt"
	"testing"

	"k8s.io/apimachinery/pkg/util/intstr"
)

// legacyResolveStepSize reproduces the inline step-size resolution that lived
// in calcDesiredProcessingUnits before the helper extraction. It is kept here
// (verbatim from the original location) as the differential oracle for
// TestResolveStepSize_BehavioralEquivalence. Do NOT refactor or "clean up"
// this function — its only purpose is to be a frozen snapshot of the legacy
// behavior so the extracted helper can be proven equivalent. If a real
// behavior change is intentional, update this function in lockstep with the
// helper and re-review the test.
func legacyResolveStepSize(s *intstr.IntOrString, basePU int, dir stepDirection) int {
	var size int
	if dir == stepDirectionScaledown {
		resolved, err := intstr.GetScaledValueFromIntOrPercent(s, basePU, false)
		if err != nil {
			return 2000
		}
		size = resolved
		if size < 1000 {
			size = (size / 100) * 100
		} else {
			size = (size / 1000) * 1000
		}
		if size < 1000 && basePU > 1000 {
			size = 1000
		} else if size < 100 && basePU <= 1000 {
			size = 100
		}
		return size
	}
	// scaleup
	resolved, err := intstr.GetScaledValueFromIntOrPercent(s, basePU, false)
	if err != nil {
		return 0
	}
	size = resolved
	if size < 1000 {
		size = (size / 100) * 100
	} else {
		size = (size / 1000) * 1000
	}
	if size != 0 && size < 1000 && basePU+size > 1000 {
		size = 1000
	} else if size != 0 && size < 100 && basePU+size <= 1000 {
		size = 100
	}
	return size
}

// TestResolveStepSize_BehavioralEquivalence asserts that the extracted
// resolveStepSize helper produces identical output to the legacy inline logic
// across the parameter space the CPU-driven autoscale path exercises. This
// is the regression guard against the helper drifting from the legacy
// behavior: any silent divergence between the two would corrupt CPU
// autoscaling's step bound calculations.
func TestResolveStepSize_BehavioralEquivalence(t *testing.T) {
	// basePU values cover the boundaries the rounding rules care about:
	// <100, around the 100-multiple boundary, the 1000 boundary, and large.
	bases := []int{100, 200, 500, 900, 999, 1000, 1100, 1500, 2000, 5000, 7000, 9999, 10000}

	steps := []intstr.IntOrString{
		intstr.FromInt(0),   // zero value — must preserve scaleup "no cap" semantics
		intstr.FromInt(100), // 100-multiple
		intstr.FromInt(500),
		intstr.FromInt(1000), // 1000-multiple boundary
		intstr.FromInt(2000),
		intstr.FromInt(5000),
		intstr.FromString("1%"),
		intstr.FromString("5%"),
		intstr.FromString("10%"),
		intstr.FromString("25%"),
		intstr.FromString("50%"),
		intstr.FromString("99%"),
		intstr.FromString("100%"),
	}

	dirs := []stepDirection{stepDirectionScaledown, stepDirectionScaleup}

	for _, base := range bases {
		for _, step := range steps {
			for _, dir := range dirs {
				name := fmt.Sprintf("base=%d/step=%s/dir=%v", base, step.String(), dir)
				t.Run(name, func(t *testing.T) {
					got := resolveStepSize(&step, base, dir)
					want := legacyResolveStepSize(&step, base, dir)
					if got != want {
						t.Errorf("resolveStepSize(%v, %d, %v) = %d; legacy = %d",
							step, base, dir, got, want)
					}
				})
			}
		}
	}
}

// TestResolveStepSize_NilInput exercises the nil-pointer path, which the
// legacy code never hit directly (it always passed &sa.Spec.ScaleConfig.Foo,
// always non-nil). New callers in the manual scaling path will pass nil for
// unset optional fields, so we lock down the expected behavior:
//   - scaledown nil -> equivalent to IntOrString zero value (Int=0)
//   - scaleup   nil -> equivalent to IntOrString zero value (Int=0)
func TestResolveStepSize_NilInput(t *testing.T) {
	zero := intstr.FromInt(0)
	bases := []int{100, 1000, 5000}
	dirs := []stepDirection{stepDirectionScaledown, stepDirectionScaleup}

	for _, base := range bases {
		for _, dir := range dirs {
			gotNil := resolveStepSize(nil, base, dir)
			gotZero := resolveStepSize(&zero, base, dir)
			if gotNil != gotZero {
				t.Errorf("base=%d dir=%v: nil result %d differs from zero-value result %d",
					base, dir, gotNil, gotZero)
			}
		}
	}
}
