package awsebs

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/runestack/rune/pkg/storage/driver"
	runetypes "github.com/runestack/rune/pkg/types"
)

func init() {
	driver.Register(DriverName, factory)
}

// factory is the registry entry point. cmd/runed (or tests) supplies the
// runefile [storage.drivers.aws-ebs] section. Region, AZ and auth are
// sourced per-call from StorageClass parameters, not from driver-level
// config.
func factory(raw map[string]any) (driver.Driver, error) {
	cfg, err := parseConfig(raw)
	if err != nil {
		return nil, err
	}
	return &ebsDriver{
		cfg:    cfg,
		client: newSDKClient(),
		mounts: execMounter{},
	}, nil
}

// ebsDriver implements driver.Driver against the EC2 / EBS API.
type ebsDriver struct {
	cfg    *Config
	client ec2Client // overridable for tests
	mounts mountExec // overridable for tests
}

// fallbackMountRoot is used when the agent passes an empty Mount target
// (ad-hoc tests). In production the agent passes
// /var/lib/rune/mounts/<vol.ID>/.
const fallbackMountRoot = "/var/lib/rune/mounts/awsebs"

// Tag keys the driver stamps on every volume it creates. The volume-id
// tag is the idempotency key Provision adopts on.
const (
	tagRuneVolumeID = "rune.io/volume-id"
	tagRuneName     = "rune.io/name"
	tagRuneNS       = "rune.io/namespace"
)

// --- Driver impl ---------------------------------------------------------

func (d *ebsDriver) Name() string { return DriverName }

func (d *ebsDriver) Capabilities() driver.Capabilities {
	return driver.Capabilities{
		AccessModes:  []runetypes.AccessMode{runetypes.AccessModeRWO},
		Snapshots:    true,
		Expand:       true,
		OnlineExpand: true, // EBS ModifyVolume grows in place while attached
		BlockDevice:  true,
		TopologyKeys: []string{runetypes.TopologyLabelZone},
	}
}

func (d *ebsDriver) Provision(ctx context.Context, opctx driver.OpContext, req driver.ProvisionRequest) (driver.VolumeHandle, error) {
	if opctx.Volume == nil {
		return "", fmt.Errorf("awsebs: Provision: OpContext.Volume is required")
	}
	ctx = d.withAWS(ctx, opctx)
	if !accessModeSupported(opctx.Volume.AccessMode) {
		return "", fmt.Errorf("awsebs: %w: %s", driver.ErrAccessModeUnsupported, opctx.Volume.AccessMode)
	}
	az, err := resolveAZ(opctx, req)
	if err != nil {
		return "", err
	}
	gib, err := bytesToGiB(req.SizeBytes)
	if err != nil {
		return "", err
	}

	// Idempotency / adopt: if a volume already carries this Rune volume's
	// id tag, reuse it (and its data) rather than creating a duplicate —
	// the same recovery path do-volume gets via DO's name-uniqueness 409.
	if existing, err := d.client.volumeByTag(ctx, tagRuneVolumeID, opctx.Volume.ID); err == nil {
		return driver.VolumeHandle(existing.ID), nil
	} else if !errors.Is(err, errNotFound) {
		return "", fmt.Errorf("awsebs: lookup existing volume by tag: %w", err)
	}

	vol, err := d.client.createVolume(ctx, createVolumeIn{
		AvailabilityZone: az,
		SizeGiB:          gib,
		VolumeType:       mergedParamOr(opctx.Parameters, "volumeType", "gp3"),
		Iops:             atoi32(mergedParam(opctx.Parameters, "iops")),
		Throughput:       atoi32(mergedParam(opctx.Parameters, "throughput")),
		Encrypted:        boolParam(opctx.Parameters, "encrypted", true),
		KmsKeyID:         mergedParam(opctx.Parameters, "kmsKeyId"),
		Tags:             d.volumeTags(opctx.Volume),
	})
	if err != nil {
		return "", fmt.Errorf("awsebs: createVolume: %w", err)
	}
	return driver.VolumeHandle(vol.ID), nil
}

func (d *ebsDriver) Delete(ctx context.Context, opctx driver.OpContext, handle driver.VolumeHandle) error {
	if handle == "" {
		return nil
	}
	ctx = d.withAWS(ctx, opctx)
	if err := d.client.deleteVolume(ctx, string(handle)); err != nil {
		if errors.Is(err, errNotFound) {
			return nil
		}
		return fmt.Errorf("awsebs: deleteVolume %s: %w", handle, err)
	}
	return nil
}

func (d *ebsDriver) Attach(ctx context.Context, opctx driver.OpContext, handle driver.VolumeHandle, node driver.NodeID) (driver.DevicePath, error) {
	if handle == "" {
		return "", fmt.Errorf("awsebs: Attach: empty handle")
	}
	if node == "" {
		return "", fmt.Errorf("awsebs: Attach: empty node id")
	}
	ctx = d.withAWS(ctx, opctx)

	lookupName := nodeLookupName(opctx, node)
	inst, err := d.client.instanceByName(ctx, lookupName)
	if err != nil {
		if errors.Is(err, errNotFound) {
			return "", fmt.Errorf("awsebs: no EC2 instance matches node %q (set the node's hostname or a Name tag to match the instance)", lookupName)
		}
		return "", fmt.Errorf("awsebs: instanceByName %s: %w", lookupName, err)
	}

	vol, err := d.client.getVolume(ctx, string(handle))
	if err != nil {
		return "", fmt.Errorf("awsebs: getVolume %s: %w", handle, err)
	}
	// Already attached to this instance? Nothing to do.
	if att, ok := attachmentFor(vol, inst.ID); ok {
		if att.State == string(types.VolumeAttachmentStateAttached) || att.State == string(types.VolumeAttachmentStateAttaching) {
			if err := d.waitAttached(ctx, string(handle), inst.ID); err != nil {
				return "", err
			}
			return ebsDevicePath(vol.ID), nil
		}
	}
	// Refuse to attach if it's already on a different instance — detach
	// must happen first via the orchestrator.
	for _, a := range vol.Attachments {
		if a.InstanceID != "" && a.InstanceID != inst.ID && a.State != string(types.VolumeAttachmentStateDetached) {
			return "", fmt.Errorf("awsebs: volume %s already attached to instance %s", handle, a.InstanceID)
		}
	}
	// Volume must be available before AttachVolume succeeds.
	if err := d.waitVolumeState(ctx, string(handle), string(types.VolumeStateAvailable)); err != nil {
		return "", err
	}
	device := pickDevice(inst.Devices)
	if err := d.client.attachVolume(ctx, string(handle), inst.ID, device); err != nil {
		return "", fmt.Errorf("awsebs: attachVolume: %w", err)
	}
	if err := d.waitAttached(ctx, string(handle), inst.ID); err != nil {
		return "", err
	}
	return ebsDevicePath(vol.ID), nil
}

func (d *ebsDriver) Detach(ctx context.Context, opctx driver.OpContext, handle driver.VolumeHandle, node driver.NodeID) error {
	if handle == "" {
		return nil
	}
	ctx = d.withAWS(ctx, opctx)
	vol, err := d.client.getVolume(ctx, string(handle))
	if err != nil {
		if errors.Is(err, errNotFound) {
			return nil // gone -> nothing to detach
		}
		return fmt.Errorf("awsebs: getVolume %s: %w", handle, err)
	}
	if len(vol.Attachments) == 0 {
		return nil // already detached
	}
	// Best-effort instance resolution; if the node was destroyed
	// out-of-band, detach from whichever instance is currently attached.
	instanceID := ""
	if node != "" {
		if inst, err := d.client.instanceByName(ctx, nodeLookupName(opctx, node)); err == nil {
			instanceID = inst.ID
		}
	}
	if err := d.client.detachVolume(ctx, string(handle), instanceID); err != nil {
		if errors.Is(err, errNotFound) {
			return nil
		}
		return fmt.Errorf("awsebs: detachVolume: %w", err)
	}
	return d.waitVolumeState(ctx, string(handle), string(types.VolumeStateAvailable))
}

func (d *ebsDriver) Mount(ctx context.Context, opctx driver.OpContext, opts driver.MountOpts) (driver.MountTarget, error) {
	dev := string(opts.Device)
	if dev == "" {
		// Derive the stable by-id device path from the volume ID (the
		// handle) — on Nitro instances the kernel-assigned /dev/nvmeXnY
		// name is unpredictable, but the by-id symlink is deterministic.
		dev = string(ebsDevicePath(string(opts.Handle)))
	}
	target := string(opts.Target)
	if target == "" {
		target = fallbackMountRoot + "/" + string(opts.Handle)
	}
	if err := d.mounts.MkdirAll(target, 0o750); err != nil {
		return "", fmt.Errorf("awsebs: mkdir %s: %w", target, err)
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

func (d *ebsDriver) Unmount(ctx context.Context, opctx driver.OpContext, target driver.MountTarget) error {
	if target == "" {
		return nil
	}
	return d.mounts.Unmount(ctx, string(target))
}

func (d *ebsDriver) Snapshot(ctx context.Context, opctx driver.OpContext, req driver.SnapshotRequest) (driver.SnapshotHandle, error) {
	if opctx.Volume == nil {
		return "", fmt.Errorf("awsebs: Snapshot: OpContext.Volume is required")
	}
	if req.Snapshot == nil {
		return "", fmt.Errorf("awsebs: Snapshot: nil Snapshot")
	}
	if req.Handle == "" {
		return "", fmt.Errorf("awsebs: Snapshot: empty volume handle")
	}
	ctx = d.withAWS(ctx, opctx)
	desc := fmt.Sprintf("rune snapshot %s/%s of volume %s", req.Snapshot.Namespace, req.Snapshot.Name, opctx.Volume.String())
	id, err := d.client.createSnapshot(ctx, string(req.Handle), desc, d.snapshotTags(opctx.Volume, req.Snapshot))
	if err != nil {
		return "", fmt.Errorf("awsebs: createSnapshot: %w", err)
	}
	return driver.SnapshotHandle(id), nil
}

func (d *ebsDriver) RestoreFromSnapshot(ctx context.Context, opctx driver.OpContext, req driver.RestoreRequest) (driver.VolumeHandle, error) {
	if req.Source == nil {
		return "", fmt.Errorf("awsebs: RestoreFromSnapshot: nil Source")
	}
	if opctx.Volume == nil {
		return "", fmt.Errorf("awsebs: RestoreFromSnapshot: OpContext.Volume (target) is required")
	}
	if req.SourceHandle == "" {
		return "", fmt.Errorf("awsebs: RestoreFromSnapshot: empty source handle")
	}
	ctx = d.withAWS(ctx, opctx)
	az, err := resolveAZ(opctx, driver.ProvisionRequest{})
	if err != nil {
		return "", err
	}
	gib, err := bytesToGiB(req.SizeBytes)
	if err != nil {
		return "", err
	}
	vol, err := d.client.createVolumeFromSnapshot(ctx, createVolumeIn{
		AvailabilityZone: az,
		SizeGiB:          gib,
		VolumeType:       mergedParamOr(opctx.Parameters, "volumeType", "gp3"),
		Encrypted:        boolParam(opctx.Parameters, "encrypted", true),
		KmsKeyID:         mergedParam(opctx.Parameters, "kmsKeyId"),
		Tags:             d.volumeTags(opctx.Volume),
	}, string(req.SourceHandle))
	if err != nil {
		return "", fmt.Errorf("awsebs: createVolumeFromSnapshot: %w", err)
	}
	return driver.VolumeHandle(vol.ID), nil
}

func (d *ebsDriver) DeleteSnapshot(ctx context.Context, opctx driver.OpContext, handle driver.SnapshotHandle) error {
	if handle == "" {
		return nil
	}
	ctx = d.withAWS(ctx, opctx)
	if err := d.client.deleteSnapshot(ctx, string(handle)); err != nil {
		if errors.Is(err, errNotFound) {
			return nil
		}
		return fmt.Errorf("awsebs: deleteSnapshot %s: %w", handle, err)
	}
	return nil
}

func (d *ebsDriver) Expand(ctx context.Context, opctx driver.OpContext, handle driver.VolumeHandle, newSize string) error {
	if handle == "" {
		return fmt.Errorf("awsebs: Expand: empty handle")
	}
	ctx = d.withAWS(ctx, opctx)
	b, err := parseQuantity(newSize)
	if err != nil {
		return fmt.Errorf("awsebs: Expand: parse size %q: %w", newSize, err)
	}
	gib, err := bytesToGiB(b)
	if err != nil {
		return err
	}
	vol, err := d.client.getVolume(ctx, string(handle))
	if err != nil {
		return fmt.Errorf("awsebs: getVolume %s: %w", handle, err)
	}
	if gib <= vol.SizeGiB {
		// Already at or above target; nothing to do. (EBS shrink is not
		// supported and would be a data-loss footgun.)
		return nil
	}
	// EBS supports online expand: no need to refuse when the volume is
	// in-use. The node-side filesystem grow happens separately on the
	// next mount cycle.
	if err := d.client.modifyVolumeSize(ctx, string(handle), gib); err != nil {
		return fmt.Errorf("awsebs: modifyVolume: %w", err)
	}
	return nil
}

// --- helpers -------------------------------------------------------------

func (d *ebsDriver) withAWS(ctx context.Context, opctx driver.OpContext) context.Context {
	return withAWS(ctx, awsParams{
		region:          mergedParam(opctx.Parameters, "region"),
		accessKeyID:     mergedParam(opctx.Parameters, "accessKeyId"),
		secretAccessKey: mergedParam(opctx.Parameters, "secretAccessKey"),
		sessionToken:    mergedParam(opctx.Parameters, "sessionToken"),
	})
}

func (d *ebsDriver) actionPollInterval() time.Duration {
	if sc, ok := d.client.(*sdkClient); ok {
		return sc.pollInterval
	}
	return 20 * time.Millisecond // fast default for fakes in tests
}

// waitVolumeState polls getVolume until State == want or ctx expires.
func (d *ebsDriver) waitVolumeState(ctx context.Context, handle, want string) error {
	return d.poll(ctx, func() (bool, error) {
		v, err := d.client.getVolume(ctx, handle)
		if err != nil {
			return false, err
		}
		return v.State == want, nil
	}, fmt.Sprintf("volume %s -> %s", handle, want))
}

// waitAttached polls until the volume reports an "attached" attachment
// to instanceID.
func (d *ebsDriver) waitAttached(ctx context.Context, handle, instanceID string) error {
	return d.poll(ctx, func() (bool, error) {
		v, err := d.client.getVolume(ctx, handle)
		if err != nil {
			return false, err
		}
		att, ok := attachmentFor(v, instanceID)
		return ok && att.State == string(types.VolumeAttachmentStateAttached), nil
	}, fmt.Sprintf("volume %s attached to %s", handle, instanceID))
}

func (d *ebsDriver) poll(ctx context.Context, cond func() (bool, error), what string) error {
	interval := d.actionPollInterval()
	if interval <= 0 {
		interval = 2 * time.Second
	}
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("awsebs: wait for %s: %w", what, ctx.Err())
		case <-timer.C:
		}
		done, err := cond()
		if err != nil {
			return err
		}
		if done {
			return nil
		}
		timer.Reset(interval)
	}
}

func (d *ebsDriver) volumeTags(v *runetypes.Volume) map[string]string {
	return map[string]string{
		"Name":          ebsName(d.cfg.VolumeNamePrefix, v.Namespace, v.Name),
		tagRuneVolumeID: v.ID,
		tagRuneNS:       v.Namespace,
		tagRuneName:     v.Name,
	}
}

func (d *ebsDriver) snapshotTags(v *runetypes.Volume, s *runetypes.Snapshot) map[string]string {
	return map[string]string{
		"Name":          ebsName(d.cfg.VolumeNamePrefix, s.Namespace, s.Name) + "-snap",
		tagRuneVolumeID: v.ID,
		tagRuneNS:       s.Namespace,
		tagRuneName:     s.Name,
	}
}

// ebsName builds the human-facing Name tag: "<prefix><ns>-<name>",
// trimmed to EBS's 255-char tag-value limit (far longer than we need).
func ebsName(prefix, ns, name string) string {
	raw := prefix + ns + "-" + name
	if len(raw) > 255 {
		raw = raw[:255]
	}
	return raw
}

// ebsDevicePath returns the stable /dev/disk/by-id symlink AWS creates
// for an attached EBS volume on Nitro instances: the volume ID with its
// hyphen removed, e.g. vol-0a1b... -> nvme-Amazon_Elastic_Block_Store_vol0a1b...
func ebsDevicePath(volumeID string) driver.DevicePath {
	id := strings.ReplaceAll(volumeID, "-", "")
	return driver.DevicePath("/dev/disk/by-id/nvme-Amazon_Elastic_Block_Store_" + id)
}

// pickDevice returns the first /dev/sd[f-p] name not already present in
// the instance's block-device mapping. AWS recommends sdf..sdp for
// additional Linux EBS volumes. The chosen name is largely cosmetic on
// Nitro (the kernel exposes NVMe names regardless) but AttachVolume
// requires a unique, unused device argument.
func pickDevice(existing []string) string {
	used := make(map[string]bool, len(existing))
	for _, e := range existing {
		used[e] = true
	}
	for c := byte('f'); c <= 'p'; c++ {
		name := "/dev/sd" + string(c)
		if !used[name] && !used["/dev/xvd"+string(c)] {
			return name
		}
	}
	return "/dev/sdf"
}

func attachmentFor(v *ebsVolume, instanceID string) (ebsAttachment, bool) {
	for _, a := range v.Attachments {
		if a.InstanceID == instanceID {
			return a, true
		}
	}
	return ebsAttachment{}, false
}

// nodeLookupName returns the name to resolve to an EC2 instance for the
// agent's node. Prefer OpContext.NodeHostname (the OS hostname, which on
// EC2 is the private-DNS name), falling back to the Rune NodeID. The
// fallback exists for tests and non-agent callers that don't populate
// NodeHostname; in production the agent always sets it.
func nodeLookupName(opctx driver.OpContext, node driver.NodeID) string {
	if opctx.NodeHostname != "" {
		return opctx.NodeHostname
	}
	return string(node)
}

// resolveAZ picks the Availability Zone for a new volume: explicit
// parameters.availabilityZone wins, else the selected topology's
// rune.io/zone label, else an error (EBS volumes are AZ-pinned).
func resolveAZ(opctx driver.OpContext, req driver.ProvisionRequest) (string, error) {
	if az := mergedParam(opctx.Parameters, "availabilityZone"); az != "" {
		return az, nil
	}
	if req.Topology != nil {
		if az, ok := req.Topology.MatchLabels[runetypes.TopologyLabelZone]; ok && az != "" {
			return az, nil
		}
	}
	return "", fmt.Errorf("awsebs: availabilityZone is required (set parameters.availabilityZone or a %s topology label)", runetypes.TopologyLabelZone)
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

func boolParam(p map[string]string, key string, def bool) bool {
	v := strings.ToLower(strings.TrimSpace(mergedParam(p, key)))
	switch v {
	case "":
		return def
	case "true", "1", "yes":
		return true
	default:
		return false
	}
}

func atoi32(s string) int32 {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0
	}
	var n int32
	if _, err := fmt.Sscanf(s, "%d", &n); err != nil || n < 0 {
		return 0
	}
	return n
}

func accessModeSupported(m runetypes.AccessMode) bool {
	return m == "" || m == runetypes.AccessModeRWO
}

// bytesToGiB rounds up to the next whole binary gibibyte (EBS sizes are
// in GiB) with a 1 GiB floor — EBS's minimum volume size.
func bytesToGiB(b int64) (int32, error) {
	if b <= 0 {
		return 0, fmt.Errorf("awsebs: invalid size %d bytes (must be > 0)", b)
	}
	const gib = int64(1) << 30
	g := (b + gib - 1) / gib
	if g < 1 {
		g = 1
	}
	return int32(g), nil
}

// parseQuantity is a minimal Kubernetes-style storage quantity parser
// covering the suffixes Rune Volume.Size strings use: bare bytes,
// decimal SI ("K","M","G","T") and binary IEC ("Ki","Mi","Gi","Ti").
// Mirrors the do-volume parser; if a shared one lands in pkg/types it
// should replace both.
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
