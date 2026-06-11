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
	"strings"
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
	quotas    map[string]int32                // key: "projects/{p}/instanceConfigs/{config_id}"
}

// NewServer returns a new Server with an empty instance store.
func NewServer() *Server {
	return &Server{
		instances: make(map[string]*instancepb.Instance),
		quotas:    make(map[string]int32),
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

	// A nil/empty field mask means "update every supported field" (mirrors the
	// real Spanner Admin API). Otherwise apply only the named paths.
	paths := req.GetFieldMask().GetPaths()
	willUpdate := func(field string) bool {
		if len(paths) == 0 {
			return true
		}
		for _, p := range paths {
			if p == field {
				return true
			}
		}
		return false
	}

	updatedProcessingUnits := existing.ProcessingUnits
	if willUpdate("processing_units") {
		updatedProcessingUnits = patch.ProcessingUnits
	}
	if err := s.checkQuotaLocked(existing, updatedProcessingUnits); err != nil {
		return nil, err
	}

	if willUpdate("processing_units") {
		existing.ProcessingUnits = patch.ProcessingUnits
	}
	if willUpdate("node_count") {
		existing.NodeCount = patch.NodeCount
	}
	if willUpdate("display_name") {
		existing.DisplayName = patch.DisplayName
	}

	return completedOperation(existing)
}

func (s *Server) checkQuotaLocked(target *instancepb.Instance, updatedProcessingUnits int32) error {
	projectID := projectIDFromInstanceName(target.GetName())
	configID := configIDFromConfigName(target.GetConfig())
	if projectID == "" || configID == "" {
		return nil
	}

	quotaKey := quotaStoreKey(projectID, configID)
	quota, ok := s.quotas[quotaKey]
	if !ok {
		return nil
	}

	var total int32
	for _, inst := range s.instances {
		if projectIDFromInstanceName(inst.GetName()) != projectID || configIDFromConfigName(inst.GetConfig()) != configID {
			continue
		}
		if inst.GetName() == target.GetName() {
			total += updatedProcessingUnits
			continue
		}
		total += inst.GetProcessingUnits()
	}
	if total > quota {
		return status.Errorf(codes.ResourceExhausted, "Project cannot add nodes for instance config %s", configID)
	}
	return nil
}

// AdminUpsertInstance creates or replaces an instance. Used by the HTTP admin API.
func (s *Server) AdminUpsertInstance(name string, processingUnits int32, config string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.instances[name] = &instancepb.Instance{
		Name:            name,
		DisplayName:     name,
		ProcessingUnits: processingUnits,
		Config:          config,
		State:           instancepb.Instance_READY,
	}
}

// AdminSetQuota sets a processing-units quota for a project/config pair.
func (s *Server) AdminSetQuota(projectID, configID string, processingUnits int32) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.quotas[quotaStoreKey(projectID, configID)] = processingUnits
}

// AdminDeleteQuota removes a processing-units quota for a project/config pair.
func (s *Server) AdminDeleteQuota(projectID, configID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.quotas, quotaStoreKey(projectID, configID))
}

// AdminDeleteInstance removes an instance. Used by the HTTP admin API.
func (s *Server) AdminDeleteInstance(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.instances, name)
}

func quotaStoreKey(projectID, configID string) string {
	return projectID + "/" + configID
}

func projectIDFromInstanceName(name string) string {
	const prefix = "projects/"
	if !strings.HasPrefix(name, prefix) {
		return ""
	}
	rest := strings.TrimPrefix(name, prefix)
	parts := strings.SplitN(rest, "/", 2)
	if len(parts) != 2 {
		return ""
	}
	return parts[0]
}

func configIDFromConfigName(config string) string {
	i := strings.LastIndex(config, "/")
	if i < 0 {
		return config
	}
	return config[i+1:]
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
