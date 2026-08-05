package spanneremulator

import (
	"context"
	"testing"

	instancepb "cloud.google.com/go/spanner/admin/instance/apiv1/instancepb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

func TestUpdateInstance_QuotaResourceExhausted(t *testing.T) {
	srv := NewServer()
	config := "projects/proj/instanceConfigs/regional-asia-northeast1"
	srv.AdminUpsertInstance("projects/proj/instances/target", 100, config)
	srv.AdminSetQuota("proj", "regional-asia-northeast1", 100000)

	_, err := srv.UpdateInstance(context.Background(), &instancepb.UpdateInstanceRequest{
		Instance: &instancepb.Instance{
			Name:            "projects/proj/instances/target",
			ProcessingUnits: 101000,
		},
		FieldMask: &fieldmaskpb.FieldMask{Paths: []string{"processing_units"}},
	})
	if status.Code(err) != codes.ResourceExhausted {
		t.Fatalf("UpdateInstance() code = %v, want ResourceExhausted; err=%v", status.Code(err), err)
	}
}

func TestUpdateInstance_QuotaAllowsWithinLimit(t *testing.T) {
	srv := NewServer()
	config := "projects/proj/instanceConfigs/regional-asia-northeast1"
	srv.AdminUpsertInstance("projects/proj/instances/target", 100, config)
	srv.AdminSetQuota("proj", "regional-asia-northeast1", 100000)

	_, err := srv.UpdateInstance(context.Background(), &instancepb.UpdateInstanceRequest{
		Instance: &instancepb.Instance{
			Name:            "projects/proj/instances/target",
			ProcessingUnits: 100000,
		},
		FieldMask: &fieldmaskpb.FieldMask{Paths: []string{"processing_units"}},
	})
	if err != nil {
		t.Fatalf("UpdateInstance() error = %v", err)
	}
}

// TestUpdateInstance_QuotaAggregatesAcrossInstances verifies that the
// emulator's quota check sums PU across all instances sharing the same
// project/config — mirroring how Cloud Spanner quotas actually behave.
// 30000 (target after update) + 15000 (other) = 45000 > 40000 → RE.
func TestUpdateInstance_QuotaAggregatesAcrossInstances(t *testing.T) {
	srv := NewServer()
	config := "projects/proj/instanceConfigs/regional-asia-northeast1"
	srv.AdminUpsertInstance("projects/proj/instances/target", 10000, config)
	srv.AdminUpsertInstance("projects/proj/instances/other", 15000, config)
	srv.AdminSetQuota("proj", "regional-asia-northeast1", 40000)

	// 30000 + 15000 = 45000 → over quota.
	_, err := srv.UpdateInstance(context.Background(), &instancepb.UpdateInstanceRequest{
		Instance: &instancepb.Instance{
			Name:            "projects/proj/instances/target",
			ProcessingUnits: 30000,
		},
		FieldMask: &fieldmaskpb.FieldMask{Paths: []string{"processing_units"}},
	})
	if status.Code(err) != codes.ResourceExhausted {
		t.Fatalf("UpdateInstance over aggregate quota code = %v, want ResourceExhausted; err=%v", status.Code(err), err)
	}

	// 25000 + 15000 = 40000 → exactly at quota, must succeed.
	_, err = srv.UpdateInstance(context.Background(), &instancepb.UpdateInstanceRequest{
		Instance: &instancepb.Instance{
			Name:            "projects/proj/instances/target",
			ProcessingUnits: 25000,
		},
		FieldMask: &fieldmaskpb.FieldMask{Paths: []string{"processing_units"}},
	})
	if err != nil {
		t.Fatalf("UpdateInstance at aggregate quota cap = %v, want nil", err)
	}
}

// TestUpdateInstance_QuotaIgnoresOtherConfigs ensures the aggregate is scoped
// to project + config_id. A large instance in a different instance config must
// not count against the regional quota.
func TestUpdateInstance_QuotaIgnoresOtherConfigs(t *testing.T) {
	srv := NewServer()
	srv.AdminUpsertInstance("projects/proj/instances/target", 100,
		"projects/proj/instanceConfigs/regional-asia-northeast1")
	srv.AdminUpsertInstance("projects/proj/instances/other", 90000,
		"projects/proj/instanceConfigs/regional-us-central1")
	srv.AdminSetQuota("proj", "regional-asia-northeast1", 40000)

	_, err := srv.UpdateInstance(context.Background(), &instancepb.UpdateInstanceRequest{
		Instance: &instancepb.Instance{
			Name:            "projects/proj/instances/target",
			ProcessingUnits: 40000,
		},
		FieldMask: &fieldmaskpb.FieldMask{Paths: []string{"processing_units"}},
	})
	if err != nil {
		t.Fatalf("UpdateInstance with cross-config instance = %v, want nil", err)
	}
}
