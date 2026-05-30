package hcloudvolume

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/runestack/rune/pkg/storage/driver"
	"github.com/runestack/rune/pkg/types"
)

func init() {
	driver.Register(DriverName, factory)
}

// factory is the registry entry point. cmd/runed (or tests) supplies
// the runefile [storage.drivers.hcloud-volume] section. Auth is sourced
// per-call from the StorageClass `parameters.apiToken` value, not from
// driver-level config.
func factory(raw map[string]any) (driver.Driver, error) {
	cfg, err := parseConfig(raw)
	if err != nil {
		return nil, err
	}
	return &hcloudVolumeDriver{
		cfg:    cfg,
		client: newHTTPClient(cfg),
		mounts: execMounter{},
	}, nil
}

// hcloudVolumeDriver implements driver.Driver against the Hetzner Cloud API.
type hcloudVolumeDriver struct {
	cfg    *Config
	client hcloudClient // overridable for tests
	mounts mountExec    // overridable for tests
}

// fallbackMountRoot is the parent directory the driver mounts each
// volume under when the caller passes an empty Target.
const fallbackMountRoot = "/var/lib/rune/mounts/hcloudvolume"

// --- Driver impl ---------------------------------------------------------

func (d *hcloudVolumeDriver) Name() string { return DriverName }

func (d *hcloudVolumeDriver) Capabilities() driver.Capabilities {
	return driver.Capabilities{
		AccessModes: []types.AccessMode{
			types.AccessModeRWO,
		},
		// Hetzner Cloud volumes do not support snapshots.
		Snapshots:    false,
		Expand:       true,
		OnlineExpand: false,
		BlockDevice:  true,
		TopologyKeys: []string{"rune.io/region"},
	}
}

func (d *hcloudVolumeDriver) Provision(ctx context.Context, opctx driver.OpContext, req driver.ProvisionRequest) (driver.VolumeHandle, error) {
	if opctx.Volume == nil {
		return "", fmt.Errorf("hcloudvolume: Provision: OpContext.Volume is required")
	}
	ctx = withToken(ctx, mergedParam(opctx.Parameters, "apiToken"))
	location := mergedParam(opctx.Parameters, "location")
	if location == "" {
		// Accept `region` as an alias so operators sharing topology keys
		// with dovolume don't trip over Hetzner's nomenclature.
		location = mergedParam(opctx.Parameters, "region")
	}
	if location == "" {
		return "", fmt.Errorf("hcloudvolume: Provision: parameters.location is required")
	}
	if !accessModeSupported(opctx.Volume.AccessMode) {
		return "", fmt.Errorf("hcloudvolume: %w: %s", driver.ErrAccessModeUnsupported, opctx.Volume.AccessMode)
	}
	gigabytes, err := bytesToGigabytes(req.SizeBytes)
	if err != nil {
		return "", err
	}
	// Hetzner enforces a 10 GB minimum.
	if gigabytes < 10 {
		gigabytes = 10
	}
	hcName := d.hcVolumeName(opctx.Volume)
	// Adopt an existing volume with the same name before creating a new
	// one. Hetzner volume names are project-unique and hcName is derived
	// deterministically from the Rune volume's namespace+name, so a
	// re-cast after the Rune Volume row was lost (or a retried Provision)
	// reuses the existing volume and its data instead of dead-ending on
	// Hetzner's name-uniqueness error. Mirrors the adopt-on-conflict path
	// in dovolume / aws-ebs.
	if existing, err := d.client.volumeByName(ctx, hcName, location); err == nil {
		return driver.VolumeHandle(strconv.FormatInt(existing.ID, 10)), nil
	} else if !errors.Is(err, errHCNotFound) {
		return "", fmt.Errorf("hcloudvolume: lookup existing volume %q: %w", hcName, err)
	}
	in := createVolumeIn{
		Name:      hcName,
		Size:      gigabytes,
		Location:  location,
		Automount: false,
		// Format is intentionally left empty — we format on first Mount
		// the same way dovolume does, so behaviour is consistent across
		// cloud drivers.
	}
	vol, act, err := d.client.createVolume(ctx, in)
	if err != nil {
		return "", fmt.Errorf("hcloudvolume: createVolume: %w", err)
	}
	if act != nil && act.Status == "running" {
		if err := waitForAction(ctx, d.client, act.ID, d.actionPollInterval()); err != nil {
			return "", err
		}
	}
	return driver.VolumeHandle(strconv.FormatInt(vol.ID, 10)), nil
}

func (d *hcloudVolumeDriver) Delete(ctx context.Context, opctx driver.OpContext, handle driver.VolumeHandle) error {
	if handle == "" {
		return nil
	}
	id, err := parseHandle(handle)
	if err != nil {
		return err
	}
	ctx = withToken(ctx, mergedParam(opctx.Parameters, "apiToken"))
	if err := d.client.deleteVolume(ctx, id); err != nil {
		if errors.Is(err, errHCNotFound) {
			return nil
		}
		return fmt.Errorf("hcloudvolume: deleteVolume %d: %w", id, err)
	}
	return nil
}

func (d *hcloudVolumeDriver) Attach(ctx context.Context, opctx driver.OpContext, handle driver.VolumeHandle, node driver.NodeID) (driver.DevicePath, error) {
	if handle == "" {
		return "", fmt.Errorf("hcloudvolume: Attach: empty handle")
	}
	if node == "" {
		return "", fmt.Errorf("hcloudvolume: Attach: empty node id")
	}
	id, err := parseHandle(handle)
	if err != nil {
		return "", err
	}
	ctx = withToken(ctx, mergedParam(opctx.Parameters, "apiToken"))
	vol, err := d.client.getVolume(ctx, id)
	if err != nil {
		return "", fmt.Errorf("hcloudvolume: getVolume %d: %w", id, err)
	}
	lookupName := serverLookupName(opctx, node)
	serverID, err := d.client.serverByName(ctx, lookupName)
	if err != nil {
		if errors.Is(err, errHCNotFound) {
			return "", fmt.Errorf("hcloudvolume: no Hetzner server matches hostname %q (set the node's hostname to match the server name)", lookupName)
		}
		return "", fmt.Errorf("hcloudvolume: serverByName %s: %w", lookupName, err)
	}
	// Already attached to this server? Nothing to do.
	if vol.Server != nil && *vol.Server == serverID {
		return hcDevicePath(vol), nil
	}
	// Refuse to attach if it's already on a different server — detach
	// must happen first via the orchestrator.
	if vol.Server != nil {
		return "", fmt.Errorf("hcloudvolume: volume %d already attached to server %d", id, *vol.Server)
	}
	act, err := d.client.attachVolume(ctx, id, serverID)
	if err != nil {
		return "", fmt.Errorf("hcloudvolume: attach action: %w", err)
	}
	if err := waitForAction(ctx, d.client, act.ID, d.actionPollInterval()); err != nil {
		return "", err
	}
	return hcDevicePath(vol), nil
}

func (d *hcloudVolumeDriver) Detach(ctx context.Context, opctx driver.OpContext, handle driver.VolumeHandle, node driver.NodeID) error {
	if handle == "" {
		return nil
	}
	id, err := parseHandle(handle)
	if err != nil {
		return err
	}
	ctx = withToken(ctx, mergedParam(opctx.Parameters, "apiToken"))
	vol, err := d.client.getVolume(ctx, id)
	if err != nil {
		if errors.Is(err, errHCNotFound) {
			return nil // gone -> nothing to detach
		}
		return fmt.Errorf("hcloudvolume: getVolume %d: %w", id, err)
	}
	if vol.Server == nil {
		return nil // already detached
	}
	act, err := d.client.detachVolume(ctx, id)
	if err != nil {
		return fmt.Errorf("hcloudvolume: detach action: %w", err)
	}
	if err := waitForAction(ctx, d.client, act.ID, d.actionPollInterval()); err != nil {
		return err
	}
	return nil
}

func (d *hcloudVolumeDriver) Mount(ctx context.Context, opctx driver.OpContext, opts driver.MountOpts) (driver.MountTarget, error) {
	dev := string(opts.Device)
	if dev == "" {
		// Caller (the agent) may not have populated Device — look the
		// volume up and derive its linux_device path.
		id, err := parseHandle(opts.Handle)
		if err != nil {
			return "", fmt.Errorf("hcloudvolume: Mount: %w", err)
		}
		ctx = withToken(ctx, mergedParam(opctx.Parameters, "apiToken"))
		vol, err := d.client.getVolume(ctx, id)
		if err != nil {
			return "", fmt.Errorf("hcloudvolume: Mount: lookup volume %d: %w", id, err)
		}
		dev = string(hcDevicePath(vol))
	}
	target := string(opts.Target)
	if target == "" {
		target = fallbackMountRoot + "/" + string(opts.Handle)
	}
	if err := d.mounts.MkdirAll(target, 0o750); err != nil {
		return "", fmt.Errorf("hcloudvolume: mkdir %s: %w", target, err)
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

func (d *hcloudVolumeDriver) Unmount(ctx context.Context, opctx driver.OpContext, target driver.MountTarget) error {
	if target == "" {
		return nil
	}
	return d.mounts.Unmount(ctx, string(target))
}

func (d *hcloudVolumeDriver) Snapshot(ctx context.Context, opctx driver.OpContext, req driver.SnapshotRequest) (driver.SnapshotHandle, error) {
	return "", driver.ErrUnsupported
}

func (d *hcloudVolumeDriver) RestoreFromSnapshot(ctx context.Context, opctx driver.OpContext, req driver.RestoreRequest) (driver.VolumeHandle, error) {
	return "", driver.ErrUnsupported
}

func (d *hcloudVolumeDriver) DeleteSnapshot(ctx context.Context, opctx driver.OpContext, handle driver.SnapshotHandle) error {
	return driver.ErrUnsupported
}

func (d *hcloudVolumeDriver) Expand(ctx context.Context, opctx driver.OpContext, handle driver.VolumeHandle, newSize string) error {
	if handle == "" {
		return fmt.Errorf("hcloudvolume: Expand: empty handle")
	}
	id, err := parseHandle(handle)
	if err != nil {
		return err
	}
	ctx = withToken(ctx, mergedParam(opctx.Parameters, "apiToken"))
	bytes, err := parseQuantity(newSize)
	if err != nil {
		return fmt.Errorf("hcloudvolume: Expand: parse size %q: %w", newSize, err)
	}
	gigabytes, err := bytesToGigabytes(bytes)
	if err != nil {
		return err
	}
	if gigabytes < 10 {
		gigabytes = 10
	}
	vol, err := d.client.getVolume(ctx, id)
	if err != nil {
		return fmt.Errorf("hcloudvolume: getVolume %d: %w", id, err)
	}
	// Hetzner requires offline expand: refuse if currently attached.
	if vol.Server != nil {
		return driver.ErrOnlineExpandUnsupported
	}
	if gigabytes <= vol.Size {
		// Already at or above target size; nothing to do.
		return nil
	}
	act, err := d.client.resizeVolume(ctx, id, gigabytes)
	if err != nil {
		return fmt.Errorf("hcloudvolume: resize action: %w", err)
	}
	return waitForAction(ctx, d.client, act.ID, d.actionPollInterval())
}

// --- helpers -------------------------------------------------------------

func (d *hcloudVolumeDriver) actionPollInterval() time.Duration {
	if hc, ok := d.client.(*httpClient); ok {
		return hc.pollInterval
	}
	return 50 * time.Millisecond // fast default for fake clients in tests
}

// hcVolumeName turns a Rune Volume into a Hetzner-acceptable volume
// name: 1–64 chars, must start and end with an alphanumeric and may
// contain `-`, `.`. Lowercased for consistency with dovolume.
func (d *hcloudVolumeDriver) hcVolumeName(v *types.Volume) string {
	raw := d.cfg.VolumeNamePrefix + v.Namespace + "-" + v.Name
	return sanitizeHCName(raw, 64)
}

// sanitizeHCName lowercases, replaces invalid chars with '-', collapses
// runs of hyphens, ensures the leading char is alphanumeric, and
// truncates to maxLen, also trimming a trailing hyphen.
func sanitizeHCName(s string, maxLen int) string {
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
	// Must start with a letter or digit.
	if out == "" || !isAlnum(out[0]) {
		out = "v" + out
	}
	if len(out) > maxLen {
		out = out[:maxLen]
		out = strings.TrimRight(out, "-")
	}
	return out
}

func isAlnum(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= '0' && b <= '9')
}

// hcDevicePath returns the well-known /dev/disk/by-id path Hetzner
// assigns to attached volumes. The API also surfaces this as
// volume.linux_device once attached; we prefer the API-supplied value
// when available, falling back to the documented format.
func hcDevicePath(v *hcVolume) driver.DevicePath {
	if v != nil && v.LinuxDevice != "" {
		return driver.DevicePath(v.LinuxDevice)
	}
	return driver.DevicePath(fmt.Sprintf("/dev/disk/by-id/scsi-0HC_Volume_%d", v.ID))
}

// serverLookupName returns the name to feed to /v1/servers?name=... for
// the agent's node. Prefer OpContext.NodeHostname (the OS hostname,
// which matches the Hetzner server name by default), falling back to
// the Rune NodeID.
func serverLookupName(opctx driver.OpContext, node driver.NodeID) string {
	if opctx.NodeHostname != "" {
		return opctx.NodeHostname
	}
	return string(node)
}

func parseHandle(h driver.VolumeHandle) (int64, error) {
	s := strings.TrimSpace(string(h))
	if s == "" {
		return 0, fmt.Errorf("hcloudvolume: empty handle")
	}
	id, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("hcloudvolume: invalid handle %q: %w", s, err)
	}
	return id, nil
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

func accessModeSupported(m types.AccessMode) bool {
	return m == "" || m == types.AccessModeRWO
}

// bytesToGigabytes rounds up to the next whole Hetzner gigabyte.
// Hetzner's API uses decimal GB (10^9), not GiB.
func bytesToGigabytes(b int64) (int64, error) {
	if b <= 0 {
		return 0, fmt.Errorf("hcloudvolume: invalid size %d bytes (must be > 0)", b)
	}
	const gb = int64(1_000_000_000)
	gigabytes := (b + gb - 1) / gb
	if gigabytes < 1 {
		gigabytes = 1
	}
	return gigabytes, nil
}

// parseQuantity is a minimal Kubernetes-style storage quantity parser
// covering the suffixes Rune Volume.Size strings actually use. Mirrors
// the helper in dovolume — if/when a shared parser lands in pkg/types
// both should switch over.
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
