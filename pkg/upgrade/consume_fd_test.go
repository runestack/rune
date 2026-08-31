//go:build linux

package upgrade

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// The applier reads a directory the unprivileged service user owns, so the
// fd checks in consumeFileAt are the boundary between "root installs a
// published build" and "root installs whatever that account wrote". Each
// case here pins one check.
func TestConsumeFileAt_RejectsUntrustedFiles(t *testing.T) {
	dir := t.TempDir()
	dirfd, err := syscall.Open(dir, syscall.O_DIRECTORY|syscall.O_RDONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer syscall.Close(dirfd)

	var st syscall.Stat_t
	if err := syscall.Fstat(dirfd, &st); err != nil {
		t.Fatal(err)
	}
	self := st.Uid

	write := func(name string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte("payload"), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}

	t.Run("wrong owner", func(t *testing.T) {
		write("owner")
		// wantUID is the staging dir's owner; a file planted by any other
		// account must be refused.
		_, err := consumeFileAt(dirfd, dir, "owner", self+1, "")
		if err == nil || !strings.Contains(err.Error(), "owned by uid") {
			t.Fatalf("expected owner refusal, got %v", err)
		}
	})

	t.Run("hardlink", func(t *testing.T) {
		p := write("linked")
		if err := os.Link(p, filepath.Join(dir, "linked.alias")); err != nil {
			t.Skipf("hardlinks unsupported here: %v", err)
		}
		_, err := consumeFileAt(dirfd, dir, "linked", self, "")
		if err == nil || !strings.Contains(err.Error(), "link count") {
			t.Fatalf("expected hardlink refusal, got %v", err)
		}
	})

	t.Run("symlink", func(t *testing.T) {
		if err := os.Symlink("/etc/passwd", filepath.Join(dir, "sym")); err != nil {
			t.Fatal(err)
		}
		if _, err := consumeFileAt(dirfd, dir, "sym", self, ""); err == nil {
			t.Fatal("expected O_NOFOLLOW to refuse a symlink")
		}
	})

	t.Run("directory", func(t *testing.T) {
		if err := os.Mkdir(filepath.Join(dir, "adir"), 0o755); err != nil {
			t.Fatal(err)
		}
		if _, err := consumeFileAt(dirfd, dir, "adir", self, ""); err == nil {
			t.Fatal("expected a non-regular file to be refused")
		}
	})
}
