package gcepd

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"cloud.google.com/go/compute/metadata"
	"github.com/runestack/rune/pkg/storage/driver"
	"github.com/runestack/rune/pkg/types"
)

func init() {
	driver.Register(DriverName, factory)
}

// factory is the registry entry point. cmd/runed (or tests) supplies the
// runefile [storage.drivers.gce-pd] section. Project, zone and auth are
// sourced per-call from StorageClass parameters, not from driver-level
// config.
func factory(raw map[string]any) (driver.Driver, error) {
	cfg, err := parseConfig(raw)
	if err != nil {
		return nil, err
	}
	return &pdDriver{
		cfg:    cfg,
		client: newSDKClient(),
		mounts: execMounter{},
	}, nil
}

// pdDriver implements driver.Driver against the GCE Compute API.
type pdDriver struct {
	cfg    *Config
	client gceClient // overridable for tests
	mounts mountExec // overridable for tests
}

const fallbackMountRoot = "/var/lib/rune/mounts/gcepd"

// Labels stamped on every disk the driver creates.
const (
	labelRuneNS   = "rune-namespace"
	labelRuneName = "rune-name"
)

// --- Driver impl ---------------------------------------------------------

func (d *pdDriver) Name() string { return DriverName }

func (d *pdDriver) Capabilities() driver.Capabilities {
	return driver.Capabilities{
		AccessModes:  []types.AccessMode{types.AccessModeRWO},
		Snapshots:    true,
		Expand:       true,
		OnlineExpand: true, // GCE disks.resize grows in place while attached
		BlockDevice:  true,
		TopologyKeys: []string{types.TopologyLabelZone},
	}
}

func (d *pdDriver) Provision(ctx context.Context, opctx driver.OpContext, req driver.ProvisionRequest) (driver.VolumeHandle, error) {
	if opctx.Volume == nil {
		return "", fmt.Errorf("gcepd: Provision: OpContext.Volume is required")
	}
	ctx = withCreds(ctx, mergedParam(opctx.Parameters, "credentialsJSON"))
	if !accessModeSupported(opctx.Volume.AccessMode) {
		return "", fmt.Errorf("gcepd: %w: %s", driver.ErrAccessModeUnsupported, opctx.Volume.AccessMode)
	}
	project, err := d.resolveProject(ctx, opctx)
	if err != nil {
		return "", err
	}
	zone, err := resolveZone(opctx, req)
	if err != nil {
		return "", err
	}
	gib, err := bytesToGiB(req.SizeBytes)
	if err != nil {
		return "", err
	}
	name := d.diskName(opctx.Volume)

	// Adopt an existing disk with the same name before creating a new one.
	// GCE disk names are unique per (project, zone) and the name is derived
	// deterministically from the Rune volume's namespace+name, so a re-cast
	// after the Rune Volume row was lost (or a retried Provision) reuses the
	// existing disk and its data instead of failing on the name clash.
	// Mirrors the adopt-on-conflict path in dovolume / aws-ebs.
	if _, err := d.client.getDisk(ctx, project, zone, name); err == nil {
		return driver.VolumeHandle(name), nil
	} else if !errors.Is(err, errNotFound) {
		return "", fmt.Errorf("gcepd: lookup existing disk %q: %w", name, err)
	}

	if err := d.client.insertDisk(ctx, project, zone, diskSpec{
		Name:     name,
		SizeGB:   gib,
		DiskType: mergedParamOr(opctx.Parameters, "diskType", "pd-balanced"),
		Labels:   d.diskLabels(opctx.Volume),
	}); err != nil {
		return "", fmt.Errorf("gcepd: insertDisk: %w", err)
	}
	return driver.VolumeHandle(name), nil
}

func (d *pdDriver) Delete(ctx context.Context, opctx driver.OpContext, handle driver.VolumeHandle) error {
	if handle == "" {
		return nil
	}
	ctx = withCreds(ctx, mergedParam(opctx.Parameters, "credentialsJSON"))
	project, err := d.resolveProject(ctx, opctx)
	if err != nil {
		return err
	}
	zone, err := resolveZone(opctx, driver.ProvisionRequest{})
	if err != nil {
		return err
	}
	if err := d.client.deleteDisk(ctx, project, zone, string(handle)); err != nil {
		if errors.Is(err, errNotFound) {
			return nil
		}
		return fmt.Errorf("gcepd: deleteDisk %s: %w", handle, err)
	}
	return nil
}

func (d *pdDriver) Attach(ctx context.Context, opctx driver.OpContext, handle driver.VolumeHandle, node driver.NodeID) (driver.DevicePath, error) {
	if handle == "" {
		return "", fmt.Errorf("gcepd: Attach: empty handle")
	}
	if node == "" {
		return "", fmt.Errorf("gcepd: Attach: empty node id")
	}
	ctx = withCreds(ctx, mergedParam(opctx.Parameters, "credentialsJSON"))
	project, err := d.resolveProject(ctx, opctx)
	if err != nil {
		return "", err
	}
	zone, err := resolveZone(opctx, driver.ProvisionRequest{})
	if err != nil {
		return "", err
	}
	lookupName := instanceLookupName(opctx, node)
	inst, err := d.client.getInstance(ctx, project, zone, lookupName)
	if err != nil {
		if errors.Is(err, errNotFound) {
			return "", fmt.Errorf("gcepd: no GCE instance %q in zone %s (set the node's hostname to match the instance name)", lookupName, zone)
		}
		return "", fmt.Errorf("gcepd: getInstance %s: %w", lookupName, err)
	}
	disk, err := d.client.getDisk(ctx, project, zone, string(handle))
	if err != nil {
		return "", fmt.Errorf("gcepd: getDisk %s: %w", handle, err)
	}
	dev := devicePath(string(handle))
	// Already attached to this instance? Nothing to do.
	if containsSelfLink(disk.Users, inst.SelfLink) {
		return dev, nil
	}
	// Refuse to attach if it's already on a different instance — detach
	// must happen first via the orchestrator.
	if len(disk.Users) > 0 {
		return "", fmt.Errorf("gcepd: disk %s already attached to %v", handle, disk.Users)
	}
	if err := d.client.attachDisk(ctx, project, zone, inst.Name, attachSpec{
		Source:     disk.SelfLink,
		DeviceName: string(handle),
		ReadOnly:   false,
	}); err != nil {
		return "", fmt.Errorf("gcepd: attachDisk: %w", err)
	}
	return dev, nil
}

func (d *pdDriver) Detach(ctx context.Context, opctx driver.OpContext, handle driver.VolumeHandle, node driver.NodeID) error {
	if handle == "" {
		return nil
	}
	ctx = withCreds(ctx, mergedParam(opctx.Parameters, "credentialsJSON"))
	project, err := d.resolveProject(ctx, opctx)
	if err != nil {
		return err
	}
	zone, err := resolveZone(opctx, driver.ProvisionRequest{})
	if err != nil {
		return err
	}
	disk, err := d.client.getDisk(ctx, project, zone, string(handle))
	if err != nil {
		if errors.Is(err, errNotFound) {
			return nil // gone -> nothing to detach
		}
		return fmt.Errorf("gcepd: getDisk %s: %w", handle, err)
	}
	if len(disk.Users) == 0 {
		return nil // already detached
	}
	// Resolve the instance to detach from: prefer the named node, else the
	// instance the disk currently reports (the node-was-destroyed path).
	instance := ""
	if node != "" {
		if inst, err := d.client.getInstance(ctx, project, zone, instanceLookupName(opctx, node)); err == nil {
			instance = inst.Name
		}
	}
	if instance == "" {
		instance = instanceNameFromSelfLink(disk.Users[0])
	}
	if err := d.client.detachDisk(ctx, project, zone, instance, string(handle)); err != nil {
		if errors.Is(err, errNotFound) {
			return nil
		}
		return fmt.Errorf("gcepd: detachDisk: %w", err)
	}
	return nil
}

func (d *pdDriver) Mount(ctx context.Context, opctx driver.OpContext, opts driver.MountOpts) (driver.MountTarget, error) {
	dev := string(opts.Device)
	if dev == "" {
		// Derive the stable by-id device path from the disk name (handle).
		dev = string(devicePath(string(opts.Handle)))
	}
	target := string(opts.Target)
	if target == "" {
		target = fallbackMountRoot + "/" + string(opts.Handle)
	}
	if err := d.mounts.MkdirAll(target, 0o750); err != nil {
		return "", fmt.Errorf("gcepd: mkdir %s: %w", target, err)
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

func (d *pdDriver) Unmount(ctx context.Context, opctx driver.OpContext, target driver.MountTarget) error {
	if target == "" {
		return nil
	}
	return d.mounts.Unmount(ctx, string(target))
}

func (d *pdDriver) Snapshot(ctx context.Context, opctx driver.OpContext, req driver.SnapshotRequest) (driver.SnapshotHandle, error) {
	if opctx.Volume == nil {
		return "", fmt.Errorf("gcepd: Snapshot: OpContext.Volume is required")
	}
	if req.Snapshot == nil {
		return "", fmt.Errorf("gcepd: Snapshot: nil Snapshot")
	}
	if req.Handle == "" {
		return "", fmt.Errorf("gcepd: Snapshot: empty volume handle")
	}
	ctx = withCreds(ctx, mergedParam(opctx.Parameters, "credentialsJSON"))
	project, err := d.resolveProject(ctx, opctx)
	if err != nil {
		return "", err
	}
	zone, err := resolveZone(opctx, driver.ProvisionRequest{})
	if err != nil {
		return "", err
	}
	snapName := d.snapshotName(req.Snapshot)
	desc := fmt.Sprintf("rune snapshot %s/%s of disk %s", req.Snapshot.Namespace, req.Snapshot.Name, req.Handle)
	if err := d.client.createSnapshot(ctx, project, zone, string(req.Handle), snapName, desc); err != nil {
		return "", fmt.Errorf("gcepd: createSnapshot: %w", err)
	}
	return driver.SnapshotHandle(snapName), nil
}

func (d *pdDriver) RestoreFromSnapshot(ctx context.Context, opctx driver.OpContext, req driver.RestoreRequest) (driver.VolumeHandle, error) {
	if req.Source == nil {
		return "", fmt.Errorf("gcepd: RestoreFromSnapshot: nil Source")
	}
	if opctx.Volume == nil {
		return "", fmt.Errorf("gcepd: RestoreFromSnapshot: OpContext.Volume (target) is required")
	}
	if req.SourceHandle == "" {
		return "", fmt.Errorf("gcepd: RestoreFromSnapshot: empty source handle")
	}
	ctx = withCreds(ctx, mergedParam(opctx.Parameters, "credentialsJSON"))
	project, err := d.resolveProject(ctx, opctx)
	if err != nil {
		return "", err
	}
	zone, err := resolveZone(opctx, driver.ProvisionRequest{})
	if err != nil {
		return "", err
	}
	gib, err := bytesToGiB(req.SizeBytes)
	if err != nil {
		return "", err
	}
	name := d.diskName(opctx.Volume)
	if err := d.client.insertDisk(ctx, project, zone, diskSpec{
		Name:           name,
		SizeGB:         gib,
		DiskType:       mergedParamOr(opctx.Parameters, "diskType", "pd-balanced"),
		SourceSnapshot: string(req.SourceHandle),
		Labels:         d.diskLabels(opctx.Volume),
	}); err != nil {
		return "", fmt.Errorf("gcepd: insertDisk from snapshot: %w", err)
	}
	return driver.VolumeHandle(name), nil
}

func (d *pdDriver) DeleteSnapshot(ctx context.Context, opctx driver.OpContext, handle driver.SnapshotHandle) error {
	if handle == "" {
		return nil
	}
	ctx = withCreds(ctx, mergedParam(opctx.Parameters, "credentialsJSON"))
	project, err := d.resolveProject(ctx, opctx)
	if err != nil {
		return err
	}
	if err := d.client.deleteSnapshot(ctx, project, string(handle)); err != nil {
		if errors.Is(err, errNotFound) {
			return nil
		}
		return fmt.Errorf("gcepd: deleteSnapshot %s: %w", handle, err)
	}
	return nil
}

func (d *pdDriver) Expand(ctx context.Context, opctx driver.OpContext, handle driver.VolumeHandle, newSize string) error {
	if handle == "" {
		return fmt.Errorf("gcepd: Expand: empty handle")
	}
	ctx = withCreds(ctx, mergedParam(opctx.Parameters, "credentialsJSON"))
	project, err := d.resolveProject(ctx, opctx)
	if err != nil {
		return err
	}
	zone, err := resolveZone(opctx, driver.ProvisionRequest{})
	if err != nil {
		return err
	}
	b, err := parseQuantity(newSize)
	if err != nil {
		return fmt.Errorf("gcepd: Expand: parse size %q: %w", newSize, err)
	}
	gib, err := bytesToGiB(b)
	if err != nil {
		return err
	}
	disk, err := d.client.getDisk(ctx, project, zone, string(handle))
	if err != nil {
		return fmt.Errorf("gcepd: getDisk %s: %w", handle, err)
	}
	if gib <= disk.SizeGB {
		return nil // already at or above target; GCE PDs can't shrink
	}
	if err := d.client.resizeDisk(ctx, project, zone, string(handle), gib); err != nil {
		return fmt.Errorf("gcepd: resizeDisk: %w", err)
	}
	return nil
}

// --- helpers -------------------------------------------------------------

// resolveProject resolves the GCP project: explicit parameters.project,
// else the project of the GCE instance the controller runs on (metadata
// server). Errors when neither is available.
func (d *pdDriver) resolveProject(ctx context.Context, opctx driver.OpContext) (string, error) {
	if p := mergedParam(opctx.Parameters, "project"); p != "" {
		return p, nil
	}
	if metadata.OnGCE() {
		mctx, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
		if p, err := metadata.ProjectIDWithContext(mctx); err == nil && p != "" {
			return p, nil
		}
	}
	return "", fmt.Errorf("gcepd: parameters.project is required (no GCE metadata project available)")
}

func (d *pdDriver) diskLabels(v *types.Volume) map[string]string {
	return map[string]string{
		labelRuneNS:   gceLabelValue(v.Namespace),
		labelRuneName: gceLabelValue(v.Name),
	}
}

// diskName turns a Rune Volume into a GCE-acceptable disk name: 1–63
// chars, lowercase, starts with a letter, [-a-z0-9].
func (d *pdDriver) diskName(v *types.Volume) string {
	return sanitizeGCEName(d.cfg.DiskNamePrefix+v.Namespace+"-"+v.Name, 63)
}

// snapshotName: "<prefix><ns>-<name>-snap-..." sanitized to GCE's 63-char
// limit; the snapshot's own name keeps it unique.
func (d *pdDriver) snapshotName(s *types.Snapshot) string {
	return sanitizeGCEName(d.cfg.DiskNamePrefix+s.Namespace+"-"+s.Name+"-snap", 63)
}

// devicePath is the stable /dev/disk/by-id symlink GCE creates for a disk
// attached with DeviceName == the disk name.
func devicePath(diskName string) driver.DevicePath {
	return driver.DevicePath("/dev/disk/by-id/google-" + diskName)
}

// sanitizeGCEName lowercases, replaces invalid chars with '-', collapses
// hyphen runs, ensures a leading letter, and truncates (trimming a
// trailing hyphen). GCE names must match [a-z]([-a-z0-9]*[a-z0-9])?.
func sanitizeGCEName(s string, maxLen int) string {
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
		}
	}
	out := strings.TrimRight(b.String(), "-")
	if out == "" || out[0] < 'a' || out[0] > 'z' {
		out = "v" + out
	}
	if len(out) > maxLen {
		out = strings.TrimRight(out[:maxLen], "-")
	}
	return out
}

// gceLabelValue sanitizes a string for a GCE label value: lowercase,
// [-_a-z0-9], ≤63 chars. Empty maps to "none" (labels can't be empty).
func gceLabelValue(s string) string {
	s = strings.ToLower(s)
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	out := b.String()
	if len(out) > 63 {
		out = out[:63]
	}
	if out == "" {
		out = "none"
	}
	return out
}

// instanceLookupName returns the GCE instance name for the node. Prefer
// OpContext.NodeHostname (the OS hostname, which on GCE equals the
// instance name), falling back to the Rune NodeID.
func instanceLookupName(opctx driver.OpContext, node driver.NodeID) string {
	if opctx.NodeHostname != "" {
		return opctx.NodeHostname
	}
	return string(node)
}

// instanceNameFromSelfLink extracts the instance name (last path segment)
// from a GCE selfLink like
// https://.../projects/p/zones/z/instances/<name>.
func instanceNameFromSelfLink(selfLink string) string {
	if i := strings.LastIndex(selfLink, "/"); i >= 0 {
		return selfLink[i+1:]
	}
	return selfLink
}

func containsSelfLink(users []string, instanceSelfLink string) bool {
	want := instanceNameFromSelfLink(instanceSelfLink)
	for _, u := range users {
		if u == instanceSelfLink || instanceNameFromSelfLink(u) == want {
			return true
		}
	}
	return false
}

// resolveZone picks the zone for a disk: explicit parameters.zone wins,
// else the selected topology's rune.io/zone label, else an error (zonal
// PDs are zone-pinned).
func resolveZone(opctx driver.OpContext, req driver.ProvisionRequest) (string, error) {
	if z := mergedParam(opctx.Parameters, "zone"); z != "" {
		return z, nil
	}
	if req.Topology != nil {
		if z, ok := req.Topology.MatchLabels[types.TopologyLabelZone]; ok && z != "" {
			return z, nil
		}
	}
	return "", fmt.Errorf("gcepd: zone is required (set parameters.zone or a %s topology label)", types.TopologyLabelZone)
}

func mergedParam(p map[string]string, key string) string {
	if p == nil {
		return ""
	}
	if v, ok := p[key]; ok {
		return v
	}
	lk := strings.ToLower(key)
	for k, v := range p {
		if strings.ToLower(k) == lk {
			return v
		}
	}
	return ""
}

func mergedParamOr(p map[string]string, key, def string) string {
	if v := mergedParam(p, key); v != "" {
		return v
	}
	return def
}

func accessModeSupported(m types.AccessMode) bool {
	return m == "" || m == types.AccessModeRWO
}

// bytesToGiB rounds up to the next whole binary gibibyte with GCE's 10 GiB
// floor (the smallest Persistent Disk).
func bytesToGiB(b int64) (int64, error) {
	if b <= 0 {
		return 0, fmt.Errorf("gcepd: invalid size %d bytes (must be > 0)", b)
	}
	const gib = int64(1) << 30
	g := (b + gib - 1) / gib
	if g < 10 {
		g = 10
	}
	return g, nil
}

// parseQuantity is a minimal Kubernetes-style storage quantity parser,
// mirroring the helper in the sibling drivers.
func parseQuantity(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("empty quantity")
	}
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
