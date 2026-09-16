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

package scaling

import (
	"fmt"
	"regexp"
	"time"
)

// Limits on spec.scaleConfig.metricWindows, enforced by the admission
// webhook. Longer windows would inflate every Cloud Monitoring response and
// delay how quickly a sustained-load rule reacts; more windows multiply the
// per-sync aggregation work and the CEL variable surface.
const (
	// MaxMetricWindow is the longest allowed metric window.
	MaxMetricWindow = time.Hour
	// MaxMetricWindows is the maximum number of metric windows per resource.
	MaxMetricWindows = 4
)

// metricWindowPattern restricts windows to a whole number of minutes or
// hours written as a single unit ("15m", "1h"). The window string is used
// verbatim as the CEL variable suffix (cpu.highPriority.min15m), so compound
// spellings such as "1h30m" are rejected to keep the variable names
// unambiguous.
var metricWindowPattern = regexp.MustCompile(`^[1-9]\d*[mh]$`)

// ParseMetricWindow parses one spec.scaleConfig.metricWindows entry and
// enforces the window constraints shared by the webhook and the controller.
func ParseMetricWindow(window string) (time.Duration, error) {
	if !metricWindowPattern.MatchString(window) {
		return 0, fmt.Errorf("metric window %q must be a whole number of minutes or hours such as \"15m\" or \"1h\"", window)
	}
	d, err := time.ParseDuration(window)
	if err != nil {
		return 0, fmt.Errorf("metric window %q: %w", window, err)
	}
	if d > MaxMetricWindow {
		return 0, fmt.Errorf("metric window %q exceeds the maximum of %s", window, MaxMetricWindow)
	}
	return d, nil
}

// ValidMetricWindows filters windows down to the entries ParseMetricWindow
// accepts, preserving order and dropping duplicates. Invalid entries cannot
// occur on a webhook-validated spec; they are skipped here so that malformed
// objects already in-cluster degrade to "window not available" (which makes
// CEL evaluation fail safe) instead of breaking the caller.
func ValidMetricWindows(windows []string) []string {
	valid, _ := ValidMetricWindowDurations(windows)
	return valid
}

// ValidMetricWindowDurations is ValidMetricWindows returning the parsed
// durations alongside the window strings (parallel slices): the strings name
// the CEL variables and status entries, the durations drive the metric
// queries and aggregation.
func ValidMetricWindowDurations(windows []string) ([]string, []time.Duration) {
	valid := make([]string, 0, len(windows))
	durations := make([]time.Duration, 0, len(windows))
	seen := make(map[string]bool, len(windows))
	for _, w := range windows {
		d, err := ParseMetricWindow(w)
		if err != nil || seen[w] {
			continue
		}
		seen[w] = true
		valid = append(valid, w)
		durations = append(durations, d)
	}
	return valid, durations
}
