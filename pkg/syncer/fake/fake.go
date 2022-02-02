package fake

import (
	"context"

	"github.com/mercari/spanner-autoscaler/pkg/syncer"
)

type Syncer struct {
	FakeStart func()
	FakeStop  func()
}

var _ syncer.Syncer = (*Syncer)(nil)

func (s *Syncer) Start() {
	s.FakeStart()
}

func (s *Syncer) Stop() {
	s.FakeStop()
}

func (s *Syncer) HasCredentials(credentials *syncer.Credentials) bool {
	return true
}

func (s *Syncer) UpdateInstance(context.Context, int) error {
	return nil
}
