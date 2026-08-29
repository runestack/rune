package upgrade

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/runestack/rune/pkg/events"
	"github.com/runestack/rune/pkg/log"
	"github.com/runestack/rune/pkg/types"
)

// Precondition reasons, carried machine-readably in the RPC error (the CLI
// picks its degrade message from the slug, and a future fleet driver can
// aggregate per-node capability without string matching).
const (
	ReasonNoSystemd    = "no-systemd"
	ReasonUnitsMissing = "units-missing"
	ReasonInProgress   = "upgrade-in-progress"
)

// PreconditionError is a staging refusal with a machine-readable reason.
type PreconditionError struct {
	Reason string
	Detail string
}

func (e *PreconditionError) Error() string {
	return fmt.Sprintf("reason=%s: %s", e.Reason, e.Detail)
}

// PreconditionReason marks the error for FailedPrecondition mapping at the
// RPC layer (see pkg/api/service.UpgradeStager).
func (e *PreconditionError) PreconditionReason() string { return e.Reason }

// PreconditionReason extracts the reason slug from an error string formatted
// by PreconditionError (possibly wrapped in a gRPC status message).
func PreconditionReason(msg string) string {
	i := strings.Index(msg, "reason=")
	if i < 0 {
		return ""
	}
	rest := msg[i+len("reason="):]
	if j := strings.IndexAny(rest, ": "); j >= 0 {
		rest = rest[:j]
	}
	return rest
}

// UpgradeUnitPath and UpgradeServicePath are where the applier's units live.
const (
	UpgradeServiceUnit = "runed-upgrade.service"
	UpgradePathUnit    = "runed-upgrade.path"
	unitDir            = "/etc/systemd/system"
)

// Stager stages a server upgrade as the unprivileged service user: it
// downloads and digest-checks the tarball into <data-dir>/upgrade, writes
// the manifest, and creates `ready` — which the root applier's path unit
// consumes. It never touches binaries or systemd itself.
type Stager struct {
	DataDir  string
	NodeID   string
	EventLog events.EventLog
	Logger   log.Logger
	HTTP     *http.Client

	// UnitDir overrides /etc/systemd/system in tests.
	UnitDir string

	// mu is the in-process half of single-flight; the flock below covers
	// other processes. Both only cover stage→ready — the apply window is
	// guarded by the ready-file/applier-active preconditions instead,
	// because applying restarts this process and releases everything.
	mu sync.Mutex
}

// Stage validates preconditions and stages version. sha256 is the digest
// the CLI resolved from checksums.txt; the stager enforces it so obviously
// bad bytes fail minutes earlier than the applier's own independent
// re-verification would catch them.
func (s *Stager) Stage(ctx context.Context, version, sha256, requester string, allowDowngrade bool) error {
	if runtime.GOOS != "linux" {
		return &PreconditionError{Reason: ReasonNoSystemd, Detail: "server self-upgrade requires a linux systemd deployment"}
	}
	if _, err := ParseVersion(version); err != nil {
		return fmt.Errorf("invalid target version: %w", err)
	}
	if len(sha256) != 64 {
		return fmt.Errorf("sha256 must be a 64-char hex digest")
	}
	if err := s.checkUnits(); err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	staging := StagingDir(s.DataDir)
	if err := os.MkdirAll(filepath.Dir(staging), 0o755); err != nil {
		return err
	}
	lock, err := acquireFlock(filepath.Join(filepath.Dir(staging), ".upgrade.lock"))
	if err != nil {
		return &PreconditionError{Reason: ReasonInProgress, Detail: "another upgrade is staging"}
	}
	defer lock.release()

	if v, ok := s.inFlight(staging); ok {
		return &PreconditionError{Reason: ReasonInProgress, Detail: fmt.Sprintf("upgrade to %s is already in progress", v)}
	}

	// Fresh staging dir per attempt: a partial tarball from a killed
	// earlier attempt must not confuse this one.
	if err := os.RemoveAll(staging); err != nil {
		return err
	}
	if err := os.MkdirAll(staging, 0o755); err != nil {
		return err
	}

	asset := ServerAsset(runtime.GOARCH)
	url := DownloadURL(version, asset)
	s.Logger.Info("Staging server upgrade", log.Str("version", version), log.Str("url", url))

	resp, err := fetchWithRetry(ctx, s.httpClient(), url)
	if err != nil {
		_ = os.RemoveAll(staging)
		return err
	}
	defer resp.Body.Close()

	if err := s.checkDisk(staging, resp.ContentLength); err != nil {
		_ = os.RemoveAll(staging)
		return err
	}

	tarPath := filepath.Join(staging, tarballName)
	got, err := downloadTo(tarPath, resp.Body)
	if err != nil {
		_ = os.RemoveAll(staging)
		return fmt.Errorf("downloading %s: %w", asset, err)
	}
	if err := VerifyDigest(got, sha256); err != nil {
		_ = os.RemoveAll(staging)
		return fmt.Errorf("%s: %w", asset, err)
	}

	m := Manifest{Version: version, AllowDowngrade: allowDowngrade, StagedAt: time.Now().UTC()}
	mb, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(staging, manifestName), mb, 0o644); err != nil {
		_ = os.RemoveAll(staging)
		return err
	}

	s.emit("UpgradeStaged", fmt.Sprintf("Server upgrade to %s staged by %s; applying now (the server will restart)", version, requester))

	// `ready` last: it is the trigger. Created synchronously — the reply
	// racing the restart is handled client-side (a transport error on
	// UpgradeServer means "possibly staged, poll anyway").
	if err := os.WriteFile(ReadyPath(s.DataDir), []byte(version+"\n"), 0o644); err != nil {
		_ = os.RemoveAll(staging)
		return fmt.Errorf("creating ready trigger: %w", err)
	}
	return nil
}

// checkUnits verifies the applier units are installed and that the path
// unit watches THIS server's staging dir. A runefile whose data_dir moved
// after install would otherwise stage into a directory nothing watches — a
// silent wedge. The unit-file check (not INVOCATION_ID, which CI runners
// inherit too) is the real capability gate.
func (s *Stager) checkUnits() error {
	dir := s.UnitDir
	if dir == "" {
		dir = unitDir
	}
	pathUnit := filepath.Join(dir, UpgradePathUnit)
	b, err := os.ReadFile(pathUnit)
	if err != nil {
		return &PreconditionError{Reason: ReasonUnitsMissing, Detail: fmt.Sprintf("%s not installed", pathUnit)}
	}
	if _, err := os.Stat(filepath.Join(dir, UpgradeServiceUnit)); err != nil {
		return &PreconditionError{Reason: ReasonUnitsMissing, Detail: fmt.Sprintf("%s not installed", filepath.Join(dir, UpgradeServiceUnit))}
	}
	want := ReadyPath(s.DataDir)
	for _, line := range strings.Split(string(b), "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), "PathExists="); ok {
			if strings.TrimSpace(v) == want {
				return nil
			}
			return &PreconditionError{Reason: ReasonUnitsMissing,
				Detail: fmt.Sprintf("%s watches %s, but this server stages to %s — reinstall the units", pathUnit, strings.TrimSpace(v), want)}
		}
	}
	return &PreconditionError{Reason: ReasonUnitsMissing, Detail: fmt.Sprintf("%s has no PathExists=", pathUnit)}
}

// inFlight reports an upgrade currently staged or applying.
func (s *Stager) inFlight(staging string) (string, bool) {
	if b, err := os.ReadFile(filepath.Join(staging, readyName)); err == nil {
		return strings.TrimSpace(string(b)), true
	}
	// `systemctl is-active` is an unprivileged read; exit 0 means active
	// or activating. This is what guards the apply window — the flock
	// cannot, because the apply restarts the flock holder.
	if out, err := exec.Command("systemctl", "is-active", UpgradeServiceUnit).Output(); err == nil {
		state := strings.TrimSpace(string(out))
		if state == "active" || state == "activating" {
			return "(applying)", true
		}
	}
	return "", false
}

func (s *Stager) checkDisk(dir string, contentLength int64) error {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil || st.Bsize <= 0 {
		return nil // unknown filesystems don't block the upgrade
	}
	free := st.Bavail * uint64(st.Bsize) // #nosec G115 -- Bsize > 0 checked above
	need := uint64(128 << 20)
	if contentLength > 0 {
		need = uint64(contentLength)
	}
	need += 64 << 20 // margin: the staging dir shares a filesystem with the store
	if free < need {
		return fmt.Errorf("not enough free space in %s: need ~%dMB, have %dMB", dir, need>>20, free>>20)
	}
	return nil
}

func (s *Stager) httpClient() *http.Client {
	if s.HTTP != nil {
		return s.HTTP
	}
	return &http.Client{Timeout: 5 * time.Minute}
}

func (s *Stager) emit(reason, message string) {
	if s.EventLog == nil {
		return
	}
	now := time.Now().UTC()
	_ = s.EventLog.Emit(context.Background(), types.Event{
		Kind:      "Node",
		Name:      s.NodeID,
		Level:     types.EventLevelInfo,
		Reason:    reason,
		Message:   message,
		FirstSeen: now,
		LastSeen:  now,
		Count:     1,
	})
}

func downloadTo(path string, r io.Reader) (sha string, err error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return "", err
	}
	defer func() {
		if cerr := f.Close(); err == nil {
			err = cerr
		}
	}()
	limited := io.LimitReader(r, maxArtifactBytes+1)
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, h), limited)
	if err != nil {
		return "", err
	}
	if n > maxArtifactBytes {
		return "", fmt.Errorf("artifact exceeds %dMB cap", maxArtifactBytes>>20)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

type flock struct{ f *os.File }

func acquireFlock(path string) (*flock, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, err
	}
	return &flock{f: f}, nil
}

func (l *flock) release() {
	_ = syscall.Flock(int(l.f.Fd()), syscall.LOCK_UN)
	_ = l.f.Close()
}
