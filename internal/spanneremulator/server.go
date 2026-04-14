// Package spanneremulator provides a minimal in-memory implementation of the
// Cloud Spanner Instance Admin gRPC service for use in integration tests.
//
// Only the operations used by spanner-autoscaler are implemented:
//
//   - GetInstance
//   - UpdateInstance
//
// All other methods return codes.Unimplemented via the embedded
// UnimplementedInstanceAdminServer.
//
// Long-running operations (UpdateInstance) are returned as already completed
// (Done: true) so callers do not need to poll GetOperation.
//
// To pre-populate instances for tests, use the HTTP admin API (admin.go).
package spanneremulator

import (
	"context"
	"sync"

	longrunningpb "cloud.google.com/go/longrunning/autogen/longrunningpb"
	instancepb "cloud.google.com/go/spanner/admin/instance/apiv1/instancepb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/anypb"
)

// Server implements instancepb.InstanceAdminServer with in-memory state.
type Server struct {
	instancepb.UnimplementedInstanceAdminServer

	mu        sync.RWMutex
	instances map[string]*instancepb.Instance // key: "projects/{p}/instances/{i}"
}

// NewServer returns a new Server with an empty instance store.
func NewServer() *Server {
	return &Server{
		instances: make(map[string]*instancepb.Instance),
	}
}

// GetInstance returns the named instance or codes.NotFound.
func (s *Server) GetInstance(_ context.Context, req *instancepb.GetInstanceRequest) (*instancepb.Instance, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	inst, ok := s.instances[req.GetName()]
	if !ok {
		return nil, status.Errorf(codes.NotFound, "instance %q not found", req.GetName())
	}
	return inst, nil
}

// UpdateInstance applies field-masked updates to an existing instance and
// returns a completed LRO.
func (s *Server) UpdateInstance(_ context.Context, req *instancepb.UpdateInstanceRequest) (*longrunningpb.Operation, error) {
	patch := req.GetInstance()
	if patch == nil || patch.GetName() == "" {
		return nil, status.Error(codes.InvalidArgument, "instance.name is required")
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	existing, ok := s.instances[patch.GetName()]
	if !ok {
		return nil, status.Errorf(codes.NotFound, "instance %q not found", patch.GetName())
	}

	if mask := req.GetFieldMask(); mask != nil {
		for _, path := range mask.GetPaths() {
			switch path {
			case "processing_units":
				existing.ProcessingUnits = patch.ProcessingUnits
			case "node_count":
				existing.NodeCount = patch.NodeCount
			case "display_name":
				existing.DisplayName = patch.DisplayName
			}
		}
	} else {
		existing.ProcessingUnits = patch.ProcessingUnits
		existing.NodeCount = patch.NodeCount
		existing.DisplayName = patch.DisplayName
	}

	return completedOperation(existing)
}

// AdminUpsertInstance creates or replaces an instance. Used by the HTTP admin API.
func (s *Server) AdminUpsertInstance(name string, processingUnits int32) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.instances[name] = &instancepb.Instance{
		Name:            name,
		DisplayName:     name,
		ProcessingUnits: processingUnits,
		State:           instancepb.Instance_READY,
	}
}

// AdminDeleteInstance removes an instance. Used by the HTTP admin API.
func (s *Server) AdminDeleteInstance(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.instances, name)
}

// completedOperation wraps inst in an already-Done LRO response.
// The Spanner client library checks Done before polling GetOperation,
// so no OperationsServer is needed.
func completedOperation(inst *instancepb.Instance) (*longrunningpb.Operation, error) {
	result, err := anypb.New(inst)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "marshal operation result: %v", err)
	}
	return &longrunningpb.Operation{
		Name: inst.GetName() + "/operations/op",
		Done: true,
		Result: &longrunningpb.Operation_Response{
			Response: result,
		},
	}, nil
}
