package monitoringemulator

import (
	"fmt"
	"regexp"
	"strings"
)

var instanceIDRegex = regexp.MustCompile(`resource\.label\.instance_id\s*=\s*"([^"]+)"`)

// extractInstanceID extracts the instance_id value from a Cloud Monitoring filter string.
//
// Example filter:
//
//	metric.type = "spanner.googleapis.com/instance/cpu/utilization_by_priority" AND
//	metric.label.priority = "high" AND
//	resource.label.instance_id = "my-instance"
func extractInstanceID(filter string) (string, error) {
	matches := instanceIDRegex.FindStringSubmatch(filter)
	if len(matches) < 2 {
		return "", fmt.Errorf("instance_id not found in filter: %s", filter)
	}
	return matches[1], nil
}

// extractProjectID extracts the project ID from a Cloud Monitoring resource name.
// The name must be in the format "projects/{project_id}".
func extractProjectID(name string) (string, error) {
	parts := strings.SplitN(name, "/", 2)
	if len(parts) != 2 || parts[0] != "projects" || parts[1] == "" {
		return "", fmt.Errorf("invalid resource name (expected \"projects/{project_id}\"): %q", name)
	}
	return parts[1], nil
}
