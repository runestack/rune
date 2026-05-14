package dovolume

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/runestack/rune/pkg/storage/driver"
	"github.com/runestack/rune/pkg/types"
)

func init() {
	driver.Register(DriverName, factory)
}

// factory is the registry entry point. cmd/runed (or tests) supplies
// the runefile [storage.drivers.do-volume] section. Auth is sourced
// per-call from the StorageClass `parameters.apiToken` value, not from
// driver-level config.
func factory(raw map[string]any) (driver.Driver, error) {
	cfg, err := parseConfig(raw)
	if err != nil {
		return nil, err
	}
	return &doVolumeDriver{
		cfg:    cfg,
		client: newHTTPClient(cfg),
		mounts: execMounter{},
	}, nil
}

// doVolumeDriver implements driver.Driver against the DigitalOcean API.
type doVolumeDriver struct {
	cfg    *Config
	client doClient  // overridable for tests
	mounts mountExec // overridable for tests
}

// MountRoot is the parent directory the driver mounts each volume
// under. A Mount(target=...) call is honoured (the agent passes
// /var/lib/rune/mounts/<vol.ID>/) but the driver also accepts an
// empty Target and falls back to its own /var/lib/rune/mounts/dovolume
// layout — useful for ad-hoc tests.
const fallbackMountRoot = "/var/lib/rune/mounts/dovolume"

// --- Driver impl ---------------------------------------------------------

func (d *doVolumeDriver) Name() string { return DriverName }

func (d *doVolumeDriver) Capabilities() driver.Capabilities {
	return driver.Capabilities{
		AccessModes: []types.AccessMode{
			types.AccessModeRWO,
		},
		Snapshots:    true,
		Expand:       true,
		OnlineExpand: false,
		BlockDevice:  true,
		TopologyKeys: []string{"rune.io/region"},
	}
}

func (d *doVolumeDriver) Provision(ctx context.Context, opctx driver.OpContext, req driver.ProvisionRequest) (driver.VolumeHandle, error) {
	if opctx.Volume == nil {
		return "", fmt.Errorf("dovolume: Provision: OpContext.Volume is required")
	}
	ctx = withToken(ctx, mergedParam(opctx.Parameters, "apiToken"))
	region := mergedParam(opctx.Parameters, "region")
	if region == "" {
		return "", fmt.Errorf("dovolume: Provision: parameters.region is required")
	}
	if !accessModeSupported(opctx.Volume.AccessMode) {
		return "", fmt.Errorf("dovolume: %w: %s", driver.ErrAccessModeUnsupported, opctx.Volume.AccessMode)
	}
	gigabytes, err := bytesToGigabytes(req.SizeBytes)
	if err != nil {
		return "", err
	}
	doName := d.doVolumeName(opctx.Volume)
	fsType := mergedParam(opctx.Parameters, "fsType")
	in := createVolumeIn{
		Name:           doName,
		Region:         region,
		SizeGigabytes:  gigabytes,
		FilesystemType: fsType, // empty -> DO leaves it unformatted; we format on first Mount
		Description:    fmt.Sprintf("rune volume %s", opctx.Volume.String()),
	}
	vol, err := d.client.createVolume(ctx, in)
	if err != nil {
		return "", fmt.Errorf("dovolume: createVolume: %w", err)
	}
	return driver.VolumeHandle(vol.ID), nil
}

func (d *doVolumeDriver) Delete(ctx context.Context, opctx driver.OpContext, handle driver.VolumeHandle) error {
	if handle == "" {
		return nil
	}
	ctx = withToken(ctx, mergedParam(opctx.Parameters, "apiToken"))
	if err := d.client.deleteVolume(ctx, string(handle)); err != nil {
		if errors.Is(err, errDONotFound) {
			return nil
		}
		return fmt.Errorf("dovolume: deleteVolume %s: %w", handle, err)
	}
	return nil
}

func (d *doVolumeDriver) Attach(ctx context.Context, opctx driver.OpContext, handle driver.VolumeHandle, node driver.NodeID) (driver.DevicePath, error) {
	if handle == "" {
		return "", fmt.Errorf("dovolume: Attach: empty handle")
	}
	if node == "" {
		return "", fmt.Errorf("dovolume: Attach: empty node id")
	}
	ctx = withToken(ctx, mergedParam(opctx.Parameters, "apiToken"))
	vol, err := d.client.getVolume(ctx, string(handle))
	if err != nil {
		return "", fmt.Errorf("dovolume: getVolume %s: %w", handle, err)
	}
	dropletID, err := d.client.dropletByName(ctx, string(node))
	if err != nil {
		if errors.Is(err, errDONotFound) {
			return "", fmt.Errorf("dovolume: no DO droplet matches node %q", node)
		}
		return "", fmt.Errorf("dovolume: dropletByName %s: %w", node, err)
	}
	// Already attached to this droplet? Nothing to do.
	if hasDroplet(vol, dropletID) {
		return doDevicePath(vol.Name), nil
	}
	// Refuse to attach if it's already on a different droplet —
	// detach must happen first via the orchestrator.
	if len(vol.DropletIDs) > 0 {
		return "", fmt.Errorf("dovolume: volume %s already attached to droplet(s) %v", handle, vol.DropletIDs)
	}
	act, err := d.client.volumeAction(ctx, string(handle), map[string]any{
		"type":       "attach",
		"droplet_id": dropletID,
		"region":     vol.Region.Slug,
	})
	if err != nil {
		return "", fmt.Errorf("dovolume: attach action: %w", err)
	}
	if err := waitForAction(ctx, d.client, act.ID, d.actionPollInterval()); err != nil {
		return "", err
	}
	return doDevicePath(vol.Name), nil
}

func (d *doVolumeDriver) Detach(ctx context.Context, opctx driver.OpContext, handle driver.VolumeHandle, node driver.NodeID) error {
	if handle == "" {
		return nil
	}
	ctx = withToken(ctx, mergedParam(opctx.Parameters, "apiToken"))
	vol, err := d.client.getVolume(ctx, string(handle))
	if err != nil {
		if errors.Is(err, errDONotFound) {
			return nil // gone -> nothing to detach
		}
		return fmt.Errorf("dovolume: getVolume %s: %w", handle, err)
	}
	if len(vol.DropletIDs) == 0 {
		return nil // already detached
	}
	var dropletID int64
	if node != "" {
		id, err := d.client.dropletByName(ctx, string(node))
		if err == nil {
			dropletID = id
		}
		// If the droplet no longer exists, fall through to detach
		// from whichever droplet is currently attached — this is the
		// "node was destroyed out-of-band" recovery path.
	}
	if dropletID == 0 {
		dropletID = vol.DropletIDs[0]
	}
	act, err := d.client.volumeAction(ctx, string(handle), map[string]any{
		"type":       "detach",
		"droplet_id": dropletID,
		"region":     vol.Region.Slug,
	})
	if err != nil {
		return fmt.Errorf("dovolume: detach action: %w", err)
	}
	if err := waitForAction(ctx, d.client, act.ID, d.actionPollInterval()); err != nil {
		return err
	}
	return nil
}

func (d *doVolumeDriver) Mount(ctx context.Context, opctx driver.OpContext, opts driver.MountOpts) (driver.MountTarget, error) {
	dev := string(opts.Device)
	if dev == "" {
		// Caller (the agent) may not have populated Device — derive
		// it from the volume name (which we set during Provision).
		ctx = withToken(ctx, mergedParam(opctx.Parameters, "apiToken"))
		vol, err := d.client.getVolume(ctx, string(opts.Handle))
		if err != nil {
			return "", fmt.Errorf("dovolume: Mount: lookup volume %s: %w", opts.Handle, err)
		}
		dev = string(doDevicePath(vol.Name))
	}
	target := string(opts.Target)
	if target == "" {
		target = fallbackMountRoot + "/" + string(opts.Handle)
	}
	if err := d.mounts.MkdirAll(target, 0o750); err != nil {
		return "", fmt.Errorf("dovolume: mkdir %s: %w", target, err)
	}
	fsType := opts.FsType
	if fsType == "" {
		fsType = "ext4"
	}
	if !opts.ReadOnly {
		if err := d.mounts.EnsureFormatted(ctx, dev, fsType); err != nil {
			return "", err
		}
	}
	if err := d.mounts.Mount(ctx, dev, target, fsType, opts.ReadOnly); err != nil {
		return "", err
	}
	return driver.MountTarget(target), nil
}

func (d *doVolumeDriver) Unmount(ctx context.Context, opctx driver.OpContext, target driver.MountTarget) error {
	if target == "" {
		return nil
	}
	return d.mounts.Unmount(ctx, string(target))
}

func (d *doVolumeDriver) Snapshot(ctx context.Context, opctx driver.OpContext, req driver.SnapshotRequest) (driver.SnapshotHandle, error) {
	if opctx.Volume == nil {
		return "", fmt.Errorf("dovolume: Snapshot: OpContext.Volume is required")
	}
	if req.Snapshot == nil {
		return "", fmt.Errorf("dovolume: Snapshot: nil Snapshot")
	}
	if req.Handle == "" {
		return "", fmt.Errorf("dovolume: Snapshot: empty volume handle")
	}
	ctx = withToken(ctx, mergedParam(opctx.Parameters, "apiToken"))
	doSnapName := d.doSnapshotName(opctx.Volume, req.Snapshot)
	snap, err := d.client.createSnapshot(ctx, string(req.Handle), doSnapName)
	if err != nil {
		return "", fmt.Errorf("dovolume: createSnapshot: %w", err)
	}
	return driver.SnapshotHandle(snap.ID), nil
}

func (d *doVolumeDriver) RestoreFromSnapshot(ctx context.Context, opctx driver.OpContext, req driver.RestoreRequest) (driver.VolumeHandle, error) {
	if req.Source == nil {
		return "", fmt.Errorf("dovolume: RestoreFromSnapshot: nil Source")
	}
	if opctx.Volume == nil {
		return "", fmt.Errorf("dovolume: RestoreFromSnapshot: OpContext.Volume (target) is required")
	}
	if req.SourceHandle == "" {
		return "", fmt.Errorf("dovolume: RestoreFromSnapshot: empty source handle")
	}
	ctx = withToken(ctx, mergedParam(opctx.Parameters, "apiToken"))
	region := mergedParam(opctx.Parameters, "region")
	if region == "" {
		return "", fmt.Errorf("dovolume: RestoreFromSnapshot: parameters.region is required")
	}
	gigabytes, err := bytesToGigabytes(req.SizeBytes)
	if err != nil {
		return "", err
	}
	doName := d.doVolumeName(opctx.Volume)
	vol, err := d.client.createVolumeFromSnapshot(ctx, restoreVolumeIn{
		Name:          doName,
		Region:        region,
		SizeGigabytes: gigabytes,
		SnapshotID:    string(req.SourceHandle),
	})
	if err != nil {
		return "", fmt.Errorf("dovolume: createVolumeFromSnapshot: %w", err)
	}
	return driver.VolumeHandle(vol.ID), nil
}

func (d *doVolumeDriver) DeleteSnapshot(ctx context.Context, opctx driver.OpContext, handle driver.SnapshotHandle) error {
	if handle == "" {
		return nil
	}
	ctx = withToken(ctx, mergedParam(opctx.Parameters, "apiToken"))
	if err := d.client.deleteSnapshot(ctx, string(handle)); err != nil {
		return fmt.Errorf("dovolume: deleteSnapshot %s: %w", handle, err)
	}
	return nil
}

func (d *doVolumeDriver) Expand(ctx context.Context, opctx driver.OpContext, handle driver.VolumeHandle, newSize string) error {
	if handle == "" {
		return fmt.Errorf("dovolume: Expand: empty handle")
	}
	ctx = withToken(ctx, mergedParam(opctx.Parameters, "apiToken"))
	bytes, err := parseQuantity(newSize)
	if err != nil {
		return fmt.Errorf("dovolume: Expand: parse size %q: %w", newSize, err)
	}
	gigabytes, err := bytesToGigabytes(bytes)
	if err != nil {
		return err
	}
	vol, err := d.client.getVolume(ctx, string(handle))
	if err != nil {
		return fmt.Errorf("dovolume: getVolume %s: %w", handle, err)
	}
	// DO requires offline expand: refuse if currently attached.
	if len(vol.DropletIDs) > 0 {
		return driver.ErrOnlineExpandUnsupported
	}
	if gigabytes <= vol.SizeGigabytes {
		// Already at or above target size; nothing to do.
		return nil
	}
	act, err := d.client.volumeAction(ctx, string(handle), map[string]any{
		"type":           "resize",
		"size_gigabytes": gigabytes,
		"region":         vol.Region.Slug,
	})
	if err != nil {
		return fmt.Errorf("dovolume: resize action: %w", err)
	}
	return waitForAction(ctx, d.client, act.ID, d.actionPollInterval())
}

// --- helpers -------------------------------------------------------------

func (d *doVolumeDriver) actionPollInterval() time.Duration {
	if hc, ok := d.client.(*httpClient); ok {
		return hc.pollInterval
	}
	return 50 * time.Millisecond // fast default for fake clients in tests
}

// doVolumeName turns a Rune Volume into a DO-acceptable volume name:
// lowercase, hyphenated, must start with a letter, ≤ 64 chars.
func (d *doVolumeDriver) doVolumeName(v *types.Volume) string {
	raw := d.cfg.VolumeNamePrefix + v.Namespace + "-" + v.Name
	return sanitizeDOName(raw, 64)
}

// doSnapshotName: <volumeDOName>-snap-<snap.Name>, sanitized.
func (d *doVolumeDriver) doSnapshotName(v *types.Volume, s *types.Snapshot) string {
	raw := d.cfg.VolumeNamePrefix + v.Namespace + "-" + v.Name + "-snap-" + s.Name
	return sanitizeDOName(raw, 64)
}

// sanitizeDOName lowercases, replaces invalid chars with '-', collapses
// runs of hyphens, ensures the leading char is a letter, and truncates
// to maxLen.
func sanitizeDOName(s string, maxLen int) string {
	s = strings.ToLower(s)
	var b strings.Builder
	prevHyphen := false
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			prevHyphen = false
		case r == '-' || r == '_' || r == '/' || r == '.' || r == ' ':
			if !prevHyphen && b.Len() > 0 {
				b.WriteByte('-')
				prevHyphen = true
			}
		default:
			// drop other runes
		}
	}
	out := strings.TrimRight(b.String(), "-")
	// Must start with a letter.
	if out == "" || (out[0] < 'a' || out[0] > 'z') {
		out = "v" + out
	}
	if len(out) > maxLen {
		out = out[:maxLen]
		out = strings.TrimRight(out, "-")
	}
	return out
}

// doDevicePath returns the well-known /dev/disk/by-id path DO assigns
// to attached volumes.
func doDevicePath(volumeName string) driver.DevicePath {
	return driver.DevicePath("/dev/disk/by-id/scsi-0DO_Volume_" + volumeName)
}

func mergedParam(p map[string]string, key string) string {
	if p == nil {
		return ""
	}
	if v, ok := p[key]; ok {
		return v
	}
	// case-insensitive fallback so operators using Camel/snake variants
	// don't get cryptic "region is required" errors.
	lk := strings.ToLower(key)
	for k, v := range p {
		if strings.ToLower(k) == lk {
			return v
		}
	}
	return ""
}

func accessModeSupported(m types.AccessMode) bool {
	return m == "" || m == types.AccessModeRWO
}

// bytesToGigabytes rounds up to the next whole DO gigabyte. DO's API
// uses decimal GB (10^9), not GiB. We round up to ensure the user gets
// at least the requested capacity.
func bytesToGigabytes(b int64) (int64, error) {
	if b <= 0 {
		return 0, fmt.Errorf("dovolume: invalid size %d bytes (must be > 0)", b)
	}
	const gb = int64(1_000_000_000)
	gigabytes := (b + gb - 1) / gb
	if gigabytes < 1 {
		gigabytes = 1
	}
	return gigabytes, nil
}

func hasDroplet(v *doVolume, id int64) bool {
	for _, d := range v.DropletIDs {
		if d == id {
			return true
		}
	}
	return false
}

// parseQuantity is a minimal Kubernetes-style storage quantity parser
// covering the suffixes Rune Volume.Size strings actually use: bare
// bytes (no suffix), decimal SI ("K","M","G","T") and binary IEC
// ("Ki","Mi","Gi","Ti"). Whitespace is trimmed. Returns the size in
// bytes.
//
// Lives in the dovolume package because the wider codebase does not
// yet have a shared parser; if/when one lands in pkg/types it should
// replace this. Kept small on purpose — full Kubernetes resource.Quantity
// semantics are out of scope.
func parseQuantity(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("empty quantity")
	}
	// Find where the numeric prefix ends.
	i := 0
	for i < len(s) && (s[i] >= '0' && s[i] <= '9') {
		i++
	}
	if i == 0 {
		return 0, fmt.Errorf("quantity %q has no numeric prefix", s)
	}
	num := s[:i]
	suffix := strings.TrimSpace(s[i:])
	var n int64
	if _, err := fmt.Sscanf(num, "%d", &n); err != nil {
		return 0, fmt.Errorf("parse number %q: %w", num, err)
	}
	if n < 0 {
		return 0, fmt.Errorf("quantity %q is negative", s)
	}
	var mult int64
	switch suffix {
	case "":
		mult = 1
	case "K", "k":
		mult = 1_000
	case "M", "m":
		mult = 1_000_000
	case "G", "g":
		mult = 1_000_000_000
	case "T", "t":
		mult = 1_000_000_000_000
	case "Ki":
		mult = 1 << 10
	case "Mi":
		mult = 1 << 20
	case "Gi":
		mult = 1 << 30
	case "Ti":
		mult = 1 << 40
	default:
		return 0, fmt.Errorf("quantity %q has unsupported suffix %q", s, suffix)
	}
	return n * mult, nil
}
