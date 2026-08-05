package fake

import (
	"context"

	"github.com/mercari/spanner-autoscaler/internal/syncer"
)

type Syncer struct {
	FakeStart func()
	FakeStop  func()
}

var _ syncer.Syncer = (*Syncer)(nil)

func (s *Syncer) Start() {
	if s.FakeStart != nil {
		s.FakeStart()
	}
}

func (s *Syncer) Stop() {
	if s.FakeStop != nil {
		s.FakeStop()
	}
}

func (s *Syncer) HasCredentials(credentials *syncer.Credentials) bool {
	return true
}

func (s *Syncer) UpdateInstance(_ context.Context, desiredProcessingUnits int) (int, error) {
	return desiredProcessingUnits, nil
}
