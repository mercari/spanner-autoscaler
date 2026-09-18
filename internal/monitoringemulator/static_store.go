package monitoringemulator

import "sync"

// storeKey returns a unique key for the given project and instance ID.
func storeKey(project, instanceID string) string {
	return project + "/" + instanceID
}

// CPUEntry holds per-metric CPU utilization values for a Spanner instance.
// Either scalar field may be nil if not configured for that metric kind.
//
// HighPriorityRegions/TotalRegions hold per-region values instead, for
// simulating a multi-region instance; a scalar field and its regional
// counterpart are mutually exclusive for the same metric kind (enforced by
// the admin API, see validateStaticSetRequest in admin.go).
type CPUEntry struct {
	HighPriority        *float64
	HighPriorityRegions map[string]float64
	Total               *float64
	TotalRegions        map[string]float64
}

func (e CPUEntry) get(kind MetricKind) (float64, bool) {
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
	return 0, false
}

// regions returns a region-name -> CPU-utilization map for kind. A scalar
// value is reported as a single-entry map under the empty region key: with
// only one entry, grouped and ungrouped reduction both yield that same
// value, so callers can treat both forms uniformly.
func (e CPUEntry) regions(kind MetricKind) (map[string]float64, bool) {
	switch kind {
	case MetricKindHighPriority:
		if len(e.HighPriorityRegions) > 0 {
			return e.HighPriorityRegions, true
		}
		if e.HighPriority != nil {
			return map[string]float64{"": *e.HighPriority}, true
		}
	case MetricKindTotal:
		if len(e.TotalRegions) > 0 {
			return e.TotalRegions, true
		}
		if e.Total != nil {
			return map[string]float64{"": *e.Total}, true
		}
	}
	return nil, false
}

// StaticStore holds fixed CPU utilization values per Spanner instance.
// It is safe for concurrent use.
type StaticStore struct {
	mu   sync.RWMutex
	data map[string]CPUEntry
}

func NewStaticStore() *StaticStore {
	return &StaticStore{
		data: make(map[string]CPUEntry),
	}
}

func (s *StaticStore) Set(project, instanceID string, entry CPUEntry) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data[storeKey(project, instanceID)] = entry
}

func (s *StaticStore) Get(project, instanceID string, kind MetricKind) (float64, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	entry, ok := s.data[storeKey(project, instanceID)]
	if !ok {
		return 0, false
	}
	return entry.get(kind)
}

func (s *StaticStore) GetEntry(project, instanceID string) (CPUEntry, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	v, ok := s.data[storeKey(project, instanceID)]
	return v, ok
}

func (s *StaticStore) Delete(project, instanceID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.data, storeKey(project, instanceID))
}
