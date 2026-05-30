package hcloudvolume

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/runestack/rune/pkg/storage/driver"
	"github.com/runestack/rune/pkg/types"
)

// ============================================================================
// fake Hetzner Cloud API server
// ============================================================================

// fakeHC is a test double for the Hetzner Cloud API. It implements just
// enough of the surface hcloudvolume uses to drive every Driver method
// through a httptest.Server, mirroring dovolume's fakeDO.
type fakeHC struct {
	t *testing.T

	mu        sync.Mutex
	volumes   map[int64]*hcVolume
	servers   map[string]int64 // name -> server id
	actions   map[int64]*hcAction
	nextID    int64
	tokenSeen []string
	actionErr bool // when set, getAction reports error
}

func newFakeHC(t *testing.T) *fakeHC {
	return &fakeHC{
		t:       t,
		volumes: map[int64]*hcVolume{},
		servers: map[string]int64{},
		actions: map[int64]*hcAction{},
	}
}

func (f *fakeHC) addServer(name string, id int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.servers[name] = id
}

func (f *fakeHC) server() *httptest.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("/", f.handle)
	return httptest.NewServer(mux)
}

func (f *fakeHC) handle(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	r.Body.Close()
	f.mu.Lock()
	if auth := r.Header.Get("Authorization"); auth != "" {
		f.tokenSeen = append(f.tokenSeen, strings.TrimPrefix(auth, "Bearer "))
	}
	f.mu.Unlock()

	p := r.URL.Path
	switch {
	case r.Method == "POST" && p == "/v1/volumes":
		f.createVolume(w, body)
	case r.Method == "GET" && strings.HasPrefix(p, "/v1/volumes/"):
		f.getVolume(w, strings.TrimPrefix(p, "/v1/volumes/"))
	case r.Method == "DELETE" && strings.HasPrefix(p, "/v1/volumes/"):
		f.deleteVolume(w, strings.TrimPrefix(p, "/v1/volumes/"))
	case r.Method == "POST" && strings.HasPrefix(p, "/v1/volumes/") && strings.Contains(p, "/actions/"):
		f.volumeAction(w, p, body)
	case r.Method == "GET" && strings.HasPrefix(p, "/v1/actions/"):
		f.getAction(w, strings.TrimPrefix(p, "/v1/actions/"))
	case r.Method == "GET" && p == "/v1/servers":
		f.listServers(w, r.URL.Query().Get("name"))
	default:
		http.Error(w, "fakeHC: not found "+r.Method+" "+p, http.StatusNotFound)
	}
}

func (f *fakeHC) newAction(command string) *hcAction {
	id := atomic.AddInt64(&f.nextID, 1)
	act := &hcAction{ID: id, Status: "running", Command: command}
	f.actions[id] = act
	return act
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	b, _ := json.Marshal(v)
	_, _ = w.Write(b)
}

func actionWire(a *hcAction) map[string]any {
	return map[string]any{"id": a.ID, "status": a.Status, "command": a.Command}
}

func (f *fakeHC) createVolume(w http.ResponseWriter, body []byte) {
	var in map[string]any
	_ = json.Unmarshal(body, &in)
	f.mu.Lock()
	defer f.mu.Unlock()
	id := atomic.AddInt64(&f.nextID, 1)
	size := int64(0)
	if s, ok := in["size"].(float64); ok {
		size = int64(s)
	}
	name, _ := in["name"].(string)
	loc, _ := in["location"].(string)
	v := &hcVolume{ID: id, Name: name, Size: size, Location: hcLoc{Name: loc}}
	f.volumes[id] = v
	act := f.newAction("create_volume")
	writeJSON(w, http.StatusCreated, map[string]any{"volume": v, "action": actionWire(act)})
}

func (f *fakeHC) getVolume(w http.ResponseWriter, idStr string) {
	id, _ := strconv.ParseInt(idStr, 10, 64)
	f.mu.Lock()
	defer f.mu.Unlock()
	v, ok := f.volumes[id]
	if !ok {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"volume": v})
}

func (f *fakeHC) deleteVolume(w http.ResponseWriter, idStr string) {
	id, _ := strconv.ParseInt(idStr, 10, 64)
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.volumes[id]; !ok {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	delete(f.volumes, id)
	w.WriteHeader(http.StatusNoContent)
}

func (f *fakeHC) volumeAction(w http.ResponseWriter, path string, body []byte) {
	// path: /v1/volumes/{id}/actions/{action}
	rest := strings.TrimPrefix(path, "/v1/volumes/")
	parts := strings.Split(rest, "/")
	if len(parts) != 3 || parts[1] != "actions" {
		http.Error(w, "bad action path", http.StatusBadRequest)
		return
	}
	id, _ := strconv.ParseInt(parts[0], 10, 64)
	action := parts[2]
	var in map[string]any
	_ = json.Unmarshal(body, &in)
	f.mu.Lock()
	defer f.mu.Unlock()
	v, ok := f.volumes[id]
	if !ok {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	switch action {
	case "attach":
		sid := int64(0)
		if s, ok := in["server"].(float64); ok {
			sid = int64(s)
		}
		v.Server = &sid
	case "detach":
		v.Server = nil
	case "resize":
		if s, ok := in["size"].(float64); ok {
			v.Size = int64(s)
		}
	default:
		http.Error(w, "unknown action", http.StatusBadRequest)
		return
	}
	act := f.newAction(action + "_volume")
	writeJSON(w, http.StatusCreated, map[string]any{"action": actionWire(act)})
}

func (f *fakeHC) getAction(w http.ResponseWriter, idStr string) {
	id, _ := strconv.ParseInt(idStr, 10, 64)
	f.mu.Lock()
	defer f.mu.Unlock()
	act, ok := f.actions[id]
	if !ok {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	wire := map[string]any{"id": act.ID, "command": act.Command}
	if f.actionErr {
		wire["status"] = "error"
		wire["error"] = map[string]string{"code": "action_failed", "message": "boom"}
	} else {
		wire["status"] = "success"
	}
	writeJSON(w, http.StatusOK, map[string]any{"action": wire})
}

func (f *fakeHC) listServers(w http.ResponseWriter, name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	type s struct {
		ID   int64  `json:"id"`
		Name string `json:"name"`
	}
	var out []s
	for n, id := range f.servers {
		if name == "" || n == name {
			out = append(out, s{ID: id, Name: n})
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"servers": out})
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

func newTestDriver(t *testing.T, ts *httptest.Server) (*hcloudVolumeDriver, *fakeMounter) {
	t.Helper()
	cfg, err := parseConfig(map[string]any{"apiBaseURL": ts.URL})
	if err != nil {
		t.Fatalf("parseConfig: %v", err)
	}
	hc := newHTTPClient(cfg)
	hc.pollInterval = time.Millisecond
	m := newFakeMounter()
	return &hcloudVolumeDriver{cfg: cfg, client: hc, mounts: m}, m
}

func mkVolume(name string) *types.Volume {
	return &types.Volume{Name: name, Namespace: "default", Size: "10Gi", AccessMode: types.AccessModeRWO}
}

// nbg1OpCtx builds an OpContext with the volume + location/apiToken params.
func nbg1OpCtx(vol *types.Volume) driver.OpContext {
	return driver.OpContext{
		Volume:     vol,
		Parameters: map[string]string{"location": "nbg1", "apiToken": "test-token"},
	}
}

// ============================================================================
// tests
// ============================================================================

func TestFactoryRegistration(t *testing.T) {
	d, err := driver.New(DriverName, map[string]any{})
	if err != nil {
		t.Fatalf("driver.New(hcloud-volume): %v", err)
	}
	if d.Name() != DriverName {
		t.Fatalf("Name = %q; want %q", d.Name(), DriverName)
	}
	caps := d.Capabilities()
	if caps.Snapshots || !caps.Expand || caps.OnlineExpand || !caps.BlockDevice {
		t.Fatalf("unexpected caps: %+v", caps)
	}
	if len(caps.AccessModes) != 1 || caps.AccessModes[0] != types.AccessModeRWO {
		t.Fatalf("expected only RWO, got %v", caps.AccessModes)
	}
}

func TestParseConfigErrors(t *testing.T) {
	for _, raw := range []map[string]any{
		{"apiBaseURL": 42},
		{"volumeNamePrefix": 42},
	} {
		if _, err := parseConfig(raw); err == nil {
			t.Fatalf("expected parseConfig error for %v", raw)
		}
	}
}

func TestTokenRequired(t *testing.T) {
	fake := newFakeHC(t)
	ts := fake.server()
	defer ts.Close()
	d, _ := newTestDriver(t, ts)
	_, err := d.Provision(context.Background(), driver.OpContext{
		Volume:     mkVolume("data"),
		Parameters: map[string]string{"location": "nbg1"},
	}, driver.ProvisionRequest{SizeBytes: 10 * 1_000_000_000})
	if err == nil || !strings.Contains(err.Error(), "parameters.apiToken is required") {
		t.Fatalf("expected apiToken-required error, got %v", err)
	}
}

func TestProvision_HappyPath(t *testing.T) {
	fake := newFakeHC(t)
	ts := fake.server()
	defer ts.Close()
	d, _ := newTestDriver(t, ts)
	vol := mkVolume("data")
	handle, err := d.Provision(context.Background(), nbg1OpCtx(vol), driver.ProvisionRequest{SizeBytes: 20 * 1_000_000_000})
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
	for _, v := range fake.volumes {
		if v.Location.Name != "nbg1" || v.Size != 20 {
			t.Fatalf("unexpected volume: %+v", v)
		}
		if !strings.HasPrefix(v.Name, "rune-default-data") {
			t.Fatalf("unexpected volume name %q", v.Name)
		}
	}
}

func TestProvision_LocationRequiredAndRegionAlias(t *testing.T) {
	fake := newFakeHC(t)
	ts := fake.server()
	defer ts.Close()
	d, _ := newTestDriver(t, ts)
	// Missing location -> error.
	_, err := d.Provision(context.Background(), driver.OpContext{
		Volume:     mkVolume("d"),
		Parameters: map[string]string{"apiToken": "t"},
	}, driver.ProvisionRequest{SizeBytes: 1 << 30})
	if err == nil || !strings.Contains(err.Error(), "location is required") {
		t.Fatalf("expected location-required error, got %v", err)
	}
	// `region` alias is accepted in place of `location`.
	if _, err := d.Provision(context.Background(), driver.OpContext{
		Volume:     mkVolume("d"),
		Parameters: map[string]string{"apiToken": "t", "region": "fsn1"},
	}, driver.ProvisionRequest{SizeBytes: 10 * 1_000_000_000}); err != nil {
		t.Fatalf("region alias should work: %v", err)
	}
}

func TestProvision_MinSize(t *testing.T) {
	fake := newFakeHC(t)
	ts := fake.server()
	defer ts.Close()
	d, _ := newTestDriver(t, ts)
	// Ask for 1 GB; Hetzner floor is 10.
	handle, err := d.Provision(context.Background(), nbg1OpCtx(mkVolume("d")), driver.ProvisionRequest{SizeBytes: 1_000_000_000})
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	id, _ := parseHandle(handle)
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.volumes[id].Size != 10 {
		t.Fatalf("expected 10 GB floor, got %d", fake.volumes[id].Size)
	}
}

func TestProvision_AccessModeUnsupported(t *testing.T) {
	fake := newFakeHC(t)
	ts := fake.server()
	defer ts.Close()
	d, _ := newTestDriver(t, ts)
	vol := mkVolume("d")
	vol.AccessMode = types.AccessModeRWX
	_, err := d.Provision(context.Background(), nbg1OpCtx(vol), driver.ProvisionRequest{SizeBytes: 10 * 1_000_000_000})
	if err == nil || !strings.Contains(err.Error(), "access mode") {
		t.Fatalf("expected access-mode error, got %v", err)
	}
}

func TestDelete_Idempotent(t *testing.T) {
	fake := newFakeHC(t)
	ts := fake.server()
	defer ts.Close()
	d, _ := newTestDriver(t, ts)
	opctx := driver.OpContext{Parameters: map[string]string{"apiToken": "t"}}
	if err := d.Delete(context.Background(), opctx, "9999"); err != nil {
		t.Fatalf("expected idempotent Delete, got %v", err)
	}
	if err := d.Delete(context.Background(), driver.OpContext{}, ""); err != nil {
		t.Fatalf("empty handle Delete: %v", err)
	}
}

func TestAttachDetach(t *testing.T) {
	fake := newFakeHC(t)
	fake.addServer("node-a", 9001)
	ts := fake.server()
	defer ts.Close()
	d, _ := newTestDriver(t, ts)
	opctx := nbg1OpCtx(mkVolume("data"))
	handle, err := d.Provision(context.Background(), opctx, driver.ProvisionRequest{SizeBytes: 10 * 1_000_000_000})
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	dev, err := d.Attach(context.Background(), opctx, handle, "node-a")
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}
	if !strings.HasPrefix(string(dev), "/dev/disk/by-id/scsi-0HC_Volume_") {
		t.Fatalf("unexpected device path %q", dev)
	}
	// Idempotent re-attach to same server.
	if _, err := d.Attach(context.Background(), opctx, handle, "node-a"); err != nil {
		t.Fatalf("re-Attach: %v", err)
	}
	// Attach to a different server -> error.
	fake.addServer("node-b", 9002)
	if _, err := d.Attach(context.Background(), opctx, handle, "node-b"); err == nil {
		t.Fatal("expected attach-to-different-server error")
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
	fake := newFakeHC(t)
	fake.addServer("rune-edge-nbg1", 570378027)
	ts := fake.server()
	defer ts.Close()
	d, _ := newTestDriver(t, ts)
	opctx := nbg1OpCtx(mkVolume("data"))
	opctx.NodeHostname = "rune-edge-nbg1"
	handle, err := d.Provision(context.Background(), opctx, driver.ProvisionRequest{SizeBytes: 10 * 1_000_000_000})
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	if _, err := d.Attach(context.Background(), opctx, handle, "node-5d7a0ab4"); err != nil {
		t.Fatalf("Attach via hostname: %v", err)
	}
}

func TestAttach_UnknownServer(t *testing.T) {
	fake := newFakeHC(t)
	ts := fake.server()
	defer ts.Close()
	d, _ := newTestDriver(t, ts)
	opctx := nbg1OpCtx(mkVolume("d"))
	handle, _ := d.Provision(context.Background(), opctx, driver.ProvisionRequest{SizeBytes: 10 * 1_000_000_000})
	_, err := d.Attach(context.Background(), opctx, handle, "ghost")
	if err == nil || !strings.Contains(err.Error(), "no Hetzner server") {
		t.Fatalf("expected no-server error, got %v", err)
	}
}

func TestMountUnmount(t *testing.T) {
	fake := newFakeHC(t)
	fake.addServer("node-a", 1)
	ts := fake.server()
	defer ts.Close()
	d, mounter := newTestDriver(t, ts)
	opctx := nbg1OpCtx(mkVolume("d"))
	handle, _ := d.Provision(context.Background(), opctx, driver.ProvisionRequest{SizeBytes: 10 * 1_000_000_000})
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

func TestMount_ReadOnlySkipsFormat(t *testing.T) {
	fake := newFakeHC(t)
	ts := fake.server()
	defer ts.Close()
	d, mounter := newTestDriver(t, ts)
	opctx := nbg1OpCtx(mkVolume("d"))
	handle, _ := d.Provision(context.Background(), opctx, driver.ProvisionRequest{SizeBytes: 10 * 1_000_000_000})
	if _, err := d.Mount(context.Background(), opctx, driver.MountOpts{
		Handle: handle, Device: "/dev/x", Target: "/mnt/ro", ReadOnly: true,
	}); err != nil {
		t.Fatalf("Mount: %v", err)
	}
	if len(mounter.formatted) != 0 {
		t.Fatalf("expected no format on read-only mount, got %+v", mounter.formatted)
	}
}

func TestSnapshotUnsupported(t *testing.T) {
	fake := newFakeHC(t)
	ts := fake.server()
	defer ts.Close()
	d, _ := newTestDriver(t, ts)
	opctx := nbg1OpCtx(mkVolume("d"))
	if _, err := d.Snapshot(context.Background(), opctx, driver.SnapshotRequest{Handle: "1"}); err != driver.ErrUnsupported {
		t.Fatalf("Snapshot: expected ErrUnsupported, got %v", err)
	}
	if _, err := d.RestoreFromSnapshot(context.Background(), opctx, driver.RestoreRequest{}); err != driver.ErrUnsupported {
		t.Fatalf("Restore: expected ErrUnsupported, got %v", err)
	}
	if err := d.DeleteSnapshot(context.Background(), opctx, "1"); err != driver.ErrUnsupported {
		t.Fatalf("DeleteSnapshot: expected ErrUnsupported, got %v", err)
	}
}

func TestExpand(t *testing.T) {
	fake := newFakeHC(t)
	fake.addServer("node-a", 1)
	ts := fake.server()
	defer ts.Close()
	d, _ := newTestDriver(t, ts)
	opctx := nbg1OpCtx(mkVolume("d"))
	handle, _ := d.Provision(context.Background(), opctx, driver.ProvisionRequest{SizeBytes: 10 * 1_000_000_000})
	// Detached -> expand succeeds.
	if err := d.Expand(context.Background(), opctx, handle, "20G"); err != nil {
		t.Fatalf("Expand detached: %v", err)
	}
	id, _ := parseHandle(handle)
	fake.mu.Lock()
	if fake.volumes[id].Size != 20 {
		fake.mu.Unlock()
		t.Fatalf("expected 20 GB after expand, got %d", fake.volumes[id].Size)
	}
	fake.mu.Unlock()
	// Attached -> ErrOnlineExpandUnsupported.
	if _, err := d.Attach(context.Background(), opctx, handle, "node-a"); err != nil {
		t.Fatalf("Attach: %v", err)
	}
	if err := d.Expand(context.Background(), opctx, handle, "30G"); err != driver.ErrOnlineExpandUnsupported {
		t.Fatalf("expected ErrOnlineExpandUnsupported when attached, got %v", err)
	}
}

func TestActionErrored(t *testing.T) {
	fake := newFakeHC(t)
	fake.addServer("node-a", 1)
	fake.actionErr = true
	ts := fake.server()
	defer ts.Close()
	d, _ := newTestDriver(t, ts)
	opctx := nbg1OpCtx(mkVolume("d"))
	// Provision waits on the create action, which now errors.
	_, err := d.Provision(context.Background(), opctx, driver.ProvisionRequest{SizeBytes: 10 * 1_000_000_000})
	if err == nil || !strings.Contains(err.Error(), "errored") {
		t.Fatalf("expected errored action failure, got %v", err)
	}
}

func TestBearerTokenSent(t *testing.T) {
	fake := newFakeHC(t)
	ts := fake.server()
	defer ts.Close()
	d, _ := newTestDriver(t, ts)
	if _, err := d.Provision(context.Background(), nbg1OpCtx(mkVolume("d")), driver.ProvisionRequest{SizeBytes: 10 * 1_000_000_000}); err != nil {
		t.Fatalf("Provision: %v", err)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.tokenSeen) == 0 {
		t.Fatal("no Authorization header observed")
	}
	for _, tok := range fake.tokenSeen {
		if tok != "test-token" {
			t.Fatalf("got token %q, want test-token", tok)
		}
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

func TestBytesToGigabytes(t *testing.T) {
	cases := map[int64]int64{1: 1, 1_000_000_000: 1, 1_500_000_000: 2, 10_000_000_000: 10}
	for in, want := range cases {
		got, err := bytesToGigabytes(in)
		if err != nil || got != want {
			t.Errorf("bytesToGigabytes(%d) = %d, %v; want %d", in, got, err, want)
		}
	}
	if _, err := bytesToGigabytes(0); err == nil {
		t.Fatal("expected error for 0 bytes")
	}
}

func TestSanitizeHCName(t *testing.T) {
	cases := map[string]string{
		"rune-default-data":     "rune-default-data",
		"rune-Default/Data":     "rune-default-data",
		"rune-_-_-strip":        "rune-strip",
		strings.Repeat("a", 80): strings.Repeat("a", 64),
	}
	for in, want := range cases {
		if got := sanitizeHCName(in, 64); got != want {
			t.Errorf("sanitizeHCName(%q) = %q, want %q", in, got, want)
		}
	}
	// An all-symbol name reduces to empty and gets a 'v' to satisfy
	// Hetzner's "must start with alphanumeric" rule.
	if got := sanitizeHCName("___", 64); got != "v" {
		t.Errorf("sanitizeHCName(___) = %q, want v", got)
	}
	// A leading hyphen is simply dropped (not replaced) since the
	// builder never emits a leading separator.
	if got := sanitizeHCName("-foo", 64); got != "foo" {
		t.Errorf("sanitizeHCName(-foo) = %q, want foo", got)
	}
}

func TestParseHandle(t *testing.T) {
	if _, err := parseHandle(""); err == nil {
		t.Fatal("empty handle should error")
	}
	if _, err := parseHandle("not-a-number"); err == nil {
		t.Fatal("non-numeric handle should error")
	}
	id, err := parseHandle(driver.VolumeHandle("12345"))
	if err != nil || id != 12345 {
		t.Fatalf("parseHandle(12345) = %d, %v", id, err)
	}
}
