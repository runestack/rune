package gcepd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"

	compute "google.golang.org/api/compute/v1"
	"google.golang.org/api/googleapi"

	"github.com/runestack/rune/pkg/storage/driver"
	"github.com/runestack/rune/pkg/types"
)

// ============================================================================
// fake GCE client
// ============================================================================

// fakeGCE is an in-memory test double for the Compute API surface gcepd
// uses. Tests run in a single project/zone, so those args are accepted
// but not keyed on.
type fakeGCE struct {
	mu        sync.Mutex
	disks     map[string]*gceDisk          // name -> disk
	labels    map[string]map[string]string // name -> labels
	instances map[string]*gceInstance      // name -> instance
	snapshots map[string]bool

	insertErr error
}

func newFakeGCE() *fakeGCE {
	return &fakeGCE{
		disks:     map[string]*gceDisk{},
		labels:    map[string]map[string]string{},
		instances: map[string]*gceInstance{},
		snapshots: map[string]bool{},
	}
}

const selfPrefix = "https://www.googleapis.com/compute/v1/projects/test/zones/europe-west2-a"

func (f *fakeGCE) addInstance(name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.instances[name] = &gceInstance{Name: name, SelfLink: selfPrefix + "/instances/" + name}
}

func (f *fakeGCE) insertDisk(_ context.Context, _, _ string, spec diskSpec) error {
	if f.insertErr != nil {
		return f.insertErr
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.disks[spec.Name] = &gceDisk{
		Name:     spec.Name,
		SelfLink: selfPrefix + "/disks/" + spec.Name,
		SizeGB:   spec.SizeGB,
		Status:   "READY",
	}
	f.labels[spec.Name] = spec.Labels
	return nil
}

func (f *fakeGCE) getDisk(_ context.Context, _, _, name string) (*gceDisk, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	d, ok := f.disks[name]
	if !ok {
		return nil, errNotFound
	}
	c := *d
	c.Users = append([]string(nil), d.Users...)
	return &c, nil
}

func (f *fakeGCE) deleteDisk(_ context.Context, _, _, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.disks[name]; !ok {
		return errNotFound
	}
	delete(f.disks, name)
	return nil
}

func (f *fakeGCE) resizeDisk(_ context.Context, _, _, name string, sizeGB int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	d, ok := f.disks[name]
	if !ok {
		return errNotFound
	}
	d.SizeGB = sizeGB
	return nil
}

func (f *fakeGCE) attachDisk(_ context.Context, _, _, instance string, spec attachSpec) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	d, ok := f.disks[spec.DeviceName]
	if !ok {
		return errNotFound
	}
	inst, ok := f.instances[instance]
	if !ok {
		return errNotFound
	}
	d.Users = append(d.Users, inst.SelfLink)
	return nil
}

func (f *fakeGCE) detachDisk(_ context.Context, _, _, _, deviceName string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	d, ok := f.disks[deviceName]
	if !ok {
		return errNotFound
	}
	d.Users = nil
	return nil
}

func (f *fakeGCE) createSnapshot(_ context.Context, _, _, diskName, snapshotName, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.disks[diskName]; !ok {
		return errNotFound
	}
	f.snapshots[snapshotName] = true
	return nil
}

func (f *fakeGCE) deleteSnapshot(_ context.Context, _, snapshotName string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.snapshots[snapshotName] {
		return errNotFound
	}
	delete(f.snapshots, snapshotName)
	return nil
}

func (f *fakeGCE) getInstance(_ context.Context, _, _, name string) (*gceInstance, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	inst, ok := f.instances[name]
	if !ok {
		return nil, errNotFound
	}
	return inst, nil
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

func newTestDriver(fake *fakeGCE) (*pdDriver, *fakeMounter) {
	cfg, _ := parseConfig(nil)
	m := newFakeMounter()
	return &pdDriver{cfg: cfg, client: fake, mounts: m}, m
}

func mkVolume(name string) *types.Volume {
	return &types.Volume{ID: "v-" + name, Name: name, Namespace: "default", Size: "10Gi", AccessMode: types.AccessModeRWO}
}

func euw2OpCtx(vol *types.Volume) driver.OpContext {
	return driver.OpContext{
		Volume:     vol,
		Parameters: map[string]string{"project": "test", "zone": "europe-west2-a"},
	}
}

// ============================================================================
// tests
// ============================================================================

func TestFactoryRegistration(t *testing.T) {
	d, err := driver.New(DriverName, map[string]any{})
	if err != nil {
		t.Fatalf("driver.New(gce-pd): %v", err)
	}
	if d.Name() != DriverName {
		t.Fatalf("Name = %q; want %q", d.Name(), DriverName)
	}
	caps := d.Capabilities()
	if !caps.BlockDevice || !caps.Snapshots || !caps.Expand || !caps.OnlineExpand {
		t.Fatalf("unexpected caps: %+v", caps)
	}
	if len(caps.AccessModes) != 1 || caps.AccessModes[0] != types.AccessModeRWO {
		t.Fatalf("expected only RWO, got %v", caps.AccessModes)
	}
	if len(caps.TopologyKeys) != 1 || caps.TopologyKeys[0] != types.TopologyLabelZone {
		t.Fatalf("expected zone topology key, got %v", caps.TopologyKeys)
	}
}

func TestParseConfigErrors(t *testing.T) {
	if _, err := parseConfig(map[string]any{"diskNamePrefix": 42}); err == nil {
		t.Fatal("expected parseConfig error for bad diskNamePrefix type")
	}
}

func TestProvision_ProjectRequired(t *testing.T) {
	d, _ := newTestDriver(newFakeGCE())
	_, err := d.Provision(context.Background(), driver.OpContext{
		Volume:     mkVolume("data"),
		Parameters: map[string]string{"zone": "europe-west2-a"},
	}, driver.ProvisionRequest{SizeBytes: 10 << 30})
	if err == nil || !strings.Contains(err.Error(), "project is required") {
		t.Fatalf("expected project-required error, got %v", err)
	}
}

func TestProvision_ZoneRequired(t *testing.T) {
	d, _ := newTestDriver(newFakeGCE())
	_, err := d.Provision(context.Background(), driver.OpContext{
		Volume:     mkVolume("data"),
		Parameters: map[string]string{"project": "test"},
	}, driver.ProvisionRequest{SizeBytes: 10 << 30})
	if err == nil || !strings.Contains(err.Error(), "zone is required") {
		t.Fatalf("expected zone-required error, got %v", err)
	}
}

func TestProvision_HappyPath(t *testing.T) {
	fake := newFakeGCE()
	d, _ := newTestDriver(fake)
	vol := mkVolume("data")
	handle, err := d.Provision(context.Background(), euw2OpCtx(vol), driver.ProvisionRequest{SizeBytes: 20 << 30})
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	if !strings.HasPrefix(string(handle), "rune-default-data") {
		t.Fatalf("unexpected disk name %q", handle)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.disks) != 1 {
		t.Fatalf("expected 1 disk, got %d", len(fake.disks))
	}
	if fake.disks[string(handle)].SizeGB != 20 {
		t.Fatalf("expected 20 GiB, got %d", fake.disks[string(handle)].SizeGB)
	}
	if fake.labels[string(handle)][labelRuneName] != "data" {
		t.Fatalf("expected rune-name label 'data', got %q", fake.labels[string(handle)][labelRuneName])
	}
}

func TestProvision_TopologyZone(t *testing.T) {
	fake := newFakeGCE()
	d, _ := newTestDriver(fake)
	opctx := driver.OpContext{
		Volume:     mkVolume("data"),
		Parameters: map[string]string{"project": "test"}, // no zone param
	}
	req := driver.ProvisionRequest{
		SizeBytes: 10 << 30,
		Topology:  &types.TopologySelector{MatchLabels: map[string]string{types.TopologyLabelZone: "europe-west2-b"}},
	}
	if _, err := d.Provision(context.Background(), opctx, req); err != nil {
		t.Fatalf("Provision with topology zone: %v", err)
	}
}

func TestProvision_AdoptsExisting(t *testing.T) {
	fake := newFakeGCE()
	d, _ := newTestDriver(fake)
	opctx := euw2OpCtx(mkVolume("data"))
	req := driver.ProvisionRequest{SizeBytes: 10 << 30}
	first, err := d.Provision(context.Background(), opctx, req)
	if err != nil {
		t.Fatalf("first Provision: %v", err)
	}
	second, err := d.Provision(context.Background(), opctx, req)
	if err != nil {
		t.Fatalf("second Provision (adopt): %v", err)
	}
	if second != first {
		t.Fatalf("adopt returned %q, want %q", second, first)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.disks) != 1 {
		t.Fatalf("expected 1 disk after adopt, got %d", len(fake.disks))
	}
}

func TestProvision_AccessModeUnsupported(t *testing.T) {
	d, _ := newTestDriver(newFakeGCE())
	vol := mkVolume("data")
	vol.AccessMode = types.AccessModeRWX
	_, err := d.Provision(context.Background(), euw2OpCtx(vol), driver.ProvisionRequest{SizeBytes: 10 << 30})
	if !errors.Is(err, driver.ErrAccessModeUnsupported) {
		t.Fatalf("expected ErrAccessModeUnsupported, got %v", err)
	}
}

func TestDelete_Idempotent(t *testing.T) {
	d, _ := newTestDriver(newFakeGCE())
	opctx := driver.OpContext{Parameters: map[string]string{"project": "test", "zone": "europe-west2-a"}}
	if err := d.Delete(context.Background(), opctx, "missing-disk"); err != nil {
		t.Fatalf("expected idempotent Delete, got %v", err)
	}
	if err := d.Delete(context.Background(), driver.OpContext{}, ""); err != nil {
		t.Fatalf("empty handle Delete: %v", err)
	}
}

func TestAttachDetach(t *testing.T) {
	fake := newFakeGCE()
	fake.addInstance("node-a")
	d, _ := newTestDriver(fake)
	opctx := euw2OpCtx(mkVolume("data"))
	handle, err := d.Provision(context.Background(), opctx, driver.ProvisionRequest{SizeBytes: 10 << 30})
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	dev, err := d.Attach(context.Background(), opctx, handle, "node-a")
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}
	if string(dev) != "/dev/disk/by-id/google-"+string(handle) {
		t.Fatalf("unexpected device path %q", dev)
	}
	// Idempotent re-attach.
	if _, err := d.Attach(context.Background(), opctx, handle, "node-a"); err != nil {
		t.Fatalf("re-Attach: %v", err)
	}
	// Attach to a different instance -> error.
	fake.addInstance("node-b")
	if _, err := d.Attach(context.Background(), opctx, handle, "node-b"); err == nil {
		t.Fatal("expected attach-to-different-instance error")
	}
	// Detach + idempotent.
	if err := d.Detach(context.Background(), opctx, handle, "node-a"); err != nil {
		t.Fatalf("Detach: %v", err)
	}
	if err := d.Detach(context.Background(), opctx, handle, "node-a"); err != nil {
		t.Fatalf("re-Detach: %v", err)
	}
}

func TestAttach_UsesNodeHostnameOverNodeID(t *testing.T) {
	fake := newFakeGCE()
	fake.addInstance("rune-prod")
	d, _ := newTestDriver(fake)
	opctx := euw2OpCtx(mkVolume("data"))
	opctx.NodeHostname = "rune-prod"
	handle, err := d.Provision(context.Background(), opctx, driver.ProvisionRequest{SizeBytes: 10 << 30})
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	if _, err := d.Attach(context.Background(), opctx, handle, "node-abc123"); err != nil {
		t.Fatalf("Attach via hostname: %v", err)
	}
}

func TestAttach_UnknownInstance(t *testing.T) {
	fake := newFakeGCE()
	d, _ := newTestDriver(fake)
	opctx := euw2OpCtx(mkVolume("d"))
	handle, _ := d.Provision(context.Background(), opctx, driver.ProvisionRequest{SizeBytes: 10 << 30})
	_, err := d.Attach(context.Background(), opctx, handle, "ghost")
	if err == nil || !strings.Contains(err.Error(), "no GCE instance") {
		t.Fatalf("expected no-instance error, got %v", err)
	}
}

func TestMountUnmount(t *testing.T) {
	fake := newFakeGCE()
	fake.addInstance("node-a")
	d, mounter := newTestDriver(fake)
	opctx := euw2OpCtx(mkVolume("d"))
	handle, _ := d.Provision(context.Background(), opctx, driver.ProvisionRequest{SizeBytes: 10 << 30})
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
	fake := newFakeGCE()
	d, mounter := newTestDriver(fake)
	opctx := euw2OpCtx(mkVolume("d"))
	handle, _ := d.Provision(context.Background(), opctx, driver.ProvisionRequest{SizeBytes: 10 << 30})
	if _, err := d.Mount(context.Background(), opctx, driver.MountOpts{
		Handle: handle, Target: "/mnt/x", FsType: "ext4",
	}); err != nil {
		t.Fatalf("Mount: %v", err)
	}
	want := string(devicePath(string(handle)))
	if mounter.mounts["/mnt/x"] != want {
		t.Fatalf("mounted %q, want derived %q", mounter.mounts["/mnt/x"], want)
	}
}

func TestMount_ReadOnlySkipsFormat(t *testing.T) {
	fake := newFakeGCE()
	d, mounter := newTestDriver(fake)
	opctx := euw2OpCtx(mkVolume("d"))
	handle, _ := d.Provision(context.Background(), opctx, driver.ProvisionRequest{SizeBytes: 10 << 30})
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
	fake := newFakeGCE()
	d, _ := newTestDriver(fake)
	opctx := euw2OpCtx(mkVolume("d"))
	handle, _ := d.Provision(context.Background(), opctx, driver.ProvisionRequest{SizeBytes: 10 << 30})
	snap := &types.Snapshot{Name: "s1", Namespace: "default"}
	snapHandle, err := d.Snapshot(context.Background(), opctx, driver.SnapshotRequest{Handle: handle, Snapshot: snap})
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if snapHandle == "" {
		t.Fatal("empty snapshot handle")
	}
	restoreOpCtx := euw2OpCtx(mkVolume("restored"))
	restored, err := d.RestoreFromSnapshot(context.Background(), restoreOpCtx, driver.RestoreRequest{
		Source: snap, SourceHandle: snapHandle, SizeBytes: 10 << 30,
	})
	if err != nil {
		t.Fatalf("RestoreFromSnapshot: %v", err)
	}
	if restored == handle {
		t.Fatal("restored handle should differ from source")
	}
}

func TestDeleteSnapshot_Idempotent(t *testing.T) {
	fake := newFakeGCE()
	d, _ := newTestDriver(fake)
	opctx := driver.OpContext{Parameters: map[string]string{"project": "test"}}
	if err := d.DeleteSnapshot(context.Background(), opctx, "missing-snap"); err != nil {
		t.Fatalf("expected idempotent DeleteSnapshot, got %v", err)
	}
	if err := d.DeleteSnapshot(context.Background(), driver.OpContext{}, ""); err != nil {
		t.Fatalf("empty handle DeleteSnapshot: %v", err)
	}
}

func TestExpand(t *testing.T) {
	fake := newFakeGCE()
	fake.addInstance("node-a")
	d, _ := newTestDriver(fake)
	opctx := euw2OpCtx(mkVolume("d"))
	handle, _ := d.Provision(context.Background(), opctx, driver.ProvisionRequest{SizeBytes: 10 << 30})
	if err := d.Expand(context.Background(), opctx, handle, "20Gi"); err != nil {
		t.Fatalf("Expand: %v", err)
	}
	fake.mu.Lock()
	if fake.disks[string(handle)].SizeGB != 20 {
		fake.mu.Unlock()
		t.Fatalf("expected 20 GiB after expand, got %d", fake.disks[string(handle)].SizeGB)
	}
	fake.mu.Unlock()
	// Online expand while attached.
	if _, err := d.Attach(context.Background(), opctx, handle, "node-a"); err != nil {
		t.Fatalf("Attach: %v", err)
	}
	if err := d.Expand(context.Background(), opctx, handle, "30Gi"); err != nil {
		t.Fatalf("online Expand while attached: %v", err)
	}
	// Shrink / equal -> no-op.
	if err := d.Expand(context.Background(), opctx, handle, "10Gi"); err != nil {
		t.Fatalf("no-op Expand: %v", err)
	}
}

func TestSanitizeGCEName(t *testing.T) {
	cases := map[string]string{
		"rune-default-data":     "rune-default-data",
		"rune-Default/Data":     "rune-default-data",
		"rune-_-_-strip":        "rune-strip",
		strings.Repeat("a", 80): strings.Repeat("a", 63),
	}
	for in, want := range cases {
		if got := sanitizeGCEName(in, 63); got != want {
			t.Errorf("sanitizeGCEName(%q) = %q, want %q", in, got, want)
		}
	}
	// Leading digit gets a 'v' (GCE names must start with a letter).
	if got := sanitizeGCEName("123-x", 63); got != "v123-x" {
		t.Errorf("sanitizeGCEName(123-x) = %q, want v123-x", got)
	}
}

func TestBytesToGiB(t *testing.T) {
	cases := map[int64]int64{
		1:              10, // floor
		10 << 30:       10,
		20 << 30:       20,
		(10 << 30) + 1: 11, // ceil past 10 GiB
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
}

func TestParseQuantity(t *testing.T) {
	cases := map[string]int64{"5Gi": 5 * (1 << 30), "5G": 5_000_000_000, "500": 500, "1Ti": 1 << 40}
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

func TestDevicePathAndSelfLink(t *testing.T) {
	if got := string(devicePath("rune-default-data")); got != "/dev/disk/by-id/google-rune-default-data" {
		t.Fatalf("devicePath = %q", got)
	}
	if got := instanceNameFromSelfLink(selfPrefix + "/instances/node-a"); got != "node-a" {
		t.Fatalf("instanceNameFromSelfLink = %q, want node-a", got)
	}
}

// --- production-client glue (bypassed by the fake interface; tested here
//     directly because it's the GCE-specific, error-prone part) ---

func TestIsGCENotFound(t *testing.T) {
	if !isGCENotFound(errNotFound) {
		t.Fatal("errNotFound should be not-found")
	}
	if !isGCENotFound(fmt.Errorf("wrapped: %w", &googleapi.Error{Code: 404})) {
		t.Fatal("googleapi 404 should be not-found")
	}
	if isGCENotFound(&googleapi.Error{Code: 403}) {
		t.Fatal("403 should NOT be not-found")
	}
	if isGCENotFound(nil) || isGCENotFound(errors.New("boom")) {
		t.Fatal("nil / generic errors should not be not-found")
	}
}

func TestOpError(t *testing.T) {
	// No error on the operation -> nil.
	if err := opError(&compute.Operation{Name: "op-1"}); err != nil {
		t.Fatalf("expected nil for clean op, got %v", err)
	}
	if err := opError(&compute.Operation{Name: "op-2", Error: &compute.OperationError{}}); err != nil {
		t.Fatalf("expected nil for empty error list, got %v", err)
	}
	// Populated error list -> formatted error carrying the codes/messages.
	err := opError(&compute.Operation{
		Name: "op-3",
		Error: &compute.OperationError{Errors: []*compute.OperationErrorErrors{
			{Code: "RESOURCE_NOT_READY", Message: "disk busy"},
			{Code: "QUOTA_EXCEEDED", Message: "no quota"},
		}},
	})
	if err == nil {
		t.Fatal("expected error for failed op")
	}
	for _, want := range []string{"op-3", "RESOURCE_NOT_READY", "disk busy", "QUOTA_EXCEEDED"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("opError message %q missing %q", err.Error(), want)
		}
	}
}

func TestGCELabelValue(t *testing.T) {
	cases := map[string]string{
		"default":       "default",
		"My/Name.Space": "my-name-space",
		"":              "none",
	}
	for in, want := range cases {
		if got := gceLabelValue(in); got != want {
			t.Errorf("gceLabelValue(%q) = %q, want %q", in, got, want)
		}
	}
	if got := gceLabelValue(strings.Repeat("a", 80)); len(got) != 63 {
		t.Errorf("gceLabelValue truncation = %d chars, want 63", len(got))
	}
}

func TestProvision_InsertError(t *testing.T) {
	fake := newFakeGCE()
	fake.insertErr = errors.New("quota exceeded")
	d, _ := newTestDriver(fake)
	_, err := d.Provision(context.Background(), euw2OpCtx(mkVolume("data")), driver.ProvisionRequest{SizeBytes: 10 << 30})
	if err == nil || !strings.Contains(err.Error(), "quota exceeded") {
		t.Fatalf("expected insert error to surface, got %v", err)
	}
}
