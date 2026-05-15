package portforwarddaemon

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestWriteAndLoadForward(t *testing.T) {
	dir := t.TempDir()
	fwd := &Forward{
		ID:         "fwd-test1",
		Namespace:  "shared",
		TargetKind: TargetService,
		Target:     "mongo",
		Mappings:   []PortMapping{{Local: "127.0.0.1:27017", Remote: 27017}},
		CreatedAt:  time.Now().UTC().Truncate(time.Second),
		Status:     StatusActive,
	}
	if err := WriteForward(dir, fwd); err != nil {
		t.Fatalf("write: %v", err)
	}

	loaded, err := LoadForwards(dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(loaded) != 1 {
		t.Fatalf("expected 1 forward, got %d", len(loaded))
	}
	if loaded[0].ID != fwd.ID || loaded[0].Target != fwd.Target {
		t.Fatalf("loaded mismatch: %+v", loaded[0])
	}

	// File permissions should be 0600.
	st, err := os.Stat(ForwardPath(dir, fwd.ID))
	if err != nil {
		t.Fatal(err)
	}
	if perm := st.Mode().Perm(); perm != 0o600 {
		t.Errorf("expected 0600 perms, got %o", perm)
	}
}

func TestLoadForwards_SkipsJunk(t *testing.T) {
	dir := t.TempDir()
	// Drop a non-json sibling and a malformed json that should be
	// silently skipped (not break recovery).
	if err := os.WriteFile(filepath.Join(dir, "README"), []byte("hi"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "broken.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}

	good := &Forward{
		ID:         "fwd-good",
		TargetKind: TargetService,
		Target:     "svc",
		CreatedAt:  time.Now().UTC(),
	}
	if err := WriteForward(dir, good); err != nil {
		t.Fatal(err)
	}

	out, err := LoadForwards(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 1 || out[0].ID != "fwd-good" {
		t.Fatalf("expected only fwd-good, got %+v", out)
	}
}

func TestRemoveForward_Idempotent(t *testing.T) {
	dir := t.TempDir()
	if err := RemoveForward(dir, "missing"); err != nil {
		t.Fatalf("removing missing should be a no-op, got %v", err)
	}
}
