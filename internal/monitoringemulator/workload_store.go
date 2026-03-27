package monitoringemulator

import "sync"

// WorkloadEntry holds the constant workload value for a Spanner instance.
//
// The relationship between CPU utilization, processing units, and workload is:
//
//	workload = cpu_utilization × processing_units  (constant)
//	current_cpu = workload / current_processing_units
//
// This models the behaviour of real Cloud Spanner: scaling up PUs reduces CPU
// utilization proportionally for the same workload.
type WorkloadEntry struct {
	Workload     float64 // ReferenceCPU * ReferencePU
	ReferenceCPU float64
	ReferencePU  int
}

// WorkloadStore holds workload-based dynamic CPU calculation parameters per
// Spanner instance. It is safe for concurrent use.
type WorkloadStore struct {
	mu   sync.RWMutex
	data map[string]WorkloadEntry
}

func NewWorkloadStore() *WorkloadStore {
	return &WorkloadStore{
		data: make(map[string]WorkloadEntry),
	}
}

// Set stores a workload entry derived from the reference CPU utilization and
// reference processing units.
func (s *WorkloadStore) Set(project, instanceID string, cpuUtilization float64, referencePU int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data[storeKey(project, instanceID)] = WorkloadEntry{
		Workload:     cpuUtilization * float64(referencePU),
		ReferenceCPU: cpuUtilization,
		ReferencePU:  referencePU,
	}
}

func (s *WorkloadStore) Get(project, instanceID string) (WorkloadEntry, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	v, ok := s.data[storeKey(project, instanceID)]
	return v, ok
}

func (s *WorkloadStore) Delete(project, instanceID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.data, storeKey(project, instanceID))
}
