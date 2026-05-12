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

func newStorageClassTestService(t *testing.T) *StorageClassService {
	t.Helper()
	st := store.NewTestStoreWithOptions(store.StoreOptions{
		ConfigLimits: store.Limits{MaxObjectBytes: 1 << 20, MaxKeyNameLength: 256},
	})
	return NewStorageClassService(st, log.GetDefaultLogger())
}

func TestStorageClassServiceCRUD(t *testing.T) {
	ctx := context.Background()
	svc := newStorageClassTestService(t)

	if _, err := svc.CreateStorageClass(ctx, &generated.CreateStorageClassRequest{
		StorageClass: &generated.StorageClass{
			Name:          "fast",
			Driver:        "local-path",
			Parameters:    map[string]string{"path": "/var/lib/rune/fast"},
			ReclaimPolicy: "Retain",
			Labels:        map[string]string{"tier": "fast"},
		},
	}); err != nil {
		t.Fatalf("create: %v", err)
	}

	getResp, err := svc.GetStorageClass(ctx, &generated.GetStorageClassRequest{Name: "fast"})
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if getResp.StorageClass.Driver != "local-path" {
		t.Fatalf("bad driver: %s", getResp.StorageClass.Driver)
	}
	if getResp.StorageClass.Parameters["path"] != "/var/lib/rune/fast" {
		t.Fatalf("missing parameter")
	}

	if _, err := svc.UpdateStorageClass(ctx, &generated.UpdateStorageClassRequest{
		StorageClass: &generated.StorageClass{
			Name:          "fast",
			Driver:        "local-path",
			ReclaimPolicy: "Delete",
		},
	}); err != nil {
		t.Fatalf("update: %v", err)
	}

	getResp, err = svc.GetStorageClass(ctx, &generated.GetStorageClassRequest{Name: "fast"})
	if err != nil {
		t.Fatalf("get after update: %v", err)
	}
	if getResp.StorageClass.ReclaimPolicy != "Delete" {
		t.Fatalf("expected Delete, got %s", getResp.StorageClass.ReclaimPolicy)
	}

	if _, err := svc.DeleteStorageClass(ctx, &generated.DeleteStorageClassRequest{Name: "fast"}); err != nil {
		t.Fatalf("delete: %v", err)
	}
	_, err = svc.GetStorageClass(ctx, &generated.GetStorageClassRequest{Name: "fast"})
	if err == nil {
		t.Fatalf("expected NotFound after delete")
	}
	if st, ok := status.FromError(err); !ok || st.Code() != codes.NotFound {
		t.Fatalf("expected NotFound, got %v", err)
	}
}

func TestStorageClassServiceListLabelSelector(t *testing.T) {
	ctx := context.Background()
	svc := newStorageClassTestService(t)

	for _, sc := range []*generated.StorageClass{
		{Name: "fast", Driver: "local-path", Labels: map[string]string{"tier": "fast"}},
		{Name: "slow", Driver: "local-path", Labels: map[string]string{"tier": "slow"}},
	} {
		if _, err := svc.CreateStorageClass(ctx, &generated.CreateStorageClassRequest{StorageClass: sc}); err != nil {
			t.Fatalf("create %s: %v", sc.Name, err)
		}
	}

	resp, err := svc.ListStorageClasses(ctx, &generated.ListStorageClassesRequest{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(resp.StorageClasses) != 2 {
		t.Fatalf("expected 2, got %d", len(resp.StorageClasses))
	}

	resp, err = svc.ListStorageClasses(ctx, &generated.ListStorageClassesRequest{
		LabelSelector: map[string]string{"tier": "slow"},
	})
	if err != nil {
		t.Fatalf("list filtered: %v", err)
	}
	if len(resp.StorageClasses) != 1 || resp.StorageClasses[0].Name != "slow" {
		t.Fatalf("unexpected filtered result: %+v", resp.StorageClasses)
	}
}

func TestStorageClassServiceCreateValidation(t *testing.T) {
	ctx := context.Background()
	svc := newStorageClassTestService(t)

	if _, err := svc.CreateStorageClass(ctx, &generated.CreateStorageClassRequest{}); err == nil {
		t.Fatalf("expected error for missing storage_class")
	}

	if _, err := svc.CreateStorageClass(ctx, &generated.CreateStorageClassRequest{
		StorageClass: &generated.StorageClass{Name: "Bad_Name", Driver: "local-path"},
	}); err == nil {
		t.Fatalf("expected error for invalid name")
	}

	if _, err := svc.CreateStorageClass(ctx, &generated.CreateStorageClassRequest{
		StorageClass: &generated.StorageClass{Name: "ok", Driver: ""},
	}); err == nil {
		t.Fatalf("expected error for missing driver")
	}
}

func TestStorageClassServiceCreateDuplicate(t *testing.T) {
	ctx := context.Background()
	svc := newStorageClassTestService(t)

	sc := &generated.StorageClass{Name: "fast", Driver: "local-path"}
	if _, err := svc.CreateStorageClass(ctx, &generated.CreateStorageClassRequest{StorageClass: sc}); err != nil {
		t.Fatalf("create: %v", err)
	}
	_, err := svc.CreateStorageClass(ctx, &generated.CreateStorageClassRequest{StorageClass: sc})
	if err == nil {
		t.Fatalf("expected AlreadyExists")
	}
	if st, ok := status.FromError(err); !ok || st.Code() != codes.AlreadyExists {
		t.Fatalf("expected AlreadyExists, got %v", err)
	}
}
