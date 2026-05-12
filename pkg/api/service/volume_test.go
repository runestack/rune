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
