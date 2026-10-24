package upgrade

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
)

// ErrBinaryNotWritable means the CLI cannot replace its own binary — a
// root-installed binary run from a non-root shell. The CLI turns this into
// the exact command to run; we never escalate privileges ourselves.
type ErrBinaryNotWritable struct {
	Path string
	Err  error
}

func (e *ErrBinaryNotWritable) Error() string {
	return fmt.Sprintf("cannot write %s: %v", e.Path, e.Err)
}

func (e *ErrBinaryNotWritable) Unwrap() error { return e.Err }

// SelfUpdate downloads the CLI tarball for this platform, verifies it
// against the provided digest, and atomically replaces the running binary
// (write beside it, rename over — the standard self-replace).
func SelfUpdate(ctx context.Context, hc *http.Client, tag, wantSHA string) (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	if exe, err = filepath.EvalSymlinks(exe); err != nil {
		return "", err
	}

	asset := CLIAsset(runtime.GOOS, runtime.GOARCH)
	resp, err := fetchWithRetry(ctx, hc, DownloadURL(tag, asset))
	if err != nil {
		return exe, err
	}
	defer resp.Body.Close()

	tmpDir, err := os.MkdirTemp("", "rune-upgrade-*")
	if err != nil {
		return exe, err
	}
	defer os.RemoveAll(tmpDir)

	tarPath := filepath.Join(tmpDir, asset)
	got, err := downloadTo(tarPath, resp.Body)
	if err != nil {
		return exe, err
	}
	if err := VerifyDigest(got, wantSHA); err != nil {
		return exe, fmt.Errorf("%s: %w", asset, err)
	}

	newBin := filepath.Join(tmpDir, "rune")
	if err := untarBinary(tarPath, "rune", newBin); err != nil {
		return exe, err
	}

	staged := filepath.Join(filepath.Dir(exe), ".rune.new")
	if err := copyExecutable(newBin, staged); err != nil {
		return exe, &ErrBinaryNotWritable{Path: exe, Err: err}
	}
	if err := os.Rename(staged, exe); err != nil {
		_ = os.Remove(staged)
		return exe, &ErrBinaryNotWritable{Path: exe, Err: err}
	}
	return exe, nil
}
