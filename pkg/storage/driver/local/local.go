// Package local implements two storage Driver names that ship with the
// Rune binary:
//
//   - "local"      — Rune-managed directory under runefile [storage].localVolumeRoot.
//     Owns directory lifecycle; honours reclaimPolicy: delete with
//     bounded rm -rf; supports filesystem-level snapshots.
//
//   - "local-host" — Operator-owned host path declared on Volume.parameters.hostPath.
//     Validated against runefile [storage].hostPathAllowlist.
//     Delete is a no-op; reclaimPolicy: delete is rejected.
//     No snapshot support.
//
// The two driver names share helpers (path validation, snapshot copy, mount
// target rewriting) but advertise different Capabilities. Each is its own
// row in the registry — there is no "subdriver" abstraction. Adding a third
// local-style driver would mean adding a third Register() call.
//
// Introduced in RUNE-069.
package local

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/runestack/rune/pkg/storage/driver"
	"github.com/runestack/rune/pkg/types"
)

// Driver names registered by this package.
const (
	DriverNameLocal     = "local"
	DriverNameLocalHost = "local-host"
)

// Config is the runefile [storage] section the factory consumes.
type Config struct {
	// LocalVolumeRoot is the parent directory the "local" driver creates
	// per-volume directories under. Defaults to /var/lib/rune/volumes when
	// empty.
	LocalVolumeRoot string

	// HostPathAllowlist is the set of allowed host-path prefixes for the
	// "local-host" driver. Empty allowlist denies every host path (safe
	// default).
	HostPathAllowlist []string

	// AllowCreateMissing, when true, lets the "local-host" driver auto-mkdir
	// host paths that don't exist (still subject to the allowlist). Default
	// false; runed --dev-mode flips this on for ~/.rune/volumes only.
	AllowCreateMissing bool

	// PreserveOnDelete converts reclaimPolicy: delete into retain for the
	// "local" (managed) driver. Belt-and-braces for operators who want
	// cascade-delete to never wipe data. local-host is unaffected (it
	// already rejects reclaimPolicy: delete).
	PreserveOnDelete bool

	// SnapshotRoot is the parent directory snapshots are stored under for
	// the "local" driver. Defaults to <LocalVolumeRoot>/.snapshots.
	SnapshotRoot string
}

func init() {
	driver.Register(DriverNameLocal, factoryManaged)
	driver.Register(DriverNameLocalHost, factoryHost)
}

// factoryManaged is the registry factory for the "local" driver name.
func factoryManaged(raw map[string]any) (driver.Driver, error) {
	cfg, err := parseConfig(raw)
	if err != nil {
		return nil, err
	}
	if cfg.LocalVolumeRoot == "" {
		cfg.LocalVolumeRoot = "/var/lib/rune/volumes"
	}
	if cfg.SnapshotRoot == "" {
		cfg.SnapshotRoot = filepath.Join(cfg.LocalVolumeRoot, ".snapshots")
	}
	return &managedDriver{cfg: cfg}, nil
}

// factoryHost is the registry factory for the "local-host" driver name.
func factoryHost(raw map[string]any) (driver.Driver, error) {
	cfg, err := parseConfig(raw)
	if err != nil {
		return nil, err
	}
	return &hostDriver{cfg: cfg}, nil
}

// parseConfig is a minimal map[string]any decoder. We avoid pulling in a
// reflective config library to keep the storage tree dependency-free; the
// runefile loader is responsible for type-coercing strings/bools/slices
// before handing them here.
//
// Keys are matched case-insensitively because the runefile loader
// (viper) lowercases nested map keys, while in-process callers and
// driver tests typically use the camelCase spelling published in the
// design doc. The lookup checks the lowercased form first and falls
// back to the original spelling for callers who pre-normalised.
func parseConfig(raw map[string]any) (*Config, error) {
	cfg := &Config{}
	if raw == nil {
		return cfg, nil
	}
	// Build a case-folded view once so each lookup is a single map hit.
	lc := make(map[string]any, len(raw))
	for k, v := range raw {
		lc[strings.ToLower(k)] = v
	}
	get := func(camel string) (any, bool) {
		if v, ok := lc[strings.ToLower(camel)]; ok {
			return v, true
		}
		return nil, false
	}

	if v, ok := get("localVolumeRoot"); ok {
		if s, ok := v.(string); ok {
			cfg.LocalVolumeRoot = s
		}
	}
	if v, ok := get("snapshotRoot"); ok {
		if s, ok := v.(string); ok {
			cfg.SnapshotRoot = s
		}
	}
	if v, ok := get("allowCreateMissing"); ok {
		if b, ok := v.(bool); ok {
			cfg.AllowCreateMissing = b
		}
	}
	if v, ok := get("preserveOnDelete"); ok {
		if b, ok := v.(bool); ok {
			cfg.PreserveOnDelete = b
		}
	}
	if v, ok := get("hostPathAllowlist"); ok {
		switch vv := v.(type) {
		case []string:
			cfg.HostPathAllowlist = append(cfg.HostPathAllowlist, vv...)
		case []any:
			for _, e := range vv {
				s, ok := e.(string)
				if !ok {
					return nil, fmt.Errorf("%w: hostPathAllowlist entry %v is not a string", driver.ErrInvalidConfig, e)
				}
				cfg.HostPathAllowlist = append(cfg.HostPathAllowlist, s)
			}
		}
	}
	return cfg, nil
}

// ============================================================================
// "local" — managed driver
// ============================================================================

type managedDriver struct {
	cfg *Config
	mu  sync.Mutex // guards filesystem mutations within the same process
}

func (d *managedDriver) Name() string { return DriverNameLocal }

func (d *managedDriver) Capabilities() driver.Capabilities {
	return driver.Capabilities{
		AccessModes:  []types.AccessMode{types.AccessModeRWO},
		Snapshots:    true,
		Expand:       false,
		OnlineExpand: false,
		BlockDevice:  false,
		TopologyKeys: []string{types.TopologyLabelRegion, types.TopologyLabelHostPathRoot},
	}
}

func (d *managedDriver) Provision(ctx context.Context, opctx driver.OpContext, req driver.ProvisionRequest) (driver.VolumeHandle, error) {
	if opctx.Volume == nil {
		return "", fmt.Errorf("local: Provision: OpContext.Volume is required: %w", driver.ErrInvalidConfig)
	}
	if err := assertAccessMode(opctx.Volume.AccessMode, d.Capabilities()); err != nil {
		return "", err
	}
	d.mu.Lock()
	defer d.mu.Unlock()

	dir, err := d.volumeDir(opctx.Volume)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return "", fmt.Errorf("local: mkdir %q: %w", dir, err)
	}
	return driver.VolumeHandle(dir), nil
}

func (d *managedDriver) Delete(ctx context.Context, opctx driver.OpContext, handle driver.VolumeHandle) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	if d.cfg.PreserveOnDelete {
		// Operator opted into "delete -> retain" conversion. Nothing to do.
		return nil
	}
	dir := string(handle)
	if dir == "" {
		return nil
	}
	// Bounded rm -rf: refuse to touch anything outside LocalVolumeRoot.
	if _, err := os.Lstat(dir); errors.Is(err, os.ErrNotExist) {
		return nil
	}
	abs, err := d.ownPath(opctx, dir)
	if err != nil {
		return fmt.Errorf("local: refusing to delete: %w", err)
	}
	if err := os.RemoveAll(abs); err != nil {
		return fmt.Errorf("local: rm -rf %q: %w", abs, err)
	}
	return nil
}

func (d *managedDriver) Attach(ctx context.Context, opctx driver.OpContext, handle driver.VolumeHandle, node driver.NodeID) (driver.DevicePath, error) {
	// Local volumes have no separate attach step.
	return "", nil
}

func (d *managedDriver) Detach(ctx context.Context, opctx driver.OpContext, handle driver.VolumeHandle, node driver.NodeID) error {
	return nil
}

func (d *managedDriver) Mount(ctx context.Context, opctx driver.OpContext, opts driver.MountOpts) (driver.MountTarget, error) {
	// The Docker runner bind-mounts the source path the driver returns.
	// For managed local volumes that's the volume directory itself —
	// there is no separate mount step.
	if opts.Handle == "" {
		return "", fmt.Errorf("local: Mount called with empty handle: %w", driver.ErrInvalidConfig)
	}
	// The handle becomes a container bind source, so it must be one of
	// this driver's volume directories. The resolved path is returned so
	// a symlink swapped in after the check cannot redirect the bind.
	dir, err := d.ownPath(opctx, string(opts.Handle))
	if err != nil {
		return "", fmt.Errorf("local: refusing to mount: %w", err)
	}
	return driver.MountTarget(dir), nil
}

func (d *managedDriver) Unmount(ctx context.Context, opctx driver.OpContext, target driver.MountTarget) error {
	return nil
}

func (d *managedDriver) Snapshot(ctx context.Context, opctx driver.OpContext, req driver.SnapshotRequest) (driver.SnapshotHandle, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	if req.Handle == "" {
		return "", fmt.Errorf("local: Snapshot called with empty handle: %w", driver.ErrInvalidConfig)
	}
	src, err := d.ownPath(opctx, string(req.Handle))
	if err != nil {
		return "", fmt.Errorf("local: refusing to snapshot: %w", err)
	}
	if req.Snapshot == nil {
		return "", fmt.Errorf("local: Snapshot called with nil Snapshot: %w", driver.ErrInvalidConfig)
	}
	snapNS := types.NS(req.Snapshot.Namespace)
	if err := pathSegments(snapNS, req.Snapshot.Name); err != nil {
		return "", fmt.Errorf("local: snapshot: %w", err)
	}
	dst := filepath.Join(d.cfg.SnapshotRoot, snapNS, req.Snapshot.Name)
	if err := os.MkdirAll(filepath.Dir(dst), 0o750); err != nil {
		return "", fmt.Errorf("local: mkdir snapshot parent %q: %w", filepath.Dir(dst), err)
	}
	if err := copyTree(src, dst); err != nil {
		return "", fmt.Errorf("local: snapshot copy %q -> %q: %w", src, dst, err)
	}
	return driver.SnapshotHandle(dst), nil
}

func (d *managedDriver) RestoreFromSnapshot(ctx context.Context, opctx driver.OpContext, req driver.RestoreRequest) (driver.VolumeHandle, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	if req.SourceHandle == "" {
		return "", fmt.Errorf("local: RestoreFromSnapshot called with empty source handle: %w", driver.ErrInvalidConfig)
	}
	if opctx.Volume == nil {
		return "", fmt.Errorf("local: RestoreFromSnapshot: OpContext.Volume (target) is required: %w", driver.ErrInvalidConfig)
	}
	src, err := d.ownSnapshotPath(req.Source, string(req.SourceHandle))
	if err != nil {
		return "", fmt.Errorf("local: refusing to restore: source %w", err)
	}
	dst, err := d.volumeDir(opctx.Volume)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o750); err != nil {
		return "", fmt.Errorf("local: mkdir target parent %q: %w", filepath.Dir(dst), err)
	}
	// Remove any partial directory from a prior failed restore.
	_ = os.RemoveAll(dst)
	if err := copyTree(src, dst); err != nil {
		return "", fmt.Errorf("local: restore copy %q -> %q: %w", req.SourceHandle, dst, err)
	}
	return driver.VolumeHandle(dst), nil
}

func (d *managedDriver) Expand(ctx context.Context, opctx driver.OpContext, handle driver.VolumeHandle, newSize string) error {
	return driver.ErrUnsupported
}

// DeleteSnapshot removes the on-disk snapshot tree. Bounded to SnapshotRoot.
func (d *managedDriver) DeleteSnapshot(ctx context.Context, opctx driver.OpContext, handle driver.SnapshotHandle) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	dir := string(handle)
	if dir == "" {
		return nil
	}
	if _, err := os.Lstat(dir); errors.Is(err, os.ErrNotExist) {
		return nil
	}
	abs, err := d.ownSnapshotPath(opctx.Snapshot, dir)
	if err != nil {
		return fmt.Errorf("local: refusing to delete snapshot: %w", err)
	}
	if err := os.RemoveAll(abs); err != nil {
		return fmt.Errorf("local: rm -rf snapshot %q: %w", abs, err)
	}
	return nil
}

// volumePath resolves handle and checks it is a volume directory of this
// driver: strictly inside LocalVolumeRoot, and not in the snapshot store.
// Symlinks are resolved first, since a container can plant one inside its
// own volume directory.
func (d *managedDriver) volumePath(handle string) (string, error) {
	p, err := within(handle, d.cfg.LocalVolumeRoot)
	if err != nil {
		if errors.Is(err, driver.ErrInvalidConfig) {
			// The likeliest legitimate cause is a volume provisioned
			// before localVolumeRoot was changed.
			return "", fmt.Errorf("%w; if this volume was created under an earlier localVolumeRoot, "+
				"move its directory under the current root %q or set localVolumeRoot back", err, d.cfg.LocalVolumeRoot)
		}
		return "", err
	}
	if snap, serr := filepath.EvalSymlinks(d.cfg.SnapshotRoot); serr == nil && pathHasPrefix(p, snap) {
		return "", fmt.Errorf("%q is in the snapshot store, not a volume: %w", handle, driver.ErrInvalidConfig)
	}
	return p, nil
}

// ownPath resolves handle and checks it is this volume's own directory,
// not merely some path inside the root: otherwise one volume's handle
// could name another volume, or a whole namespace. Without a volume in
// the OpContext it falls back to the root check.
func (d *managedDriver) ownPath(opctx driver.OpContext, handle string) (string, error) {
	p, err := d.volumePath(handle)
	if err != nil || opctx.Volume == nil {
		return p, err
	}
	want, err := d.volumeDir(opctx.Volume)
	if err != nil {
		return "", err
	}
	if w, werr := filepath.EvalSymlinks(want); werr != nil || w != p {
		return "", fmt.Errorf("%q is not volume %s's directory %q: %w", handle, opctx.Volume.String(), want, driver.ErrInvalidConfig)
	}
	return p, nil
}

// snapshotPath resolves a snapshot handle and checks it is exactly one
// snapshot's directory, <snapshotRoot>/<namespace>/<name>.
func (d *managedDriver) snapshotPath(handle string) (string, error) {
	p, err := within(handle, d.cfg.SnapshotRoot)
	if err != nil {
		return "", err
	}
	r, _ := filepath.EvalSymlinks(d.cfg.SnapshotRoot)
	rel, err := filepath.Rel(r, p)
	if err != nil || len(strings.Split(rel, string(filepath.Separator))) != 2 {
		return "", fmt.Errorf("%q is not a snapshot directory under %q: %w", handle, d.cfg.SnapshotRoot, driver.ErrInvalidConfig)
	}
	return p, nil
}

// ownSnapshotPath is snapshotPath pinned to snap's own directory,
// <snapshotRoot>/<namespace>/<name>, so a handle cannot name another
// namespace's snapshot. Without a snapshot it falls back to snapshotPath.
func (d *managedDriver) ownSnapshotPath(snap *types.Snapshot, handle string) (string, error) {
	p, err := d.snapshotPath(handle)
	if err != nil || snap == nil {
		return p, err
	}
	ns := types.NS(snap.Namespace)
	if err := pathSegments(ns, snap.Name); err != nil {
		return "", err
	}
	want := filepath.Join(d.cfg.SnapshotRoot, ns, snap.Name)
	if w, werr := filepath.EvalSymlinks(want); werr != nil || w != p {
		return "", fmt.Errorf("%q is not snapshot %s/%s's directory %q: %w", handle, ns, snap.Name, want, driver.ErrInvalidConfig)
	}
	return p, nil
}

func (d *managedDriver) volumeDir(v *types.Volume) (string, error) {
	// Volumes live at <root>/<namespace>/<name>/. ID would also work but
	// keeping the human-readable layout makes operator debugging easier.
	ns := types.NS(v.Namespace)
	if err := pathSegments(ns, v.Name); err != nil {
		return "", fmt.Errorf("local: volume %s: %w", v.String(), err)
	}
	return filepath.Join(d.cfg.LocalVolumeRoot, ns, v.Name), nil
}

// pathSegments checks each name can be used as exactly one path segment.
func pathSegments(names ...string) error {
	for _, n := range names {
		if n == "" || n == "." || n == ".." || strings.ContainsAny(n, "/\\\x00") {
			return fmt.Errorf("%q is not usable as a path segment: %w", n, driver.ErrInvalidConfig)
		}
	}
	return nil
}

// ============================================================================
// "local-host" — operator-owned driver
// ============================================================================

type hostDriver struct {
	cfg *Config
}

func (d *hostDriver) Name() string { return DriverNameLocalHost }

func (d *hostDriver) Capabilities() driver.Capabilities {
	return driver.Capabilities{
		AccessModes:  []types.AccessMode{types.AccessModeRWO, types.AccessModeROX},
		Snapshots:    false,
		Expand:       false,
		OnlineExpand: false,
		BlockDevice:  false,
		TopologyKeys: []string{types.TopologyLabelHostPathRoot},
	}
}

func (d *hostDriver) Provision(ctx context.Context, opctx driver.OpContext, req driver.ProvisionRequest) (driver.VolumeHandle, error) {
	if opctx.Volume == nil {
		return "", fmt.Errorf("local-host: Provision: OpContext.Volume is required: %w", driver.ErrInvalidConfig)
	}
	if err := assertAccessMode(opctx.Volume.AccessMode, d.Capabilities()); err != nil {
		return "", err
	}
	if opctx.Volume.ReclaimPolicy == types.ReclaimPolicyDelete {
		return "", fmt.Errorf("local-host: reclaimPolicy %q is not allowed (operator owns the host path): %w",
			types.ReclaimPolicyDelete, driver.ErrInvalidConfig)
	}
	hostPath, ok := opctx.Parameters["hostPath"]
	if !ok || hostPath == "" {
		return "", fmt.Errorf("local-host: parameters.hostPath is required: %w", driver.ErrInvalidConfig)
	}
	abs, err := filepath.Abs(hostPath)
	if err != nil {
		return "", fmt.Errorf("local-host: resolve %q: %w", hostPath, err)
	}
	if !d.allowed(abs) {
		return "", fmt.Errorf("local-host: %q is not in hostPathAllowlist: %w", abs, driver.ErrInvalidConfig)
	}
	info, err := os.Stat(abs)
	if errors.Is(err, os.ErrNotExist) {
		if !d.cfg.AllowCreateMissing {
			return "", fmt.Errorf("local-host: %q does not exist (allowCreateMissing=false): %w", abs, driver.ErrInvalidConfig)
		}
		if err := os.MkdirAll(abs, 0o750); err != nil {
			return "", fmt.Errorf("local-host: mkdir %q: %w", abs, err)
		}
	} else if err != nil {
		return "", fmt.Errorf("local-host: stat %q: %w", abs, err)
	} else if !info.IsDir() {
		return "", fmt.Errorf("local-host: %q is not a directory: %w", abs, driver.ErrInvalidConfig)
	}
	return driver.VolumeHandle(abs), nil
}

func (d *hostDriver) Delete(ctx context.Context, opctx driver.OpContext, handle driver.VolumeHandle) error {
	// Operator owns the host path. Always a no-op.
	return nil
}

func (d *hostDriver) Attach(ctx context.Context, opctx driver.OpContext, handle driver.VolumeHandle, node driver.NodeID) (driver.DevicePath, error) {
	return "", nil
}

func (d *hostDriver) Detach(ctx context.Context, opctx driver.OpContext, handle driver.VolumeHandle, node driver.NodeID) error {
	return nil
}

func (d *hostDriver) Mount(ctx context.Context, opctx driver.OpContext, opts driver.MountOpts) (driver.MountTarget, error) {
	if opts.Handle == "" {
		return "", fmt.Errorf("local-host: Mount called with empty handle: %w", driver.ErrInvalidConfig)
	}
	// Re-validate the path on every mount: an operator may have removed
	// the directory between provision and bind, and we want a clear error
	// rather than letting Docker fail with a confusing message.
	if _, err := os.Stat(string(opts.Handle)); err != nil {
		return "", fmt.Errorf("local-host: stat mount source %q: %w", opts.Handle, err)
	}
	// The allowlist was checked at provision, but the handle is what gets
	// bound, so it is checked again here, after resolving symlinks.
	resolved, err := filepath.EvalSymlinks(string(opts.Handle))
	if err != nil {
		return "", fmt.Errorf("local-host: resolve mount source %q: %w", opts.Handle, err)
	}
	if !d.allowedResolved(resolved) {
		return "", fmt.Errorf("local-host: refusing to mount %q (resolves to %q): not in hostPathAllowlist: %w",
			opts.Handle, resolved, driver.ErrInvalidConfig)
	}
	return driver.MountTarget(resolved), nil
}

func (d *hostDriver) Unmount(ctx context.Context, opctx driver.OpContext, target driver.MountTarget) error {
	return nil
}

func (d *hostDriver) Snapshot(ctx context.Context, opctx driver.OpContext, req driver.SnapshotRequest) (driver.SnapshotHandle, error) {
	return "", driver.ErrUnsupported
}

func (d *hostDriver) RestoreFromSnapshot(ctx context.Context, opctx driver.OpContext, req driver.RestoreRequest) (driver.VolumeHandle, error) {
	return "", driver.ErrUnsupported
}

func (d *hostDriver) Expand(ctx context.Context, opctx driver.OpContext, handle driver.VolumeHandle, newSize string) error {
	return driver.ErrUnsupported
}

func (d *hostDriver) DeleteSnapshot(ctx context.Context, opctx driver.OpContext, handle driver.SnapshotHandle) error {
	return driver.ErrUnsupported
}

func (d *hostDriver) allowed(abs string) bool {
	return IsHostPathAllowed(abs, d.cfg.HostPathAllowlist)
}

// allowedResolved checks an already symlink-resolved path against the
// allowlist with its entries resolved the same way, so an allowlisted
// directory reached through a symlink still matches.
func (d *hostDriver) allowedResolved(resolved string) bool {
	for _, prefix := range d.cfg.HostPathAllowlist {
		p, err := filepath.EvalSymlinks(prefix)
		if err != nil {
			if p, err = filepath.Abs(prefix); err != nil {
				continue
			}
		}
		if pathHasPrefix(resolved, p) {
			return true
		}
	}
	return false
}

// within resolves path through symlinks and checks it lies strictly
// inside root, also resolved. It returns the resolved path.
func within(path, root string) (string, error) {
	r, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", fmt.Errorf("resolve root %q: %w", root, err)
	}
	p, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", fmt.Errorf("resolve %q: %w", path, err)
	}
	if p == r || !pathHasPrefix(p, r) {
		return "", fmt.Errorf("%q (resolves to %q) is outside %q: %w", path, p, r, driver.ErrInvalidConfig)
	}
	return p, nil
}

// IsHostPathAllowed reports whether abs is inside any prefix in
// allowlist. Each prefix is resolved with filepath.Abs and compared with
// proper path-segment semantics (so /var/lib/rune/volumes2 does NOT
// match a /var/lib/rune/volumes prefix).
//
// Exported so the API server can perform the same cast-time check the
// driver performs at provision time, without instantiating the driver.
func IsHostPathAllowed(abs string, allowlist []string) bool {
	for _, prefix := range allowlist {
		p, err := filepath.Abs(prefix)
		if err != nil {
			continue
		}
		if pathHasPrefix(abs, p) {
			return true
		}
	}
	return false
}

// AllowlistFromConfig extracts the hostPathAllowlist entries from a raw
// runefile config map (the same shape consumed by factoryHost). Returns
// nil for missing/empty/malformed entries — never errors. Exported so
// the API server can read the allowlist without round-tripping through
// driver instantiation.
func AllowlistFromConfig(raw map[string]any) []string {
	if raw == nil {
		return nil
	}
	for k, v := range raw {
		if strings.EqualFold(k, "hostPathAllowlist") {
			switch vv := v.(type) {
			case []string:
				out := make([]string, 0, len(vv))
				out = append(out, vv...)
				return out
			case []any:
				out := make([]string, 0, len(vv))
				for _, e := range vv {
					if s, ok := e.(string); ok {
						out = append(out, s)
					}
				}
				return out
			}
		}
	}
	return nil
}

// ============================================================================
// Shared helpers
// ============================================================================

func assertAccessMode(mode types.AccessMode, caps driver.Capabilities) error {
	for _, m := range caps.AccessModes {
		if m == mode {
			return nil
		}
	}
	return fmt.Errorf("local: access mode %q not supported: %w", mode, driver.ErrAccessModeUnsupported)
}

// pathHasPrefix is filepath.HasPrefix done correctly (avoids the
// substring-match bug where "/var/lib/rune/volumes2" would appear to be
// inside "/var/lib/rune/volumes").
func pathHasPrefix(path, prefix string) bool {
	if !strings.HasPrefix(path, prefix) {
		return false
	}
	if len(path) == len(prefix) {
		return true
	}
	// Next char must be a separator — otherwise we matched a sibling dir.
	return path[len(prefix)] == os.PathSeparator
}

// copyTree recursively copies a directory tree. Used by the managed
// driver's filesystem-level snapshot path. Intentionally simple — btrfs
// subvolume snapshot is a future optimization.
func copyTree(src, dst string) error {
	return filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		switch {
		case info.IsDir():
			return os.MkdirAll(target, info.Mode().Perm())
		case info.Mode()&os.ModeSymlink != 0:
			link, err := os.Readlink(path)
			if err != nil {
				return err
			}
			return os.Symlink(link, target)
		default:
			return copyFile(path, target, info.Mode().Perm())
		}
	})
}

func copyFile(src, dst string, perm os.FileMode) error {
	// O_NOFOLLOW: the walk saw a regular file, but a running container
	// could have swapped in a symlink since.
	in, err := openNoFollow(src)
	if err != nil {
		return err
	}
	defer in.Close()
	if err := os.MkdirAll(filepath.Dir(dst), 0o750); err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, perm)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
