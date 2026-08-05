package monitoringemulator

import "sync"

type QuotaEntry struct {
	QuotaMetric string `json:"quotaMetric"`
	LimitName   string `json:"limitName"`
	LimitNodes  int64  `json:"limitNodes"`
	UsageNodes  int64  `json:"usageNodes"`
}

type quotaKey struct {
	project  string
	location string
}

type QuotaStore struct {
	mu   sync.RWMutex
	data map[quotaKey]QuotaEntry
}

func NewQuotaStore() *QuotaStore {
	return &QuotaStore{
		data: make(map[quotaKey]QuotaEntry),
	}
}

func (s *QuotaStore) Set(project, location string, entry QuotaEntry) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data[quotaKey{project: project, location: location}] = entry
}

func (s *QuotaStore) List(project string) map[string]QuotaEntry {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make(map[string]QuotaEntry)
	for key, entry := range s.data {
		if key.project == project {
			out[key.location] = entry
		}
	}
	return out
}

func (s *QuotaStore) Delete(project, location string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.data, quotaKey{project: project, location: location})
}
