package docker

import (
	"errors"
	"io"
	"sync/atomic"
	"testing"
)

// fakeStream implements runner.ExecStream for the cleanupExecStream test.
// Only Close is exercised; the other methods are stubbed.
type fakeStream struct {
	closed   atomic.Int32
	closeErr error
}

func (f *fakeStream) Read(p []byte) (int, error)       { return 0, io.EOF }
func (f *fakeStream) Write(p []byte) (int, error)      { return len(p), nil }
func (f *fakeStream) Stderr() io.Reader                { return nil }
func (f *fakeStream) ResizeTerminal(w, h uint32) error { return nil }
func (f *fakeStream) Signal(name string) error         { return nil }
func (f *fakeStream) ExitCode() (int, error)           { return 0, nil }
func (f *fakeStream) Close() error {
	f.closed.Add(1)
	return f.closeErr
}

// TestCleanupExecStreamRunsCleanupOnce verifies the sidecar teardown wrapper:
// the underlying stream's Close is called, then the cleanup func runs, and
// repeat Closes don't re-run cleanup. This is the contract that keeps debug
// sidecars from getting orphaned (cleanup MUST run) and from being
// double-removed (cleanup MUST NOT re-run).
func TestCleanupExecStreamRunsCleanupOnce(t *testing.T) {
	inner := &fakeStream{}
	var cleanupRuns atomic.Int32
	wrapped := &cleanupExecStream{
		ExecStream: inner,
		cleanup:    func() { cleanupRuns.Add(1) },
	}

	if err := wrapped.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if got := inner.closed.Load(); got != 1 {
		t.Errorf("inner Close calls = %d, want 1", got)
	}
	if got := cleanupRuns.Load(); got != 1 {
		t.Errorf("cleanup runs after first Close = %d, want 1", got)
	}

	// Second Close: cleanup must not re-run (sync.Once protects it). This
	// guards against double-rm log spam if the exec service Closes twice
	// during teardown.
	_ = wrapped.Close()
	if got := cleanupRuns.Load(); got != 1 {
		t.Errorf("cleanup runs after second Close = %d, want 1 (sync.Once)", got)
	}
}

// TestCleanupExecStreamPropagatesInnerError verifies a Close error from the
// underlying stream is returned to the caller, BUT cleanup still runs. We
// never want a stream error to leak a sidecar.
func TestCleanupExecStreamPropagatesInnerError(t *testing.T) {
	innerErr := errors.New("transport closed")
	inner := &fakeStream{closeErr: innerErr}
	var cleanupRuns atomic.Int32
	wrapped := &cleanupExecStream{
		ExecStream: inner,
		cleanup:    func() { cleanupRuns.Add(1) },
	}

	err := wrapped.Close()
	if !errors.Is(err, innerErr) {
		t.Errorf("expected inner error to propagate, got %v", err)
	}
	if got := cleanupRuns.Load(); got != 1 {
		t.Errorf("cleanup must still run on inner error; got %d runs", got)
	}
}
