package monitoringemulator

import (
	"fmt"
	"strings"
	"sync"

	"google.golang.org/grpc/codes"
)

// QuotaMode causes quota metric queries to return the configured gRPC code
// instead of a successful TimeSeries response. Set via the admin API to
// exercise the controller's quota-lookup degrade paths in integration tests.
type QuotaMode struct {
	Code    codes.Code
	Message string
}

// QuotaModeStore holds at most one QuotaMode. Empty means quota queries use
// the QuotaStore normally.
type QuotaModeStore struct {
	mu   sync.RWMutex
	mode *QuotaMode
}

func NewQuotaModeStore() *QuotaModeStore {
	return &QuotaModeStore{}
}

func (s *QuotaModeStore) Set(code codes.Code, message string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.mode = &QuotaMode{Code: code, Message: message}
}

func (s *QuotaModeStore) Clear() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.mode = nil
}

func (s *QuotaModeStore) Get() (QuotaMode, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.mode == nil {
		return QuotaMode{}, false
	}
	return *s.mode, true
}

// ParseGrpcCode maps a case-insensitive gRPC code name (matching the Code's
// String()) to its codes.Code value. Returns an error for unknown names.
func ParseGrpcCode(name string) (codes.Code, error) {
	normalized := strings.ToLower(strings.TrimSpace(name))
	for code := codes.OK; code <= codes.Unauthenticated; code++ {
		if strings.ToLower(code.String()) == normalized {
			return code, nil
		}
	}
	return codes.OK, fmt.Errorf("unknown gRPC code %q", name)
}
