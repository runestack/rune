//go:build linux

package upgrade

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/runestack/rune/internal/config"
	"github.com/runestack/rune/pkg/api/generated"
	"github.com/runestack/rune/pkg/systemd"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// Applier is the root side of RUNE-321: it consumes a staged upgrade,
// independently re-verifies it against the release's published checksums,
// swaps the binaries, refreshes the units, restarts runed and verifies it
// healthy — rolling back on failure. It runs as `runed apply-upgrade` from
// a root oneshot triggered by runed-upgrade.path.
//
// The staging directory is writable by the live, unprivileged service user
// and is treated as hostile input throughout: every file is consumed
// through a validated fd into the root-owned workdir before anything
// believes its content, and no staging path is dereferenced after that.
type Applier struct {
	StagingDir string // <data-dir>/upgrade, from the unit's ExecStart
	ConfigPath string // runefile passed to runed via --config ("" = defaults)
	Runtime    ApplierRuntime

	// unitRefreshNote records a skipped unit refresh so the operator sees
	// it in the result and the event, not only in the journal.
	unitRefreshNote string
	// grpcAddrOverride is runed's bind address when the live unit sets it
	// by flag or environment. runed resolves flag > env > runefile, and
	// polling the wrong address rolls back a healthy upgrade, so the
	// applier has to read the unit rather than the runefile alone.
	grpcAddrOverride string
	// fileCaps is the capability set the replaced binary carried, verbatim;
	// setcap does not survive the copy that installs the new one.
	fileCaps string
}

// ApplierRuntime carries the host facts an apply needs; separated so tests
// can exercise the decision logic without a systemd host.
type ApplierRuntime struct {
	CurrentVersion string // version of this (installed) runed binary
	FloorPath      string
	Now            func() time.Time
}

const (
	verifyBudget         = 180 * time.Second
	verifyHoldDown       = 10 * time.Second
	rollbackVerifyBudget = 60 * time.Second
	runedUnit            = "runed.service"
	runedUnitPath        = "/etc/systemd/system/runed.service"
)

func logf(format string, args ...any) {
	// The oneshot's stdout is the journal; plain lines beat JSON for the
	// operator reading `journalctl -u runed-upgrade`.
	fmt.Printf("[apply-upgrade] "+format+"\n", args...)
}

// Apply runs one apply attempt end to end. The returned error is for the
// exit code; the durable record is the result file, written on every path.
func (a *Applier) Apply(ctx context.Context) error {
	now := a.Runtime.Now
	if now == nil {
		now = time.Now
	}

	if err := os.MkdirAll(Workdir, 0o755); err != nil {
		return err
	}

	// Step 0: consume the trigger and the staged files. Copy (not rename:
	// the staging dir and /run are different filesystems, rename(2) is
	// EXDEV) through validated fds, then unlink the originals so the
	// path unit cannot refire on this attempt's files.
	m, tarLocal, consumeErr := a.consume()
	if consumeErr != nil {
		res := Result{Outcome: "failed", FromVersion: a.Runtime.CurrentVersion, Reason: fmt.Sprintf("consuming staged upgrade: %v", consumeErr), FinishedAt: now().UTC()}
		_ = WriteResult(res)
		return consumeErr
	}
	defer os.Remove(tarLocal)

	from := a.Runtime.CurrentVersion
	logf("staged upgrade: %s -> %s (staged %s)", from, m.Version, m.StagedAt.Format(time.RFC3339))

	if m.Stale(now()) {
		logf("manifest older than the staleness window; consuming as a no-op (a crash mid-staging must not replay at boot)")
		_ = WriteResult(Result{Outcome: "noop", FromVersion: from, ToVersion: m.Version, Reason: "stale manifest", FinishedAt: now().UTC()})
		return nil
	}
	if m.Version == from {
		logf("target equals installed version; nothing to do")
		_ = WriteResult(Result{Outcome: "noop", FromVersion: from, ToVersion: m.Version, Reason: "already at target", FinishedAt: now().UTC()})
		return nil
	}

	if err := a.checkFloor(m); err != nil {
		_ = WriteResult(Result{Outcome: "failed", FromVersion: from, ToVersion: m.Version, Reason: err.Error(), FinishedAt: now().UTC()})
		return err
	}

	binDir, unitVals, err := a.currentUnitValues()
	if err != nil {
		_ = WriteResult(Result{Outcome: "failed", FromVersion: from, ToVersion: m.Version, Reason: err.Error(), FinishedAt: now().UTC()})
		return err
	}
	newRune, newRuned, err := a.verifyAndUnpack(ctx, m, tarLocal)
	if err != nil {
		_ = WriteResult(Result{Outcome: "failed", FromVersion: from, ToVersion: m.Version, Reason: err.Error(), FinishedAt: now().UTC()})
		return err
	}
	defer os.Remove(newRune)
	defer os.Remove(newRuned)
	// Nothing has been stopped and no binary touched up to here: every
	// failure above leaves the running server untouched.

	if err := a.swapAndRestart(ctx, m, binDir, unitVals, newRune, newRuned, from, now); err != nil {
		return err
	}

	if err := WriteFloor(a.Runtime.FloorPath, m.Version); err != nil {
		logf("warning: could not advance version floor: %v", err)
	} else {
		logf("version floor advanced to %s", m.Version)
	}
	_ = WriteResult(Result{Outcome: "success", FromVersion: from, ToVersion: m.Version, Reason: a.unitRefreshNote, FinishedAt: now().UTC()})
	logf("✅ upgrade to %s complete", m.Version)
	return nil
}

// maxConsumeBytes caps what the applier copies into the workdir. The
// workdir is tmpfs (RAM), so this bounds both a legit apply's peak and a
// hostile service account ballooning /run before verification. Real
// server tarballs are ~40-60 MB.
const maxConsumeBytes = 256 << 20

// consume moves manifest+tarball into the workdir via validated fds and
// removes ready/manifest/tarball from the staging dir — on EVERY outcome.
// An error that left `ready` behind would make systemd refire the oneshot
// in a loop until the path unit trips its trigger limit and lands in
// failed state, after which no future upgrade ever fires; a failed consume
// must therefore still consume the trigger (the operator re-stages).
func (a *Applier) consume() (*Manifest, string, error) {
	// The staging directory itself is rune-writable, so even the unlinks
	// must not traverse it by path (a swapped-in symlink would point
	// root's unlinkat elsewhere): open it once, O_NOFOLLOW, and do
	// everything through the dirfd. Residual (availability-only): if the
	// service account replaces the whole dir with a symlink, this open
	// fails ELOOP before the unlink defers exist and `ready` survives at
	// the symlink's target — the refire is then contained by systemd's
	// trigger limit rather than consumed.
	dirfd, err := syscall.Open(a.StagingDir, syscall.O_DIRECTORY|syscall.O_NOFOLLOW|syscall.O_RDONLY|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, "", fmt.Errorf("staging dir: %w", err)
	}
	defer syscall.Close(dirfd)
	defer func() {
		_ = syscall.Unlinkat(dirfd, manifestName)
		_ = syscall.Unlinkat(dirfd, tarballName)
		_ = syscall.Unlinkat(dirfd, readyName)
	}()

	var dirStat syscall.Stat_t
	if err := syscall.Fstat(dirfd, &dirStat); err != nil {
		return nil, "", fmt.Errorf("staging dir: %w", err)
	}

	manifestBytes, err := consumeFileAt(dirfd, a.StagingDir, manifestName, dirStat.Uid, "")
	if err != nil {
		return nil, "", err
	}
	tarLocal := filepath.Join(Workdir, tarballName)
	_ = os.Remove(tarLocal)
	if _, err := consumeFileAt(dirfd, a.StagingDir, tarballName, dirStat.Uid, tarLocal); err != nil {
		return nil, "", err
	}

	m, err := parseManifest(manifestBytes)
	if err != nil {
		return nil, "", err
	}
	return m, tarLocal, nil
}

// consumeFileAt opens name relative to the staging dirfd with O_NOFOLLOW,
// validates the fd (regular file, owned by the staging dir's owner, link
// count 1) and either returns its bytes (dst=="") or copies the fd's
// content to dst (root-owned, O_EXCL). It never touches a staging path by
// name — the service user owns those inodes and can swap them at any
// moment; the fd is the only trustworthy handle.
func consumeFileAt(dirfd int, dir, name string, wantUID uint32, dst string) ([]byte, error) {
	fd, err := syscall.Openat(dirfd, name, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("opening %s/%s: %w", dir, name, err)
	}
	f := os.NewFile(uintptr(fd), name)
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok || !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("%s/%s: not a regular file", dir, name)
	}
	if st.Uid != wantUID {
		return nil, fmt.Errorf("%s/%s: owned by uid %d, want %d", dir, name, st.Uid, wantUID)
	}
	if st.Nlink != 1 {
		return nil, fmt.Errorf("%s/%s: link count %d, want 1 (hardlink games)", dir, name, st.Nlink)
	}
	if dst == "" {
		return io.ReadAll(io.LimitReader(f, 1<<20))
	}
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, err
	}
	defer out.Close()
	n, err := io.Copy(out, io.LimitReader(f, maxConsumeBytes+1))
	if err != nil {
		_ = os.Remove(dst)
		return nil, err
	}
	if n > maxConsumeBytes {
		_ = os.Remove(dst)
		// Refuse rather than truncate: a silently truncated copy would
		// die later as a misleading digest mismatch.
		return nil, fmt.Errorf("%s/%s exceeds the %dMB consume cap", dir, name, maxConsumeBytes>>20)
	}
	return nil, nil
}

func (a *Applier) checkFloor(m *Manifest) error {
	// The downgrade opt-in is enforced here, not only displayed: a direct
	// RPC (or forged manifest) with an old-but-above-floor target and no
	// opt-in must not silently downgrade. The floor below remains the
	// control the in-scope attackers cannot write.
	if isDowngrade(m.Version, a.Runtime.CurrentVersion) && !m.AllowDowngrade {
		return fmt.Errorf("target %s is older than the running %s and the request did not opt into a downgrade", m.Version, a.Runtime.CurrentVersion)
	}

	floor, err := ReadFloor(a.Runtime.FloorPath)
	if err != nil {
		return err // includes ErrFloorUnparseable: corruption fails closed
	}
	if floor == nil {
		logf("⚠️  no version floor at %s (pre-seeding host) — allowing; the floor will be seeded on success", a.Runtime.FloorPath)
		return nil
	}
	target, err := ParseVersion(m.Version)
	if err != nil {
		return err
	}
	if target.LessThan(floor) {
		return fmt.Errorf("target %s is below the version floor %s; for a deliberate rollback, run (as root):  echo %s > %s   and retry",
			m.Version, floor.Original(), m.Version, a.Runtime.FloorPath)
	}
	return nil
}

// isDowngrade reports target < current; unparseable versions (a "dev"
// from-source build) report false and leave the decision to the floor.
func isDowngrade(target, current string) bool {
	cmp, err := CompareVersions(target, current)
	return err == nil && cmp < 0
}

// targetRequiresReady decides whether the post-restart verify demands the
// `ready` flag. Any target NEWER than this (already ready-aware) applier
// carries the flag; a downgrade target may predate it — today every
// released build does — and requiring it there would burn the verify
// budget and roll back a working deliberate downgrade. Unparseable
// versions default to requiring it.
func targetRequiresReady(target, current string) bool {
	return !isDowngrade(target, current)
}

// verifyAndUnpack is the trust anchor: the digest that authorizes the swap
// comes from the release's checksums.txt fetched by THIS root process over
// TLS from the pinned repo, never from anything the service user wrote. It
// carries the same retry budget as the stager because it hits the same CDN
// edge that 504s on freshly published releases.
func (a *Applier) verifyAndUnpack(ctx context.Context, m *Manifest, tarLocal string) (newRune, newRuned string, err error) {
	hc := &http.Client{Timeout: 2 * time.Minute}
	cs, err := FetchChecksums(ctx, hc, m.Version)
	if err != nil {
		return "", "", err
	}
	// This applier binary is the installed runed, so its own GOARCH is
	// the host arch by construction.
	want, err := cs.Digest(ServerAsset(runtime.GOARCH))
	if err != nil {
		return "", "", err
	}
	got, err := SHA256File(tarLocal)
	if err != nil {
		return "", "", err
	}
	if err := VerifyDigest(got, want); err != nil {
		return "", "", fmt.Errorf("staged tarball: %w", err)
	}
	logf("sha256 verified against %s from the %s release", ChecksumsAsset, m.Version)

	newRune = filepath.Join(Workdir, "rune.new")
	newRuned = filepath.Join(Workdir, "runed.new")
	if err := untarBinaries(tarLocal, map[string]string{"rune": newRune, "runed": newRuned}); err != nil {
		return "", "", err
	}
	// The workdir is tmpfs (RAM): drop the tarball as soon as it is
	// unpacked to bound peak usage.
	_ = os.Remove(tarLocal)
	return newRune, newRuned, nil
}

// swapAndRestart performs the mutating half: backup, swap, unit refresh,
// restart, verify — and rolls back on verification failure.
func (a *Applier) swapAndRestart(ctx context.Context, m *Manifest, binDir string, vals systemd.UnitOptions, newRune, newRuned, from string, now func() time.Time) error {
	a.detectFileCaps(binDir)
	backupRune := filepath.Join(binDir, ".rune.bak")
	backupRuned := filepath.Join(binDir, ".runed.bak")
	if err := copyFile(filepath.Join(binDir, "rune"), backupRune, 0o755); err != nil && !os.IsNotExist(err) {
		return a.fail(m, from, now, fmt.Errorf("backing up rune: %w", err))
	}
	if err := copyFile(filepath.Join(binDir, "runed"), backupRuned, 0o755); err != nil {
		return a.fail(m, from, now, fmt.Errorf("backing up runed: %w", err))
	}
	logf("backed up current binaries to %s / %s", backupRune, backupRuned)

	if err := installFile(newRune, filepath.Join(binDir, "rune")); err != nil {
		return a.fail(m, from, now, err)
	}
	if err := installFile(newRuned, filepath.Join(binDir, "runed")); err != nil {
		_ = installFile(backupRune, filepath.Join(binDir, "rune"))
		return a.fail(m, from, now, err)
	}
	logf("installed %s binaries to %s", m.Version, binDir)

	unitBackedUp := a.refreshUnits(binDir, vals)
	a.applyCaps(binDir)

	if err := systemctl("daemon-reload"); err != nil {
		logf("warning: daemon-reload: %v", err)
	}
	// The path unit was just rewritten from the new binary's template;
	// restart it so systemd watches the unit as rendered rather than as it
	// was loaded.
	if err := systemctl("restart", UpgradePathUnit); err != nil {
		logf("warning: could not re-arm %s: %v", UpgradePathUnit, err)
	}
	logf("restarting %s — the API drops here", runedUnit)
	if err := systemctl("restart", runedUnit); err != nil {
		return a.rollback(ctx, m, binDir, backupRune, backupRuned, unitBackedUp, from, now, fmt.Errorf("systemctl restart: %w", err))
	}

	if err := a.verify(ctx, m.Version, targetRequiresReady(m.Version, from), verifyBudget); err != nil {
		return a.rollback(ctx, m, binDir, backupRune, backupRuned, unitBackedUp, from, now, err)
	}
	return nil
}

func (a *Applier) fail(m *Manifest, from string, now func() time.Time, err error) error {
	_ = WriteResult(Result{Outcome: "failed", FromVersion: from, ToVersion: m.Version, Reason: err.Error(), FinishedAt: now().UTC()})
	return err
}

func (a *Applier) rollback(ctx context.Context, m *Manifest, binDir, backupRune, backupRuned string, unitBackedUp bool, from string, now func() time.Time, cause error) error {
	logf("❌ verification failed: %v", cause)
	logf("rolling back to %s", from)
	_ = installFile(backupRune, filepath.Join(binDir, "rune"))
	_ = installFile(backupRuned, filepath.Join(binDir, "runed"))
	if unitBackedUp {
		_ = copyFile(runedUnitPath+".bak", runedUnitPath, 0o644)
	}
	for _, u := range []string{UpgradeServiceUnit, UpgradePathUnit} {
		p := filepath.Join(unitDir, u)
		if _, err := os.Stat(p + ".bak"); err == nil {
			_ = copyFile(p+".bak", p, 0o644)
		}
	}
	a.applyCaps(binDir)
	_ = systemctl("daemon-reload")
	_ = systemctl("restart", runedUnit)
	// The rolled-back binary may predate the `ready` field, so rollback
	// verification is version-only.
	verr := a.verify(ctx, from, false, rollbackVerifyBudget)
	outcome := "rolled-back"
	reason := cause.Error()
	if verr != nil {
		outcome = "failed"
		reason = fmt.Sprintf("%v; rollback verification also failed: %v — backups remain at %s/.{rune,runed}.bak", cause, verr, binDir)
		logf("❌ %s", reason)
	} else {
		logf("rolled back to %s and verified", from)
	}
	_ = WriteResult(Result{Outcome: outcome, FromVersion: from, ToVersion: m.Version, Reason: reason, FinishedAt: now().UTC()})
	return cause
}

// refreshUnits rewrites runed.service from the NEW binary's print-systemd,
// preserving the live unit's user/binary/config values (a flagless render
// would clobber a custom install — see ParseUnitShow), and re-renders the
// applier's own units so they are not frozen at bootstrap forever. The
// runed unit is skipped unless systemd loads it from exactly that path; the
// applier's own units are refreshed either way. Returns whether the runed
// unit was backed up, for rollback.
func (a *Applier) refreshUnits(binDir string, vals systemd.UnitOptions) bool {
	newBinary := filepath.Join(binDir, "runed")
	render := func(args ...string) (string, error) {
		out, err := exec.Command(newBinary, args...).Output()
		return string(out), err
	}

	unitBackedUp := false
	// Writing /etc/systemd/system when the live unit is a vendor unit under
	// /lib does not refresh it, it shadows it — and rollback would have no
	// backup to restore, leaving the old binary under the new unit.
	// "Don't know" is treated as "don't touch it": systemctl is known to
	// work by now (currentUnitValues hard-errors otherwise), so an empty
	// answer means a transient unit or no unit at all — and a
	// generator-written one answers with its /run path, which is equally
	// not ours to overwrite. Writing /etc would shadow rather than refresh,
	// with no backup for rollback to restore. The applier's own units are re-rendered below
	// regardless: those always live in /etc and are always ours.
	skipRunedUnit := liveFragmentPath() != runedUnitPath
	if skipRunedUnit {
		a.unitRefreshNote = "left " + runedUnitPath + " unchanged: systemd loads runed.service from elsewhere"
		logf("⚠️  %s", a.unitRefreshNote)
	}
	newUnit, err := render("print-systemd", "--user", vals.User, "--group", vals.Group, "--binary", vals.BinaryPath, "--config", vals.ConfigPath)
	if err != nil && !skipRunedUnit {
		logf("warning: rendering refreshed unit failed (%v); leaving %s as is", err, runedUnitPath)
	} else if !skipRunedUnit {
		current, _ := os.ReadFile(runedUnitPath)
		reason := unitRefreshUnsafe(string(current), newUnit)
		switch {
		case string(current) == newUnit:
			// unchanged
		case reason != "":
			// Authored by something other than this binary; see
			// unitRefreshUnsafe.
			a.unitRefreshNote = "left " + runedUnitPath + " unchanged: " + reason
			logf("⚠️  %s", a.unitRefreshNote)
			logf("    apply any new directives by hand, or use a drop-in under %s.d/", runedUnitPath)
		case len(current) > 0 && copyFile(runedUnitPath, runedUnitPath+".bak", 0o644) != nil:
			// Never replace a unit we could not back up — a later
			// rollback would have nothing to restore.
			logf("warning: could not back up %s; leaving it unchanged", runedUnitPath)
		default:
			unitBackedUp = len(current) > 0
			if err := os.WriteFile(runedUnitPath, []byte(newUnit), 0o644); err != nil {
				// O_TRUNC may have left a partial unit; put the backup
				// straight back rather than clearing unitBackedUp — a
				// failed write is exactly when the backup matters.
				logf("warning: writing refreshed unit: %v", err)
				if unitBackedUp {
					_ = copyFile(runedUnitPath+".bak", runedUnitPath, 0o644)
				}
			} else {
				logf("refreshed %s (previous at %s.bak); changes:", runedUnitPath, runedUnitPath)
				journalDiff(string(current), newUnit)
			}
		}
	}

	svc, err1 := render("print-systemd", "--upgrade-units", "--staging", a.StagingDir, "--binary", vals.BinaryPath, "--config", vals.ConfigPath)
	path, err2 := render("print-systemd", "--upgrade-path-unit", "--staging", a.StagingDir)
	if err1 == nil && err2 == nil {
		svcPath := filepath.Join(unitDir, UpgradeServiceUnit)
		pathPath := filepath.Join(unitDir, UpgradePathUnit)
		// Backed up alongside runed.service: a rollback that left the new
		// release's upgrade units in place would hand the old binary an
		// ExecStart it may not accept, wedging every future upgrade.
		_ = copyFile(svcPath, svcPath+".bak", 0o644)
		_ = copyFile(pathPath, pathPath+".bak", 0o644)
		_ = os.WriteFile(svcPath, []byte(svc), 0o644)
		_ = os.WriteFile(pathPath, []byte(path), 0o644)
		logf("refreshed own units from the new binary")
	} else {
		logf("warning: new binary cannot render upgrade units; leaving them as installed")
	}
	return unitBackedUp
}

// liveFragmentPath returns the unit file systemd actually loaded runed
// from, or "" when it cannot be determined.
func liveFragmentPath() string {
	out, err := exec.Command("systemctl", "show", runedUnit, "-p", "FragmentPath", "--value").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// parseGetcap extracts the capability set from getcap output for binPath,
// or "" when there is none.
//
// Every clause, not just the last: a file can carry
// "cap_net_bind_service=ep cap_sys_admin=p", and setcap replaces the whole
// set rather than adding to it, so keeping one clause silently drops the
// rest. Older libcap prints "<path> = <caps>", so a bare "=" is dropped.
func parseGetcap(out, binPath string) string {
	rest := strings.TrimSpace(out)
	// Strip the path we asked about rather than splitting on whitespace:
	// a bin dir containing a space would otherwise fold part of the path
	// into the capability string, setcap would reject it, and the upgrade
	// would report success with runed unable to bind :80/:443.
	rest, ok := strings.CutPrefix(rest, binPath)
	if !ok {
		// Not the file we asked about; anything else in this line is not
		// a capability set we should hand to setcap.
		return ""
	}
	rest = strings.TrimSpace(rest)
	rest = strings.TrimSpace(strings.TrimPrefix(rest, "="))
	if !strings.Contains(rest, "cap_") {
		return ""
	}
	return rest
}

// unitRefreshUnsafe reports why the live runed unit must not be replaced by
// a print-systemd render, or "" when the refresh is safe.
//
// The check is derived from the render rather than a hardcoded list, so it
// stays correct as the template grows: a directive the live unit sets and
// the render does not is one the refresh would silently drop. It compares
// names and ExecStart flags, not values — a hand-tuned RestartSec= is reset
// to the template's, which journalDiff records.
//
// Units written by an out-of-tree provisioner are the real case:
// EnvironmentFile= carrying credentials, or no User= because runed runs as
// root there. Dropping either while still reporting a successful upgrade is
// the worst outcome this applier can produce.
func unitRefreshUnsafe(current, rendered string) string {
	if strings.TrimSpace(current) == "" {
		return ""
	}
	cur, ren := unitDirectives(current), unitDirectives(rendered)
	var dropped []string
	for d := range cur {
		if !ren[d] && !benignDirectives[d] {
			dropped = append(dropped, d)
		}
	}
	if len(dropped) > 0 {
		sort.Strings(dropped)
		return "it sets " + strings.Join(dropped, ", ") + ", which this build's unit template cannot express"
	}
	if !cur["User"] && ren["User"] {
		return "it runs runed as root (no User=), and the refresh would change the service user"
	}
	return ""
}

// benignDirectives are inert: losing one changes nothing about how the
// service runs, so it must not disable the refresh for ever over a cosmetic
// line. examples/config/runed.service ships Documentation=. Alias= is
// deliberately absent: it creates a working second name for the unit.
var benignDirectives = map[string]bool{
	"Documentation": true,
}

// joinContinuations folds systemd's backslash line continuations into one
// logical line each. Skipping the wrapped lines instead would drop the
// flags they carry — a long ExecStart is normally written wrapped, and
// that is precisely where --data-dir lives.
func joinContinuations(unit string) []string {
	var out []string
	var cur strings.Builder
	for _, line := range strings.Split(unit, "\n") {
		t := strings.TrimSpace(line)
		if strings.HasSuffix(t, "\\") {
			cur.WriteString(strings.TrimSuffix(t, "\\"))
			cur.WriteString(" ")
			continue
		}
		if cur.Len() > 0 {
			cur.WriteString(t)
			out = append(out, cur.String())
			cur.Reset()
			continue
		}
		out = append(out, t)
	}
	if cur.Len() > 0 {
		out = append(out, cur.String())
	}
	return out
}

// unitDirectives returns what a unit file sets: every directive name, plus
// each flag in an ExecStart command line as "ExecStart --<flag>", dashes
// normalised.
//
// The flags matter as much as the directive names. runed takes two dozen
// daemon flags and the template reproduces one of them, so a unit started with
// e.g. --data-dir would come back pointing at the default store: the server
// would restart on an empty database, pass verification, and report a
// successful upgrade.
func unitDirectives(unit string) map[string]bool {
	out := map[string]bool{}
	for _, line := range joinContinuations(unit) {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") || strings.HasPrefix(line, "[") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		k = strings.TrimSpace(k)
		out[k] = true
		if k == "ExecStart" {
			for _, f := range strings.Fields(v) {
				if !strings.HasPrefix(f, "-") {
					continue
				}
				// "--flag=value" and "--flag value" are the same flag, and
				// Go's flag package accepts "-flag" for "--flag".
				name, _, _ := strings.Cut(f, "=")
				out["ExecStart --"+strings.TrimLeft(name, "-")] = true
			}
		}
	}
	return out
}

// applyCaps restores whichever capability mechanism the host was using. A
// unit declaring AmbientCapabilities= is the source of truth, and file caps
// on the binary suppress ambient caps, so strip them. Otherwise the file
// caps are what let runed bind :80/:443 — and they are xattrs on the inode,
// so the copy that replaced the binary dropped them; re-apply.
func (a *Applier) applyCaps(binDir string) {
	runedBin := filepath.Join(binDir, "runed")
	current, err := os.ReadFile(runedUnitPath)
	if err == nil && strings.Contains(string(current), "AmbientCapabilities=") {
		_ = exec.Command("setcap", "-r", runedBin).Run()
		return
	}
	if a.fileCaps != "" {
		if err := exec.Command("setcap", a.fileCaps, runedBin).Run(); err != nil {
			logf("warning: could not re-apply %q to %s: %v", a.fileCaps, runedBin, err)
		} else {
			logf("re-applied %q to %s", a.fileCaps, runedBin)
		}
	}
}

// detectFileCaps records the capability set the installed binary carries,
// before it is replaced. The whole set, not just the one capability this
// project usually grants — setcap replaces rather than adds, so
// re-applying a narrowed set would silently drop the rest on a host that
// had more.
func (a *Applier) detectFileCaps(binDir string) {
	out, err := exec.Command("getcap", filepath.Join(binDir, "runed")).Output()
	if err != nil {
		return
	}
	a.fileCaps = parseGetcap(string(out), filepath.Join(binDir, "runed"))
}

// currentUnitValues reads User/Group/ExecStart from the live unit via
// `systemctl show` (canonical output — no unit-file parsing) and derives
// the bin dir, binary path and --config argument.
func (a *Applier) currentUnitValues() (string, systemd.UnitOptions, error) {
	vals := systemd.DefaultUnitOptions()
	vals.ConfigPath = a.ConfigPath

	out, err := exec.Command("systemctl", "show", runedUnit, "-p", "User", "-p", "Group", "-p", "ExecStart", "-p", "Environment").Output()
	if err != nil {
		return "", vals, fmt.Errorf("systemctl show %s: %w", runedUnit, err)
	}
	binPath, cfg, grpcAddr := ParseUnitShow(string(out), &vals)
	if binPath != "" {
		vals.BinaryPath = binPath
	}
	if cfg != "" {
		// The runefile runed actually reads, which is not necessarily the
		// one the upgrade unit was installed with.
		vals.ConfigPath = cfg
		a.ConfigPath = cfg
	}
	a.grpcAddrOverride = grpcAddr
	return filepath.Dir(vals.BinaryPath), vals, nil
}

// verify polls GetServerVersion on the server's own gRPC address until it
// answers with the expected version — and, when requireReady is set, with
// the startup-phase ready flag — then holds for stability: two positive
// answers with systemd's NRestarts unchanged between them. Version alone
// is not a health check: the API serves before the node phase finishes,
// and a crash-looping binary answers with the new version for a few
// seconds per loop.
func (a *Applier) verify(ctx context.Context, wantVersion string, requireReady bool, budget time.Duration) error {
	addr, err := a.grpcAddr()
	if err != nil {
		return err
	}
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return err
	}
	defer conn.Close()
	hc := generated.NewHealthServiceClient(conn)

	deadline := time.Now().Add(budget)
	probe := func() bool {
		cctx, cancel := context.WithTimeout(ctx, 3*time.Second)
		defer cancel()
		resp, err := hc.GetServerVersion(cctx, &generated.GetServerVersionRequest{})
		if err != nil {
			return false
		}
		if resp.GetVersion() != wantVersion {
			return false
		}
		return !requireReady || resp.GetReady()
	}

	for !probe() {
		if time.Now().After(deadline) {
			return fmt.Errorf("server did not answer healthy as %s within %s (addr %s)", wantVersion, budget, addr)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}

	before := nRestarts()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(verifyHoldDown):
	}
	if !probe() {
		return fmt.Errorf("server answered as %s but did not stay up through the %s hold-down", wantVersion, verifyHoldDown)
	}
	if after := nRestarts(); after != before {
		return fmt.Errorf("runed restarted during the hold-down (NRestarts %s -> %s): crash loop", before, after)
	}
	logf("server answering healthy as %s (stable over %s)", wantVersion, verifyHoldDown)
	return nil
}

// grpcAddr derives the poll address from the same runefile the unit passes
// via --config; the bind address is configurable, so localhost is a guess
// only when the config binds a wildcard.
func (a *Applier) grpcAddr() (string, error) {
	addr := a.grpcAddrOverride
	if addr == "" {
		cfg, err := config.Load(a.ConfigPath)
		if err != nil {
			return "", fmt.Errorf("loading runefile for the verify address: %w", err)
		}
		addr = cfg.Server.GRPCAddr
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return "", fmt.Errorf("grpc address %q: %w", addr, err)
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	return net.JoinHostPort(host, port), nil
}

func nRestarts() string {
	out, err := exec.Command("systemctl", "show", runedUnit, "-p", "NRestarts", "--value").Output()
	if err != nil {
		return "?"
	}
	return strings.TrimSpace(string(out))
}

func systemctl(args ...string) error {
	out, err := exec.Command("systemctl", args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("systemctl %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

func parseManifest(b []byte) (*Manifest, error) {
	var m Manifest
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("parsing upgrade manifest: %w", err)
	}
	if m.Version == "" {
		return nil, fmt.Errorf("upgrade manifest has no version")
	}
	// The version is interpolated into the release URL this applier fetches
	// its checksums from — the anchor for everything it installs. A version
	// that is not a semver tag could redirect that fetch, so it never
	// reaches DownloadURL unparsed. checkFloor cannot be relied on for this:
	// it returns early on a host with no floor.
	if _, err := ParseVersion(m.Version); err != nil {
		return nil, fmt.Errorf("upgrade manifest: %w", err)
	}
	return &m, nil
}

// installFile is atomic: write beside dst, then rename over.
func installFile(src, dst string) error {
	tmp := dst + ".new"
	if err := copyFile(src, tmp, 0o755); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, dst); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

func journalDiff(oldContent, newContent string) {
	oldLines := strings.Split(oldContent, "\n")
	newLines := strings.Split(newContent, "\n")
	oldSet := make(map[string]bool, len(oldLines))
	for _, l := range oldLines {
		oldSet[l] = true
	}
	newSet := make(map[string]bool, len(newLines))
	for _, l := range newLines {
		newSet[l] = true
	}
	for _, l := range oldLines {
		if !newSet[l] && strings.TrimSpace(l) != "" {
			logf("  - %s", l)
		}
	}
	for _, l := range newLines {
		if !oldSet[l] && strings.TrimSpace(l) != "" {
			logf("  + %s", l)
		}
	}
}
