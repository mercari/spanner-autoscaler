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

package simulator

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"sigs.k8s.io/yaml"

	spannerv1beta1 "github.com/mercari/spanner-autoscaler/api/v1beta1"
	webhookv1beta1 "github.com/mercari/spanner-autoscaler/internal/webhook/v1beta1"
)

var yamlDocSeparator = regexp.MustCompile(`(?m)^---\s*$`)

// LoadManifests parses one or more YAML documents and returns the single
// SpannerAutoscaler plus any SpannerAutoscaleSchedules they contain. YAML
// documents of other kinds are ignored, so an existing manifest directory
// dump can be passed as-is.
//
// The production defaulting webhook is applied to the autoscaler (nodes→PU
// conversion, default scaledownStepSize, …) so the simulated spec matches
// what the controller would actually see in-cluster.
func LoadManifests(data []byte) (*spannerv1beta1.SpannerAutoscaler, []*spannerv1beta1.SpannerAutoscaleSchedule, error) {
	var autoscaler *spannerv1beta1.SpannerAutoscaler
	var schedules []*spannerv1beta1.SpannerAutoscaleSchedule

	for i, doc := range yamlDocSeparator.Split(string(data), -1) {
		if strings.TrimSpace(doc) == "" {
			continue
		}

		var meta struct {
			Kind       string `json:"kind"`
			APIVersion string `json:"apiVersion"`
		}
		if err := yaml.Unmarshal([]byte(doc), &meta); err != nil {
			return nil, nil, fmt.Errorf("yaml document %d: %w", i+1, err)
		}

		switch meta.Kind {
		case "SpannerAutoscaler":
			if autoscaler != nil {
				return nil, nil, fmt.Errorf("yaml document %d: multiple SpannerAutoscaler resources found; pass exactly one", i+1)
			}
			var sa spannerv1beta1.SpannerAutoscaler
			if err := yaml.UnmarshalStrict([]byte(doc), &sa); err != nil {
				return nil, nil, fmt.Errorf("yaml document %d (SpannerAutoscaler): %w", i+1, err)
			}
			autoscaler = &sa

		case "SpannerAutoscaleSchedule":
			var sas spannerv1beta1.SpannerAutoscaleSchedule
			if err := yaml.UnmarshalStrict([]byte(doc), &sas); err != nil {
				return nil, nil, fmt.Errorf("yaml document %d (SpannerAutoscaleSchedule): %w", i+1, err)
			}
			schedules = append(schedules, &sas)

		default:
			// Other kinds (Namespace, Secret, …) commonly live in the same
			// manifest file; skip them silently.
		}
	}

	if autoscaler == nil {
		return nil, nil, fmt.Errorf("no SpannerAutoscaler resource found in manifests")
	}

	if err := (&webhookv1beta1.SpannerAutoscalerCustomDefaulter{}).Default(context.Background(), autoscaler); err != nil {
		return nil, nil, fmt.Errorf("applying SpannerAutoscaler defaults: %w", err)
	}

	return autoscaler, schedules, nil
}
