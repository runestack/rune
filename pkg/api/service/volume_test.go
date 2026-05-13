package service

import (
	"context"
	"testing"

	"github.com/runestack/rune/pkg/api/generated"
	"github.com/runestack/rune/pkg/log"
	"github.com/runestack/rune/pkg/store"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func newVolumeTestService(t *testing.T) *VolumeService {
	t.Helper()
	st := store.NewTestStoreWithOptions(store.StoreOptions{
		ConfigLimits: store.Limits{MaxObjectBytes: 1 << 20, MaxKeyNameLength: 256},
	})
	return NewVolumeService(st, log.GetDefaultLogger())
}

func TestVolumeServiceCRUD(t *testing.T) {
	ctx := context.Background()
	svc := newVolumeTestService(t)

	if _, err := svc.CreateVolume(ctx, &generated.CreateVolumeRequest{
		Volume: &generated.Volume{
			Name:             "data",
			Namespace:        "prod",
			StorageClassName: "local",
			Size:             "1Gi",
			AccessMode:       "ReadWriteOnce",
			Labels:           map[string]string{"app": "api"},
		},
		EnsureNamespace: true,
	}); err != nil {
		t.Fatalf("create: %v", err)
	}

	getResp, err := svc.GetVolume(ctx, &generated.GetVolumeRequest{Name: "data", Namespace: "prod"})
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if getResp.Volume.StorageClassName != "local" {
		t.Fatalf("bad storage class: %s", getResp.Volume.StorageClassName)
	}
	if getResp.Volume.Status == "" {
		t.Fatalf("expected default status, got empty")
	}

	if _, err := svc.UpdateVolume(ctx, &generated.UpdateVolumeRequest{Volume: &generated.Volume{
		Name:             "data",
		Namespace:        "prod",
		StorageClassName: "local",
		Size:             "2Gi",
		AccessMode:       "ReadWriteOnce",
		Status:           "Available",
	}}); err != nil {
		t.Fatalf("update: %v", err)
	}

	if _, err := svc.DeleteVolume(ctx, &generated.DeleteVolumeRequest{Name: "data", Namespace: "prod"}); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := svc.GetVolume(ctx, &generated.GetVolumeRequest{Name: "data", Namespace: "prod"}); err == nil {
		t.Fatalf("expected NotFound after delete")
	}
}

func TestVolumeServiceListSelectors(t *testing.T) {
	ctx := context.Background()
	svc := newVolumeTestService(t)

	for _, v := range []*generated.Volume{
		{Name: "a", Namespace: "ns1", StorageClassName: "local", Size: "1Gi", AccessMode: "ReadWriteOnce", Labels: map[string]string{"tier": "fast"}, Status: "Available"},
		{Name: "b", Namespace: "ns1", StorageClassName: "local", Size: "1Gi", AccessMode: "ReadWriteOnce", Labels: map[string]string{"tier": "slow"}, Status: "Pending"},
		{Name: "c", Namespace: "ns2", StorageClassName: "local", Size: "1Gi", AccessMode: "ReadWriteOnce"},
	} {
		if _, err := svc.CreateVolume(ctx, &generated.CreateVolumeRequest{Volume: v, EnsureNamespace: true}); err != nil {
			t.Fatalf("create %s: %v", v.Name, err)
		}
	}

	resp, err := svc.ListVolumes(ctx, &generated.ListVolumesRequest{Namespace: "ns1"})
	if err != nil {
		t.Fatalf("list ns1: %v", err)
	}
	if len(resp.Volumes) != 2 {
		t.Fatalf("expected 2 in ns1, got %d", len(resp.Volumes))
	}

	resp, err = svc.ListVolumes(ctx, &generated.ListVolumesRequest{
		Namespace:     "ns1",
		LabelSelector: map[string]string{"tier": "fast"},
	})
	if err != nil {
		t.Fatalf("list label: %v", err)
	}
	if len(resp.Volumes) != 1 || resp.Volumes[0].Name != "a" {
		t.Fatalf("unexpected label-filtered: %+v", resp.Volumes)
	}

	resp, err = svc.ListVolumes(ctx, &generated.ListVolumesRequest{
		Namespace:     "ns1",
		FieldSelector: map[string]string{"status": "Available"},
	})
	if err != nil {
		t.Fatalf("list field: %v", err)
	}
	if len(resp.Volumes) != 1 || resp.Volumes[0].Name != "a" {
		t.Fatalf("unexpected field-filtered: %+v", resp.Volumes)
	}
}

func TestVolumeServiceCreateValidation(t *testing.T) {
	ctx := context.Background()
	svc := newVolumeTestService(t)

	if _, err := svc.CreateVolume(ctx, &generated.CreateVolumeRequest{}); err == nil {
		t.Fatalf("expected error for missing volume")
	}

	if _, err := svc.CreateVolume(ctx, &generated.CreateVolumeRequest{
		Volume:          &generated.Volume{Name: "Bad_Name", Namespace: "ns", StorageClassName: "local"},
		EnsureNamespace: true,
	}); err == nil {
		t.Fatalf("expected error for invalid name")
	}

	if _, err := svc.CreateVolume(ctx, &generated.CreateVolumeRequest{
		Volume:          &generated.Volume{Name: "ok", Namespace: "ns", StorageClassName: ""},
		EnsureNamespace: true,
	}); err == nil {
		t.Fatalf("expected error for missing storageClassName")
	}
}

func TestVolumeServiceCreateDuplicate(t *testing.T) {
	ctx := context.Background()
	svc := newVolumeTestService(t)

	v := &generated.Volume{Name: "data", Namespace: "ns", StorageClassName: "local", Size: "1Gi", AccessMode: "ReadWriteOnce"}
	if _, err := svc.CreateVolume(ctx, &generated.CreateVolumeRequest{Volume: v, EnsureNamespace: true}); err != nil {
		t.Fatalf("create: %v", err)
	}
	_, err := svc.CreateVolume(ctx, &generated.CreateVolumeRequest{Volume: v, EnsureNamespace: true})
	if err == nil {
		t.Fatalf("expected AlreadyExists")
	}
	if st, ok := status.FromError(err); !ok || st.Code() != codes.AlreadyExists {
		t.Fatalf("expected AlreadyExists, got %v", err)
	}
}

// TestVolumeServiceRetryProvision exercises the retry-provision RPC: it
// must reset Failed/Stalled volumes back to Pending and reject anything
// else.
func TestVolumeServiceRetryProvision(t *testing.T) {
	ctx := context.Background()
	svc := newVolumeTestService(t)

	mk := func(name, st string) {
		if _, err := svc.CreateVolume(ctx, &generated.CreateVolumeRequest{
			Volume: &generated.Volume{
				Name: name, Namespace: "ns", StorageClassName: "local",
				Size: "1Gi", AccessMode: "ReadWriteOnce", Status: st,
			},
			EnsureNamespace: true,
		}); err != nil {
			t.Fatalf("seed %s: %v", name, err)
		}
	}
	mk("failed-vol", "Failed")
	mk("stalled-vol", "Stalled")
	mk("avail-vol", "Available")

	// Failed → Pending.
	resp, err := svc.RetryProvisionVolume(ctx, &generated.RetryProvisionVolumeRequest{Name: "failed-vol", Namespace: "ns"})
	if err != nil {
		t.Fatalf("retry failed: %v", err)
	}
	if resp.Volume.Status != "Pending" {
		t.Fatalf("expected Pending after retry, got %q", resp.Volume.Status)
	}

	// Stalled → Pending.
	if _, err := svc.RetryProvisionVolume(ctx, &generated.RetryProvisionVolumeRequest{Name: "stalled-vol", Namespace: "ns"}); err != nil {
		t.Fatalf("retry stalled: %v", err)
	}

	// Available is rejected.
	_, err = svc.RetryProvisionVolume(ctx, &generated.RetryProvisionVolumeRequest{Name: "avail-vol", Namespace: "ns"})
	if err == nil {
		t.Fatalf("expected FailedPrecondition for Available volume")
	}
	if st, ok := status.FromError(err); !ok || st.Code() != codes.FailedPrecondition {
		t.Fatalf("expected FailedPrecondition, got %v", err)
	}

	// Missing volume is NotFound.
	_, err = svc.RetryProvisionVolume(ctx, &generated.RetryProvisionVolumeRequest{Name: "nope", Namespace: "ns"})
	if st, ok := status.FromError(err); !ok || st.Code() != codes.NotFound {
		t.Fatalf("expected NotFound, got %v", err)
	}
}

// TestVolumeServiceDetach exercises the detach RPC: without --force a
// Bound volume is refused; with --force the bind state is cleared and
// the volume is moved to Available.
func TestVolumeServiceDetach(t *testing.T) {
	ctx := context.Background()
	svc := newVolumeTestService(t)

	if _, err := svc.CreateVolume(ctx, &generated.CreateVolumeRequest{
		Volume: &generated.Volume{
			Name: "bound", Namespace: "ns", StorageClassName: "local",
			Size: "1Gi", AccessMode: "ReadWriteOnce",
			Status: "Bound", BoundClaim: "svc/data/0", BoundNode: "n1",
			Handle: "/var/lib/rune/v/ns/bound", OwnerService: "ns/svc",
		},
		EnsureNamespace: true,
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Soft detach refused.
	_, err := svc.DetachVolume(ctx, &generated.DetachVolumeRequest{Name: "bound", Namespace: "ns"})
	if err == nil {
		t.Fatalf("expected FailedPrecondition for soft detach of Bound volume")
	}
	if st, ok := status.FromError(err); !ok || st.Code() != codes.FailedPrecondition {
		t.Fatalf("expected FailedPrecondition, got %v", err)
	}

	// Force detach succeeds and clears state.
	resp, err := svc.DetachVolume(ctx, &generated.DetachVolumeRequest{Name: "bound", Namespace: "ns", Force: true})
	if err != nil {
		t.Fatalf("force detach: %v", err)
	}
	if resp.Volume.BoundClaim != "" || resp.Volume.BoundNode != "" || resp.Volume.OwnerService != "" {
		t.Fatalf("expected bind state cleared, got %+v", resp.Volume)
	}
	if resp.Volume.Status != "Available" {
		t.Fatalf("expected Available after force-detach, got %q", resp.Volume.Status)
	}

	// Soft detach of an already-detached volume succeeds (no-op-ish).
	resp2, err := svc.DetachVolume(ctx, &generated.DetachVolumeRequest{Name: "bound", Namespace: "ns"})
	if err != nil {
		t.Fatalf("soft detach of cleared volume: %v", err)
	}
	if resp2.Volume.Status != "Available" {
		t.Fatalf("expected Available, got %q", resp2.Volume.Status)
	}

	// Missing volume is NotFound.
	_, err = svc.DetachVolume(ctx, &generated.DetachVolumeRequest{Name: "nope", Namespace: "ns"})
	if st, ok := status.FromError(err); !ok || st.Code() != codes.NotFound {
		t.Fatalf("expected NotFound, got %v", err)
	}
}
