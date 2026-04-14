package monitoringemulator

import (
	"fmt"
	"os"
	"sync"
	"time"

	sigsyaml "sigs.k8s.io/yaml"
)

// ScenarioStep is one step in a scenario sequence.
// Exactly one of CPUUtilization or Workload must be set.
//
//   - CPUUtilization: returns a fixed CPU value regardless of current PU.
//   - Workload: computes CPU dynamically (cpu = workload / current_pu),
//     modeling real Cloud Spanner behavior where scaling up reduces CPU.
type ScenarioStep struct {
	Duration       Duration          `json:"duration"`
	CPUUtilization *float64          `json:"cpu_utilization,omitempty"`
	Workload       *WorkloadScenario `json:"workload,omitempty"`
}

// WorkloadScenario holds the parameters for a workload-based step.
type WorkloadScenario struct {
	CPUUtilization           float64 `json:"cpu_utilization"`
	ReferenceProcessingUnits int     `json:"reference_processing_units"`
}

// scenarioFile is the top-level structure of a scenario YAML file.
type scenarioFile struct {
	Instances []instanceScenario `json:"instances"`
}

type instanceScenario struct {
	Project  string         `json:"project"`
	Instance string         `json:"instance"`
	Steps    []ScenarioStep `json:"steps"`
}

// Duration wraps time.Duration for YAML/JSON unmarshalling from strings like "30s".
type Duration struct{ time.Duration }

func (d *Duration) UnmarshalJSON(b []byte) error {
	s := string(b)
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		s = s[1 : len(s)-1]
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", s, err)
	}
	d.Duration = v
	return nil
}

// scenarioEntry tracks the runtime state of one instance's scenario.
type scenarioEntry struct {
	steps     []ScenarioStep
	total     time.Duration // sum of all step durations
	startTime time.Time
}

// currentStep returns the step active at the current moment.
// The scenario loops indefinitely.
func (e *scenarioEntry) currentStep() ScenarioStep {
	elapsed := time.Since(e.startTime) % e.total
	var cum time.Duration
	for _, step := range e.steps {
		cum += step.Duration.Duration
		if elapsed < cum {
			return step
		}
	}
	return e.steps[len(e.steps)-1]
}

// ScenarioStore holds time-based CPU scenarios per Spanner instance.
// It is safe for concurrent use.
type ScenarioStore struct {
	mu   sync.RWMutex
	data map[string]*scenarioEntry
}

func NewScenarioStore() *ScenarioStore {
	return &ScenarioStore{data: make(map[string]*scenarioEntry)}
}

// Set registers a scenario for the given instance, starting from now.
func (s *ScenarioStore) Set(project, instanceID string, steps []ScenarioStep) error {
	if len(steps) == 0 {
		return fmt.Errorf("scenario must have at least one step")
	}
	var total time.Duration
	for i, step := range steps {
		if step.Duration.Duration <= 0 {
			return fmt.Errorf("step %d: duration must be positive", i)
		}
		if step.CPUUtilization == nil && step.Workload == nil {
			return fmt.Errorf("step %d: must set cpu_utilization or workload", i)
		}
		if step.CPUUtilization != nil && step.Workload != nil {
			return fmt.Errorf("step %d: cpu_utilization and workload are mutually exclusive", i)
		}
		if step.CPUUtilization != nil && (*step.CPUUtilization < 0 || *step.CPUUtilization > 1) {
			return fmt.Errorf("step %d: cpu_utilization must be between 0.0 and 1.0", i)
		}
		total += step.Duration.Duration
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data[storeKey(project, instanceID)] = &scenarioEntry{
		steps:     steps,
		total:     total,
		startTime: time.Now(),
	}
	return nil
}

// Get returns the current active step for the instance, if a scenario is registered.
func (s *ScenarioStore) Get(project, instanceID string) (ScenarioStep, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	e, ok := s.data[storeKey(project, instanceID)]
	if !ok {
		return ScenarioStep{}, false
	}
	return e.currentStep(), true
}

// Delete removes the scenario for the given instance.
func (s *ScenarioStore) Delete(project, instanceID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.data, storeKey(project, instanceID))
}

// LoadFile loads a scenario YAML file and populates the store.
func (s *ScenarioStore) LoadFile(path string) error {
	data, err := os.ReadFile(path) //nolint:gosec
	if err != nil {
		return fmt.Errorf("read scenario file: %w", err)
	}
	var f scenarioFile
	if err := sigsyaml.Unmarshal(data, &f); err != nil {
		return fmt.Errorf("parse scenario file: %w", err)
	}
	for _, inst := range f.Instances {
		if err := s.Set(inst.Project, inst.Instance, inst.Steps); err != nil {
			return fmt.Errorf("instance %s/%s: %w", inst.Project, inst.Instance, err)
		}
	}
	return nil
}
