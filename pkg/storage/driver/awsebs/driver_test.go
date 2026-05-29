package awsebs

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/runestack/rune/pkg/storage/driver"
	runetypes "github.com/runestack/rune/pkg/types"
)

// ============================================================================
// fake EC2 client
// ============================================================================

// fakeEC2 is an in-memory test double for the EC2 surface awsebs uses.
// It enforces awsFromContext (region required) exactly like the real
// sdkClient so the region-required contract is exercised in tests.
type fakeEC2 struct {
	mu        sync.Mutex
	volumes   map[string]*ebsVolume
	tags      map[string]map[string]string // volID -> tags
	snapshots map[string]bool
	instances map[string]*ec2Instance // lookup name -> instance
	nextID    int

	createErr error // injected error for createVolume
}

func newFakeEC2() *fakeEC2 {
	return &fakeEC2{
		volumes:   map[string]*ebsVolume{},
		tags:      map[string]map[string]string{},
		snapshots: map[string]bool{},
		instances: map[string]*ec2Instance{},
	}
}

func (f *fakeEC2) addInstance(name, id string, devices ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.instances[name] = &ec2Instance{ID: id, Devices: devices}
}

func (f *fakeEC2) requireRegion(ctx context.Context) error {
	_, err := awsFromContext(ctx)
	return err
}

func (f *fakeEC2) create(ctx context.Context, in createVolumeIn, snapshotID string) (*ebsVolume, error) {
	if err := f.requireRegion(ctx); err != nil {
		return nil, err
	}
	if f.createErr != nil {
		return nil, f.createErr
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextID++
	id := "vol-" + itoa(f.nextID)
	v := &ebsVolume{ID: id, SizeGiB: in.SizeGiB, State: "available"}
	f.volumes[id] = v
	f.tags[id] = in.Tags
	return &ebsVolume{ID: id, SizeGiB: in.SizeGiB, State: "creating"}, nil
}

func (f *fakeEC2) createVolume(ctx context.Context, in createVolumeIn) (*ebsVolume, error) {
	return f.create(ctx, in, "")
}

func (f *fakeEC2) createVolumeFromSnapshot(ctx context.Context, in createVolumeIn, snapshotID string) (*ebsVolume, error) {
	return f.create(ctx, in, snapshotID)
}

func (f *fakeEC2) getVolume(ctx context.Context, id string) (*ebsVolume, error) {
	if err := f.requireRegion(ctx); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	v, ok := f.volumes[id]
	if !ok {
		return nil, errNotFound
	}
	return cloneVol(v), nil
}

func (f *fakeEC2) volumeByTag(ctx context.Context, key, value string) (*ebsVolume, error) {
	if err := f.requireRegion(ctx); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for id, tags := range f.tags {
		if tags[key] == value {
			return cloneVol(f.volumes[id]), nil
		}
	}
	return nil, errNotFound
}

func (f *fakeEC2) deleteVolume(ctx context.Context, id string) error {
	if err := f.requireRegion(ctx); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.volumes[id]; !ok {
		return errNotFound
	}
	delete(f.volumes, id)
	delete(f.tags, id)
	return nil
}

func (f *fakeEC2) attachVolume(ctx context.Context, volumeID, instanceID, device string) error {
	if err := f.requireRegion(ctx); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	v, ok := f.volumes[volumeID]
	if !ok {
		return errNotFound
	}
	v.Attachments = append(v.Attachments, ebsAttachment{
		InstanceID: instanceID, Device: device, State: string(types.VolumeAttachmentStateAttached),
	})
	v.State = string(types.VolumeStateInUse)
	return nil
}

func (f *fakeEC2) detachVolume(ctx context.Context, volumeID, instanceID string) error {
	if err := f.requireRegion(ctx); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	v, ok := f.volumes[volumeID]
	if !ok {
		return errNotFound
	}
	v.Attachments = nil
	v.State = string(types.VolumeStateAvailable)
	return nil
}

func (f *fakeEC2) modifyVolumeSize(ctx context.Context, volumeID string, sizeGiB int32) error {
	if err := f.requireRegion(ctx); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	v, ok := f.volumes[volumeID]
	if !ok {
		return errNotFound
	}
	v.SizeGiB = sizeGiB
	return nil
}

func (f *fakeEC2) createSnapshot(ctx context.Context, volumeID, description string, tags map[string]string) (string, error) {
	if err := f.requireRegion(ctx); err != nil {
		return "", err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.volumes[volumeID]; !ok {
		return "", errNotFound
	}
	f.nextID++
	id := "snap-" + itoa(f.nextID)
	f.snapshots[id] = true
	return id, nil
}

func (f *fakeEC2) deleteSnapshot(ctx context.Context, id string) error {
	if err := f.requireRegion(ctx); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.snapshots[id] {
		return errNotFound
	}
	delete(f.snapshots, id)
	return nil
}

func (f *fakeEC2) instanceByName(ctx context.Context, name string) (*ec2Instance, error) {
	if err := f.requireRegion(ctx); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	inst, ok := f.instances[name]
	if !ok {
		return nil, errNotFound
	}
	return inst, nil
}

func cloneVol(v *ebsVolume) *ebsVolume {
	c := *v
	c.Attachments = append([]ebsAttachment(nil), v.Attachments...)
	return &c
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

// ============================================================================
// fake mounter
// ============================================================================

type fakeMounter struct {
	mu        sync.Mutex
	formatted map[string]string
	mounts    map[string]string
}

func newFakeMounter() *fakeMounter {
	return &fakeMounter{formatted: map[string]string{}, mounts: map[string]string{}}
}

func (f *fakeMounter) MkdirAll(string, os.FileMode) error { return nil }
func (f *fakeMounter) EnsureFormatted(_ context.Context, dev, fsType string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.formatted[dev]; !ok {
		f.formatted[dev] = fsType
	}
	return nil
}
func (f *fakeMounter) Mount(_ context.Context, dev, target, _ string, _ bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.mounts[target] = dev
	return nil
}
func (f *fakeMounter) Unmount(_ context.Context, target string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.mounts, target)
	return nil
}

// ============================================================================
// helpers
// ============================================================================

func newTestDriver(fake *fakeEC2) (*ebsDriver, *fakeMounter) {
	cfg, _ := parseConfig(nil)
	m := newFakeMounter()
	return &ebsDriver{cfg: cfg, client: fake, mounts: m}, m
}

func mkVolume(name string) *runetypes.Volume {
	return &runetypes.Volume{
		ID:         "v-" + name,
		Name:       name,
		Namespace:  "default",
		Size:       "5Gi",
		AccessMode: runetypes.AccessModeRWO,
	}
}

// euw2OpCtx builds an OpContext with a region + AZ parameter map.
func euw2OpCtx(vol *runetypes.Volume) driver.OpContext {
	return driver.OpContext{
		Volume: vol,
		Parameters: map[string]string{
			"region":           "eu-west-2",
			"availabilityZone": "eu-west-2a",
		},
	}
}

// ============================================================================
// tests
// ============================================================================

func TestFactoryRegistration(t *testing.T) {
	d, err := driver.New(DriverName, map[string]any{})
	if err != nil {
		t.Fatalf("driver.New(aws-ebs): %v", err)
	}
	if d.Name() != DriverName {
		t.Fatalf("Name = %q; want %q", d.Name(), DriverName)
	}
	caps := d.Capabilities()
	if !caps.BlockDevice || !caps.Snapshots || !caps.Expand || !caps.OnlineExpand {
		t.Fatalf("unexpected caps: %+v", caps)
	}
	if len(caps.AccessModes) != 1 || caps.AccessModes[0] != runetypes.AccessModeRWO {
		t.Fatalf("expected only RWO, got %v", caps.AccessModes)
	}
	if len(caps.TopologyKeys) != 1 || caps.TopologyKeys[0] != runetypes.TopologyLabelZone {
		t.Fatalf("expected zone topology key, got %v", caps.TopologyKeys)
	}
}

func TestParseConfigErrors(t *testing.T) {
	if _, err := parseConfig(map[string]any{"volumeNamePrefix": 42}); err == nil {
		t.Fatal("expected parseConfig error for bad volumeNamePrefix type")
	}
}

func TestProvision_RegionRequired(t *testing.T) {
	d, _ := newTestDriver(newFakeEC2())
	_, err := d.Provision(context.Background(), driver.OpContext{
		Volume:     mkVolume("data"),
		Parameters: map[string]string{"availabilityZone": "eu-west-2a"},
	}, driver.ProvisionRequest{SizeBytes: 1 << 30})
	if err == nil || !strings.Contains(err.Error(), "region is required") {
		t.Fatalf("expected region-required error, got %v", err)
	}
}

func TestProvision_AZRequired(t *testing.T) {
	d, _ := newTestDriver(newFakeEC2())
	_, err := d.Provision(context.Background(), driver.OpContext{
		Volume:     mkVolume("data"),
		Parameters: map[string]string{"region": "eu-west-2"},
	}, driver.ProvisionRequest{SizeBytes: 1 << 30})
	if err == nil || !strings.Contains(err.Error(), "availabilityZone is required") {
		t.Fatalf("expected availabilityZone-required error, got %v", err)
	}
}

func TestProvision_HappyPath(t *testing.T) {
	fake := newFakeEC2()
	d, _ := newTestDriver(fake)
	vol := mkVolume("data")
	handle, err := d.Provision(context.Background(), euw2OpCtx(vol), driver.ProvisionRequest{SizeBytes: 5 << 30})
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	if handle == "" {
		t.Fatal("empty handle")
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.volumes) != 1 {
		t.Fatalf("expected 1 volume, got %d", len(fake.volumes))
	}
	tags := fake.tags[string(handle)]
	if tags[tagRuneVolumeID] != vol.ID {
		t.Fatalf("expected volume-id tag %q, got %q", vol.ID, tags[tagRuneVolumeID])
	}
	if !strings.HasPrefix(tags["Name"], "rune-default-data") {
		t.Fatalf("unexpected Name tag %q", tags["Name"])
	}
	if fake.volumes[string(handle)].SizeGiB != 5 {
		t.Fatalf("expected 5 GiB, got %d", fake.volumes[string(handle)].SizeGiB)
	}
}

func TestProvision_TopologyZone(t *testing.T) {
	fake := newFakeEC2()
	d, _ := newTestDriver(fake)
	opctx := driver.OpContext{
		Volume:     mkVolume("data"),
		Parameters: map[string]string{"region": "eu-west-2"}, // no availabilityZone param
	}
	req := driver.ProvisionRequest{
		SizeBytes: 1 << 30,
		Topology:  &runetypes.TopologySelector{MatchLabels: map[string]string{runetypes.TopologyLabelZone: "eu-west-2b"}},
	}
	if _, err := d.Provision(context.Background(), opctx, req); err != nil {
		t.Fatalf("Provision with topology zone: %v", err)
	}
}

func TestProvision_AdoptsExisting(t *testing.T) {
	fake := newFakeEC2()
	d, _ := newTestDriver(fake)
	opctx := euw2OpCtx(mkVolume("data"))
	req := driver.ProvisionRequest{SizeBytes: 5 << 30}
	first, err := d.Provision(context.Background(), opctx, req)
	if err != nil {
		t.Fatalf("first Provision: %v", err)
	}
	second, err := d.Provision(context.Background(), opctx, req)
	if err != nil {
		t.Fatalf("second Provision (adopt): %v", err)
	}
	if second != first {
		t.Fatalf("adopt returned %q, want existing %q", second, first)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.volumes) != 1 {
		t.Fatalf("expected 1 volume after adopt, got %d", len(fake.volumes))
	}
}

func TestProvision_AccessModeUnsupported(t *testing.T) {
	d, _ := newTestDriver(newFakeEC2())
	vol := mkVolume("data")
	vol.AccessMode = runetypes.AccessModeRWX
	_, err := d.Provision(context.Background(), euw2OpCtx(vol), driver.ProvisionRequest{SizeBytes: 1 << 30})
	if !errors.Is(err, driver.ErrAccessModeUnsupported) {
		t.Fatalf("expected ErrAccessModeUnsupported, got %v", err)
	}
}

func TestDelete_Idempotent(t *testing.T) {
	d, _ := newTestDriver(newFakeEC2())
	opctx := driver.OpContext{Parameters: map[string]string{"region": "eu-west-2"}}
	if err := d.Delete(context.Background(), opctx, "vol-missing"); err != nil {
		t.Fatalf("expected idempotent Delete, got %v", err)
	}
	if err := d.Delete(context.Background(), driver.OpContext{}, ""); err != nil {
		t.Fatalf("empty handle Delete: %v", err)
	}
}

func TestAttachDetach(t *testing.T) {
	fake := newFakeEC2()
	fake.addInstance("node-a", "i-aaa", "/dev/sda1")
	d, _ := newTestDriver(fake)
	opctx := euw2OpCtx(mkVolume("data"))
	handle, err := d.Provision(context.Background(), opctx, driver.ProvisionRequest{SizeBytes: 2 << 30})
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	dev, err := d.Attach(context.Background(), opctx, handle, "node-a")
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}
	wantDev := "/dev/disk/by-id/nvme-Amazon_Elastic_Block_Store_" + strings.ReplaceAll(string(handle), "-", "")
	if string(dev) != wantDev {
		t.Fatalf("device path = %q, want %q", dev, wantDev)
	}
	// Idempotent re-attach to the same instance.
	if _, err := d.Attach(context.Background(), opctx, handle, "node-a"); err != nil {
		t.Fatalf("re-Attach: %v", err)
	}
	// Attach to a different instance -> error.
	fake.addInstance("node-b", "i-bbb", "/dev/sda1")
	if _, err := d.Attach(context.Background(), opctx, handle, "node-b"); err == nil {
		t.Fatal("expected attach-to-different-instance error")
	}
	// Detach + idempotent re-detach.
	if err := d.Detach(context.Background(), opctx, handle, "node-a"); err != nil {
		t.Fatalf("Detach: %v", err)
	}
	if err := d.Detach(context.Background(), opctx, handle, "node-a"); err != nil {
		t.Fatalf("re-Detach: %v", err)
	}
}

func TestAttach_UsesNodeHostnameOverNodeID(t *testing.T) {
	fake := newFakeEC2()
	fake.addInstance("ip-10-0-1-23", "i-host", "/dev/sda1")
	d, _ := newTestDriver(fake)
	opctx := euw2OpCtx(mkVolume("data"))
	opctx.NodeHostname = "ip-10-0-1-23"
	handle, err := d.Provision(context.Background(), opctx, driver.ProvisionRequest{SizeBytes: 1 << 30})
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	// A Rune-style NodeID that matches no instance; lookup must use the hostname.
	if _, err := d.Attach(context.Background(), opctx, handle, "node-5d7a0ab4"); err != nil {
		t.Fatalf("Attach: %v", err)
	}
}

func TestAttach_UnknownNode(t *testing.T) {
	fake := newFakeEC2()
	d, _ := newTestDriver(fake)
	opctx := euw2OpCtx(mkVolume("d"))
	handle, _ := d.Provision(context.Background(), opctx, driver.ProvisionRequest{SizeBytes: 1 << 30})
	_, err := d.Attach(context.Background(), opctx, handle, "ghost")
	if err == nil || !strings.Contains(err.Error(), "no EC2 instance") {
		t.Fatalf("expected no-instance error, got %v", err)
	}
}

func TestMountUnmount(t *testing.T) {
	fake := newFakeEC2()
	fake.addInstance("node-a", "i-aaa", "/dev/sda1")
	d, mounter := newTestDriver(fake)
	opctx := euw2OpCtx(mkVolume("d"))
	handle, _ := d.Provision(context.Background(), opctx, driver.ProvisionRequest{SizeBytes: 1 << 30})
	dev, _ := d.Attach(context.Background(), opctx, handle, "node-a")
	target := "/var/lib/rune/mounts/" + string(handle)
	got, err := d.Mount(context.Background(), opctx, driver.MountOpts{
		Handle: handle, Device: dev, Target: driver.MountTarget(target), FsType: "ext4",
	})
	if err != nil {
		t.Fatalf("Mount: %v", err)
	}
	if string(got) != target {
		t.Fatalf("Mount returned %q want %q", got, target)
	}
	if mounter.formatted[string(dev)] != "ext4" {
		t.Fatalf("device not formatted: %+v", mounter.formatted)
	}
	if err := d.Unmount(context.Background(), opctx, got); err != nil {
		t.Fatalf("Unmount: %v", err)
	}
	if _, ok := mounter.mounts[target]; ok {
		t.Fatal("still mounted after Unmount")
	}
}

func TestMount_DerivesDeviceFromHandle(t *testing.T) {
	fake := newFakeEC2()
	d, mounter := newTestDriver(fake)
	opctx := euw2OpCtx(mkVolume("d"))
	handle, _ := d.Provision(context.Background(), opctx, driver.ProvisionRequest{SizeBytes: 1 << 30})
	// No Device set: the driver must derive the by-id path from the handle.
	if _, err := d.Mount(context.Background(), opctx, driver.MountOpts{
		Handle: handle, Target: "/mnt/x", FsType: "ext4",
	}); err != nil {
		t.Fatalf("Mount: %v", err)
	}
	wantDev := string(ebsDevicePath(string(handle)))
	if mounter.mounts["/mnt/x"] != wantDev {
		t.Fatalf("mounted %q, want derived %q", mounter.mounts["/mnt/x"], wantDev)
	}
}

func TestMount_ReadOnlySkipsFormat(t *testing.T) {
	fake := newFakeEC2()
	d, mounter := newTestDriver(fake)
	opctx := euw2OpCtx(mkVolume("d"))
	handle, _ := d.Provision(context.Background(), opctx, driver.ProvisionRequest{SizeBytes: 1 << 30})
	if _, err := d.Mount(context.Background(), opctx, driver.MountOpts{
		Handle: handle, Device: "/dev/x", Target: "/mnt/ro", ReadOnly: true,
	}); err != nil {
		t.Fatalf("Mount: %v", err)
	}
	if len(mounter.formatted) != 0 {
		t.Fatalf("expected no format on read-only mount, got %+v", mounter.formatted)
	}
}

func TestSnapshotAndRestore(t *testing.T) {
	fake := newFakeEC2()
	d, _ := newTestDriver(fake)
	opctx := euw2OpCtx(mkVolume("d"))
	handle, _ := d.Provision(context.Background(), opctx, driver.ProvisionRequest{SizeBytes: 2 << 30})
	snap := &runetypes.Snapshot{Name: "s1", Namespace: "default"}
	snapHandle, err := d.Snapshot(context.Background(), opctx, driver.SnapshotRequest{Handle: handle, Snapshot: snap})
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if snapHandle == "" {
		t.Fatal("empty snapshot handle")
	}
	restoreOpCtx := euw2OpCtx(mkVolume("restored"))
	restored, err := d.RestoreFromSnapshot(context.Background(), restoreOpCtx, driver.RestoreRequest{
		Source: snap, SourceHandle: snapHandle, SizeBytes: 2 << 30,
	})
	if err != nil {
		t.Fatalf("RestoreFromSnapshot: %v", err)
	}
	if restored == handle {
		t.Fatal("restored handle should differ from source")
	}
}

func TestDeleteSnapshot_Idempotent(t *testing.T) {
	fake := newFakeEC2()
	d, _ := newTestDriver(fake)
	opctx := driver.OpContext{Parameters: map[string]string{"region": "eu-west-2"}}
	if err := d.DeleteSnapshot(context.Background(), opctx, "snap-missing"); err != nil {
		t.Fatalf("expected idempotent DeleteSnapshot, got %v", err)
	}
	if err := d.DeleteSnapshot(context.Background(), driver.OpContext{}, ""); err != nil {
		t.Fatalf("empty handle DeleteSnapshot: %v", err)
	}
}

func TestExpand(t *testing.T) {
	fake := newFakeEC2()
	fake.addInstance("node-a", "i-aaa", "/dev/sda1")
	d, _ := newTestDriver(fake)
	opctx := euw2OpCtx(mkVolume("d"))
	handle, _ := d.Provision(context.Background(), opctx, driver.ProvisionRequest{SizeBytes: 2 << 30})
	// Grow.
	if err := d.Expand(context.Background(), opctx, handle, "10Gi"); err != nil {
		t.Fatalf("Expand: %v", err)
	}
	fake.mu.Lock()
	if fake.volumes[string(handle)].SizeGiB != 10 {
		fake.mu.Unlock()
		t.Fatalf("expected 10 GiB after expand, got %d", fake.volumes[string(handle)].SizeGiB)
	}
	fake.mu.Unlock()
	// Online expand: still works while attached (EBS supports it).
	if _, err := d.Attach(context.Background(), opctx, handle, "node-a"); err != nil {
		t.Fatalf("Attach: %v", err)
	}
	if err := d.Expand(context.Background(), opctx, handle, "20Gi"); err != nil {
		t.Fatalf("online Expand while attached: %v", err)
	}
	// Shrink / equal -> no-op, no error.
	if err := d.Expand(context.Background(), opctx, handle, "5Gi"); err != nil {
		t.Fatalf("no-op Expand: %v", err)
	}
}

func TestParseQuantity(t *testing.T) {
	cases := map[string]int64{
		"5Gi": 5 * (1 << 30), "5G": 5_000_000_000, "1024Mi": 1024 * (1 << 20), "500": 500, "1Ti": 1 << 40,
	}
	for in, want := range cases {
		got, err := parseQuantity(in)
		if err != nil || got != want {
			t.Errorf("parseQuantity(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "abc", "5XB", "-5"} {
		if _, err := parseQuantity(bad); err == nil {
			t.Errorf("parseQuantity(%q) expected error", bad)
		}
	}
}

func TestBytesToGiB(t *testing.T) {
	cases := map[int64]int32{
		1: 1, 1 << 30: 1, (1 << 30) + 1: 2, 5 << 30: 5,
	}
	for in, want := range cases {
		got, err := bytesToGiB(in)
		if err != nil || got != want {
			t.Errorf("bytesToGiB(%d) = %d, %v; want %d", in, got, err, want)
		}
	}
	if _, err := bytesToGiB(0); err == nil {
		t.Fatal("expected error for 0 bytes")
	}
	// Above the EBS max (64 TiB) -> error, not an overflowed int32.
	if _, err := bytesToGiB((maxEBSGiB + 1) * (1 << 30)); err == nil {
		t.Fatal("expected error for size beyond the EBS maximum")
	}
}

func TestPickDevice(t *testing.T) {
	if d := pickDevice([]string{"/dev/sda1"}); d != "/dev/sdf" {
		t.Fatalf("pickDevice = %q, want /dev/sdf", d)
	}
	if d := pickDevice([]string{"/dev/sda1", "/dev/sdf"}); d != "/dev/sdg" {
		t.Fatalf("pickDevice = %q, want /dev/sdg", d)
	}
	if d := pickDevice([]string{"/dev/sda1", "/dev/xvdf"}); d != "/dev/sdg" {
		t.Fatalf("pickDevice (xvd collision) = %q, want /dev/sdg", d)
	}
}

func TestEBSDevicePath(t *testing.T) {
	got := string(ebsDevicePath("vol-0a1b2c3d"))
	want := "/dev/disk/by-id/nvme-Amazon_Elastic_Block_Store_vol0a1b2c3d"
	if got != want {
		t.Fatalf("ebsDevicePath = %q, want %q", got, want)
	}
}
