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

import "k8s.io/apimachinery/pkg/util/intstr"

// stepDirection selects the direction-specific rounding behavior of
// resolveStepSize. The two directions are mostly symmetric, but scaleup
// preserves a resolved value of 0 as "no per-step cap" (back-compat with the
// historical default), while scaledown always rounds 0 up to a minimum chunk
// (100 or 1000 depending on the base PU).
type stepDirection int

const (
	stepDirectionScaledown stepDirection = iota
	stepDirectionScaleup
)

// resolveStepSize resolves an IntOrString step size against basePU (typically
// the current processing units) using the same rules as the legacy inline
// logic that lived in calcDesiredProcessingUnits:
//
//  1. intstr percent-or-int resolution against basePU.
//  2. Round down to nearest 100 (when <1000) or 1000 (when >=1000), so that a
//     percentage step always lands on a Spanner-valid PU increment.
//  3. Round up to 100 or 1000 to avoid intermediate values (e.g. 7000->7600
//     becomes 7000->8000), with direction-specific handling of zero.
//
// A nil pointer is treated as the IntOrString zero value (Int=0).
//
// On intstr resolution error, the function returns the legacy hard-coded
// fallback: 2000 for scaledown, 0 for scaleup. These match the inline
// fallbacks in calcDesiredProcessingUnits.
//
// This helper is shared by the CPU-driven path and the SpannerManualScaling
// path so that percentage / rounding semantics stay identical. The differential
// test in stepsize_test.go locks down equivalence with the legacy inline
// version against the parameter space the CPU path exercises.
func resolveStepSize(s *intstr.IntOrString, basePU int, dir stepDirection) int {
	var size int
	if s != nil {
		resolved, err := intstr.GetScaledValueFromIntOrPercent(s, basePU, false)
		if err != nil {
			if dir == stepDirectionScaledown {
				return 2000
			}
			return 0
		}
		size = resolved
	}

	// Round down for percentage cases so the step stays within the requested
	// percentage of basePU.
	if size < 1000 {
		size = (size / 100) * 100
	} else {
		size = (size / 1000) * 1000
	}

	// Round up to avoid intermediate values that would land on invalid PU
	// boundaries. The condition differs between directions because scaleup
	// treats 0 as "no cap" (skipped downstream) while scaledown rounds 0 up
	// to ensure progress.
	switch dir {
	case stepDirectionScaledown:
		if size < 1000 && basePU > 1000 {
			size = 1000
		} else if size < 100 && basePU <= 1000 {
			size = 100
		}
	case stepDirectionScaleup:
		if size != 0 && size < 1000 && basePU+size > 1000 {
			size = 1000
		} else if size != 0 && size < 100 && basePU+size <= 1000 {
			size = 100
		}
	}

	return size
}
