package controllers

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Nil/empty inputs are a no-op: applyFSOwnership must not stat or
// otherwise touch the path. That matters for local-host volumes
// (operator-managed paths) where the controller should stay
// hands-off.
func TestApplyFSOwnership_AbsentFieldsAreNoOp(t *testing.T) {
	// Path deliberately does not exist — applyFSOwnership must not
	// reach the Stat call.
	if err := applyFSOwnership("/nonexistent/path", nil, nil, ""); err != nil {
		t.Fatalf("expected no-op, got error: %v", err)
	}
}

func TestApplyFSOwnership_ChmodApplied(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "mountroot")
	require.NoError(t, os.Mkdir(target, 0o700))

	require.NoError(t, applyFSOwnership(target, nil, nil, "0775"))

	info, err := os.Stat(target)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o775), info.Mode().Perm())
}

func TestApplyFSOwnership_ChmodSkippedWhenMatching(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "mountroot")
	require.NoError(t, os.Mkdir(target, 0o775))
	// Idempotency: with mode already at target, applyFSOwnership
	// should be a successful no-op (no permission error from a
	// redundant chmod).
	require.NoError(t, applyFSOwnership(target, nil, nil, "0775"))
}

func TestApplyFSOwnership_InvalidFSModeFails(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "mountroot")
	require.NoError(t, os.Mkdir(target, 0o700))
	err := applyFSOwnership(target, nil, nil, "not-octal")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "parse fsMode")
}

// Idempotent chown: passing the path's current uid/gid is a no-op.
// We can't assert across-uid behaviour in unit tests without root,
// but we can prove the same-uid path doesn't error.
func TestApplyFSOwnership_SameUIDIsNoOp(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "mountroot")
	require.NoError(t, os.Mkdir(target, 0o700))

	info, err := os.Stat(target)
	require.NoError(t, err)
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		t.Skip("non-unix file system; skipping")
	}
	uid := int(st.Uid)
	gid := int(st.Gid)
	require.NoError(t, applyFSOwnership(target, &uid, &gid, ""))
}
