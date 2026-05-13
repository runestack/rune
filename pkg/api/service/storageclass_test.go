package service

import (
	"context"
	"testing"
	"time"

	"github.com/runestack/rune/pkg/api/generated"
	"github.com/runestack/rune/pkg/log"
	"github.com/runestack/rune/pkg/store"
	"github.com/runestack/rune/pkg/store/repos"
	"github.com/runestack/rune/pkg/types"
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

// TestStorageClassServiceDefaultUniqueness verifies the at-most-one-Default
// invariant is enforced on the API write path: the second Default-true
// Create wins, and the first class is atomically demoted; an Update that
// flips Default=true on an existing class likewise demotes its peers.
func TestStorageClassServiceDefaultUniqueness(t *testing.T) {
	ctx := context.Background()
	svc := newStorageClassTestService(t)

	// First default class.
	if _, err := svc.CreateStorageClass(ctx, &generated.CreateStorageClassRequest{
		StorageClass: &generated.StorageClass{Name: "first", Driver: "local", Default: true},
	}); err != nil {
		t.Fatalf("create first: %v", err)
	}

	// Second default class — should win and demote "first".
	if _, err := svc.CreateStorageClass(ctx, &generated.CreateStorageClassRequest{
		StorageClass: &generated.StorageClass{Name: "second", Driver: "local", Default: true},
	}); err != nil {
		t.Fatalf("create second: %v", err)
	}

	// Verify only "second" carries Default=true.
	resp, err := svc.ListStorageClasses(ctx, &generated.ListStorageClassesRequest{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	defaults := map[string]bool{}
	for _, c := range resp.StorageClasses {
		defaults[c.Name] = c.Default
	}
	if defaults["first"] {
		t.Errorf("first should have been demoted, got Default=true")
	}
	if !defaults["second"] {
		t.Errorf("second should be the new Default, got Default=false")
	}

	// A third non-default class doesn't disturb anything.
	if _, err := svc.CreateStorageClass(ctx, &generated.CreateStorageClassRequest{
		StorageClass: &generated.StorageClass{Name: "third", Driver: "local"},
	}); err != nil {
		t.Fatalf("create third: %v", err)
	}

	// Update "first" to Default=true — should now demote "second".
	if _, err := svc.UpdateStorageClass(ctx, &generated.UpdateStorageClassRequest{
		StorageClass: &generated.StorageClass{Name: "first", Driver: "local", Default: true},
	}); err != nil {
		t.Fatalf("update first: %v", err)
	}
	resp, err = svc.ListStorageClasses(ctx, &generated.ListStorageClassesRequest{})
	if err != nil {
		t.Fatalf("list after update: %v", err)
	}
	defaults = map[string]bool{}
	for _, c := range resp.StorageClasses {
		defaults[c.Name] = c.Default
	}
	if !defaults["first"] {
		t.Errorf("first should be Default after update, got Default=false")
	}
	if defaults["second"] {
		t.Errorf("second should have been demoted, got Default=true")
	}
	if defaults["third"] {
		t.Errorf("third was never Default, got Default=true")
	}
}

// TestStorageClassServiceDeleteCascade exercises the in-use guard added by
// RUNE-073: deletion is refused while any Volume still references the
// class, and --cascade bypasses that guard (without deleting the
// dependent volumes).
func TestStorageClassServiceDeleteCascade(t *testing.T) {
	ctx := context.Background()

	// Need to share one store between the service-under-test and the
	// volume repo so the volume seeded below is visible to the service's
	// cascade check. We use MemoryStore (not TestStore) because TestStore
	// rejects List(""), but the production BadgerStore — and our cascade
	// check — relies on cross-namespace listing semantics.
	st := store.NewMemoryStore()
	_ = st.Open("")
	svc := NewStorageClassService(st, log.GetDefaultLogger())
	volRepo := repos.NewVolumeRepo(st)
	// Cascade enumeration walks namespaces; ensure the test namespace
	// exists ("default" is reserved, so use a non-reserved name).
	nsRepo := repos.NewNamespaceRepo(st)
	if err := nsRepo.Create(ctx, &types.Namespace{Name: "team-a"}); err != nil {
		t.Fatalf("seed namespace: %v", err)
	}

	if _, err := svc.CreateStorageClass(ctx, &generated.CreateStorageClassRequest{
		StorageClass: &generated.StorageClass{Name: "fast", Driver: "local"},
	}); err != nil {
		t.Fatalf("create class: %v", err)
	}

	// Seed a volume that references the class.
	if err := volRepo.Create(ctx, &types.Volume{
		Name:             "data",
		Namespace:        "team-a",
		StorageClassName: "fast",
		AccessMode:       types.AccessModeRWO,
		Size:             "1Gi",
		CreatedAt:        time.Now(),
	}); err != nil {
		t.Fatalf("seed volume: %v", err)
	}

	// Delete without --cascade must be refused with FailedPrecondition
	// and the message must surface the dependent volume.
	_, err := svc.DeleteStorageClass(ctx, &generated.DeleteStorageClassRequest{Name: "fast"})
	if err == nil {
		t.Fatalf("expected FailedPrecondition while volume still references class")
	}
	if st, ok := status.FromError(err); !ok || st.Code() != codes.FailedPrecondition {
		t.Fatalf("expected FailedPrecondition, got %v", err)
	}

	// Delete with --cascade succeeds; the volume itself is intentionally
	// left in place.
	if _, err := svc.DeleteStorageClass(ctx, &generated.DeleteStorageClassRequest{
		Name: "fast", Cascade: true,
	}); err != nil {
		t.Fatalf("cascade delete: %v", err)
	}
	if _, err := svc.GetStorageClass(ctx, &generated.GetStorageClassRequest{Name: "fast"}); err == nil {
		t.Fatalf("class should be gone after cascade delete")
	}
	got, err := volRepo.Get(ctx, "team-a", "data")
	if err != nil {
		t.Fatalf("volume should still exist after --cascade: %v", err)
	}
	if got.StorageClassName != "fast" {
		t.Fatalf("volume StorageClassName should be unchanged, got %q", got.StorageClassName)
	}
}
