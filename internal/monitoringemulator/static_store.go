package monitoringemulator

import "sync"

// storeKey returns a unique key for the given project and instance ID.
func storeKey(project, instanceID string) string {
	return project + "/" + instanceID
}

// StaticStore holds fixed CPU utilization values per Spanner instance.
// It is safe for concurrent use.
type StaticStore struct {
	mu   sync.RWMutex
	data map[string]float64
}

func NewStaticStore() *StaticStore {
	return &StaticStore{
		data: make(map[string]float64),
	}
}

func (s *StaticStore) Set(project, instanceID string, cpuUtilization float64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data[storeKey(project, instanceID)] = cpuUtilization
}

func (s *StaticStore) Get(project, instanceID string) (float64, bool) {
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
