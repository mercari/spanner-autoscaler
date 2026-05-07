package monitoringemulator

import "sync"

// WorkloadParams holds the constant workload value for one CPU metric type.
//
// The relationship between CPU utilization, processing units, and workload is:
//
//	workload = cpu_utilization × processing_units  (constant)
//	current_cpu = workload / current_processing_units
//
// This models the behavior of real Cloud Spanner: scaling up PUs reduces CPU
// utilization proportionally for the same workload.
type WorkloadParams struct {
	Workload     float64 // ReferenceCPU * ReferencePU
	ReferenceCPU float64
	ReferencePU  int
}

// WorkloadEntry holds workload parameters for each CPU metric type.
// Either field may be nil if not configured for that metric kind.
type WorkloadEntry struct {
	HighPriority *WorkloadParams
	Total        *WorkloadParams
}

func (e WorkloadEntry) get(kind MetricKind) (WorkloadParams, bool) {
	switch kind {
	case MetricKindHighPriority:
		if e.HighPriority != nil {
			return *e.HighPriority, true
		}
	case MetricKindTotal:
		if e.Total != nil {
			return *e.Total, true
		}
	}
	return WorkloadParams{}, false
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

func (s *WorkloadStore) Set(project, instanceID string, entry WorkloadEntry) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data[storeKey(project, instanceID)] = entry
}

func (s *WorkloadStore) Get(project, instanceID string, kind MetricKind) (WorkloadParams, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	entry, ok := s.data[storeKey(project, instanceID)]
	if !ok {
		return WorkloadParams{}, false
	}
	return entry.get(kind)
}

func (s *WorkloadStore) GetEntry(project, instanceID string) (WorkloadEntry, bool) {
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

func newWorkloadParams(cpuUtilization float64, referencePU int) WorkloadParams {
	return WorkloadParams{
		Workload:     cpuUtilization * float64(referencePU),
		ReferenceCPU: cpuUtilization,
		ReferencePU:  referencePU,
	}
}
