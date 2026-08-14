package dovolume

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/runestack/rune/pkg/storage/driver"
	"github.com/runestack/rune/pkg/types"
)

// ============================================================================
// fake DO API server
// ============================================================================

// fakeDO is a test double for the DigitalOcean API. It implements just
// enough of the surface area dovolume uses to drive every Driver method
// through a httptest.Server.
type fakeDO struct {
	t *testing.T

	mu        sync.Mutex
	volumes   map[string]*doVolume        // id -> volume
	snapshots map[string]*doSnapshot      // id -> snapshot
	droplets  map[string]int64            // name -> droplet id
	actions   map[int64]*doAction         // id -> action
	requests  []recordedRequest           // observable history
	nextID    int64                       // monotonic action id source
	hooks     map[string]http.HandlerFunc // path-prefix overrides for error injection
	tokenSeen []string                    // bearer tokens observed (for auth tests)
	actionErr bool                        // when set, all actions report errored after first poll
}

type recordedRequest struct {
	method string
	path   string
	body   string
}

func newFakeDO(t *testing.T) *fakeDO {
	return &fakeDO{
		t:         t,
		volumes:   map[string]*doVolume{},
		snapshots: map[string]*doSnapshot{},
		droplets:  map[string]int64{},
		actions:   map[int64]*doAction{},
		hooks:     map[string]http.HandlerFunc{},
	}
}

func (f *fakeDO) addDroplet(name string, id int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.droplets[name] = id
}

// server returns a started httptest.Server whose URL should be plugged
// into Config.APIBaseURL. Caller closes it.
func (f *fakeDO) server() *httptest.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("/", f.handle)
	return httptest.NewServer(mux)
}

func (f *fakeDO) handle(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	r.Body.Close()
	f.mu.Lock()
	f.requests = append(f.requests, recordedRequest{method: r.Method, path: r.URL.RequestURI(), body: string(body)})
	if auth := r.Header.Get("Authorization"); auth != "" {
		f.tokenSeen = append(f.tokenSeen, strings.TrimPrefix(auth, "Bearer "))
	}
	for prefix, h := range f.hooks {
		if strings.HasPrefix(r.URL.Path, prefix) {
			f.mu.Unlock()
			h(w, r)
			return
		}
	}
	f.mu.Unlock()

	switch {
	case r.Method == "POST" && r.URL.Path == "/v2/volumes":
		f.handleCreateVolume(w, body)
	case r.Method == "GET" && r.URL.Path == "/v2/volumes":
		f.handleListVolumes(w, r.URL.Query().Get("name"), r.URL.Query().Get("region"))
	case r.Method == "GET" && strings.HasPrefix(r.URL.Path, "/v2/volumes/") && !strings.Contains(r.URL.Path[len("/v2/volumes/"):], "/"):
		id := strings.TrimPrefix(r.URL.Path, "/v2/volumes/")
		f.handleGetVolume(w, id)
	case r.Method == "DELETE" && strings.HasPrefix(r.URL.Path, "/v2/volumes/") && !strings.Contains(r.URL.Path[len("/v2/volumes/"):], "/"):
		id := strings.TrimPrefix(r.URL.Path, "/v2/volumes/")
		f.handleDeleteVolume(w, id)
	case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/actions") && strings.HasPrefix(r.URL.Path, "/v2/volumes/"):
		id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v2/volumes/"), "/actions")
		f.handleVolumeAction(w, id, body)
	case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/snapshots") && strings.HasPrefix(r.URL.Path, "/v2/volumes/"):
		id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v2/volumes/"), "/snapshots")
		f.handleCreateSnapshot(w, id, body)
	case r.Method == "GET" && strings.HasPrefix(r.URL.Path, "/v2/actions/"):
		f.handleGetAction(w, strings.TrimPrefix(r.URL.Path, "/v2/actions/"))
	case r.Method == "GET" && r.URL.Path == "/v2/droplets":
		f.handleListDroplets(w, r.URL.Query().Get("name"))
	default:
		http.Error(w, "fakeDO: not found "+r.Method+" "+r.URL.Path, http.StatusNotFound)
	}
}

func (f *fakeDO) handleCreateVolume(w http.ResponseWriter, body []byte) {
	var in map[string]any
	_ = json.Unmarshal(body, &in)
	f.mu.Lock()
	defer f.mu.Unlock()
	id := fmt.Sprintf("vol-%d", atomic.AddInt64(&f.nextID, 1))
	region := ""
	if r, ok := in["region"].(string); ok {
		region = r
	}
	size := int64(0)
	if s, ok := in["size_gigabytes"].(float64); ok {
		size = int64(s)
	}
	name := ""
	if n, ok := in["name"].(string); ok {
		name = n
	}
	// DO rejects a duplicate (name, region) with HTTP 409.
	for _, ex := range f.volumes {
		if ex.Name == name && ex.Region.Slug == region {
			http.Error(w, `{"id":"conflict","message":"volume with this name already exists"}`, http.StatusConflict)
			return
		}
	}
	v := &doVolume{ID: id, Name: name, SizeGigabytes: size, Region: doSlug{Slug: region}}
	f.volumes[id] = v
	resp, _ := json.Marshal(map[string]any{"volume": v})
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_, _ = w.Write(resp)
}

// handleListVolumes serves GET /v2/volumes?name=&region= — the lookup
// Provision uses to adopt an existing volume after a 409.
func (f *fakeDO) handleListVolumes(w http.ResponseWriter, name, region string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	matches := make([]*doVolume, 0)
	for _, v := range f.volumes {
		if name != "" && v.Name != name {
			continue
		}
		if region != "" && v.Region.Slug != region {
			continue
		}
		matches = append(matches, v)
	}
	resp, _ := json.Marshal(map[string]any{"volumes": matches})
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(resp)
}

func (f *fakeDO) handleGetVolume(w http.ResponseWriter, id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	v, ok := f.volumes[id]
	if !ok {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	resp, _ := json.Marshal(map[string]any{"volume": v})
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(resp)
}

func (f *fakeDO) handleDeleteVolume(w http.ResponseWriter, id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.volumes[id]; !ok {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	delete(f.volumes, id)
	w.WriteHeader(http.StatusNoContent)
}

func (f *fakeDO) handleVolumeAction(w http.ResponseWriter, id string, body []byte) {
	var in map[string]any
	_ = json.Unmarshal(body, &in)
	f.mu.Lock()
	defer f.mu.Unlock()
	v, ok := f.volumes[id]
	if !ok {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	t, _ := in["type"].(string)
	switch t {
	case "attach":
		dropID, _ := in["droplet_id"].(float64)
		v.DropletIDs = append(v.DropletIDs, int64(dropID))
	case "detach":
		dropID, _ := in["droplet_id"].(float64)
		out := v.DropletIDs[:0]
		for _, d := range v.DropletIDs {
			if d != int64(dropID) {
				out = append(out, d)
			}
		}
		v.DropletIDs = out
	case "resize":
		size, _ := in["size_gigabytes"].(float64)
		v.SizeGigabytes = int64(size)
	default:
		http.Error(w, "unknown action type", http.StatusBadRequest)
		return
	}
	aid := atomic.AddInt64(&f.nextID, 1)
	status := "completed"
	if f.actionErr {
		status = "in-progress"
	}
	act := &doAction{ID: aid, Status: status, Type: t}
	f.actions[aid] = act
	resp, _ := json.Marshal(map[string]any{"action": act})
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(resp)
}

func (f *fakeDO) handleCreateSnapshot(w http.ResponseWriter, volID string, body []byte) {
	var in map[string]any
	_ = json.Unmarshal(body, &in)
	name, _ := in["name"].(string)
	f.mu.Lock()
	defer f.mu.Unlock()
	v, ok := f.volumes[volID]
	if !ok {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	id := fmt.Sprintf("snap-%d", atomic.AddInt64(&f.nextID, 1))
	snap := &doSnapshot{ID: id, Name: name, SizeGigabytes: v.SizeGigabytes}
	f.snapshots[id] = snap
	resp, _ := json.Marshal(map[string]any{"snapshot": snap})
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_, _ = w.Write(resp)
}

func (f *fakeDO) handleGetAction(w http.ResponseWriter, idStr string) {
	var id int64
	_, _ = fmt.Sscanf(idStr, "%d", &id)
	f.mu.Lock()
	defer f.mu.Unlock()
	act, ok := f.actions[id]
	if !ok {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if f.actionErr {
		// Flip the cached action to errored so subsequent polls see it.
		act.Status = "errored"
	}
	resp, _ := json.Marshal(map[string]any{"action": act})
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(resp)
}

func (f *fakeDO) handleListDroplets(w http.ResponseWriter, name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	type d struct {
		ID   int64  `json:"id"`
		Name string `json:"name"`
	}
	var out []d
	for n, id := range f.droplets {
		if name == "" || n == name {
			out = append(out, d{ID: id, Name: n})
		}
	}
	resp, _ := json.Marshal(map[string]any{"droplets": out})
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(resp)
}

// ============================================================================
// fake mount executor
// ============================================================================

type fakeMounter struct {
	mu        sync.Mutex
	formatted map[string]string // dev -> fsType
	mounts    map[string]string // target -> dev
	dirs      map[string]bool   // target -> created
	failOn    string            // method name to fail (one-shot)
	failErr   error
}

func newFakeMounter() *fakeMounter {
	return &fakeMounter{
		formatted: map[string]string{},
		mounts:    map[string]string{},
		dirs:      map[string]bool{},
	}
}

func (f *fakeMounter) maybeFail(method string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failOn == method {
		f.failOn = ""
		return f.failErr
	}
	return nil
}

func (f *fakeMounter) MkdirAll(target string, _ os.FileMode) error {
	if err := f.maybeFail("MkdirAll"); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.dirs[target] = true
	return nil
}
func (f *fakeMounter) EnsureFormatted(_ context.Context, dev, fsType string) error {
	if err := f.maybeFail("EnsureFormatted"); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.formatted[dev]; ok {
		return nil
	}
	f.formatted[dev] = fsType
	return nil
}
func (f *fakeMounter) Mount(_ context.Context, dev, target, _ string, _ bool) error {
	if err := f.maybeFail("Mount"); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.mounts[target] = dev
	return nil
}
func (f *fakeMounter) Unmount(_ context.Context, target string) error {
	if err := f.maybeFail("Unmount"); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.mounts, target)
	return nil
}

// ============================================================================
// helpers
// ============================================================================

func newTestDriver(t *testing.T, fake *fakeDO, ts *httptest.Server) (*doVolumeDriver, *fakeMounter) {
	t.Helper()
	cfg, err := parseConfig(map[string]any{
		"apiBaseURL": ts.URL,
	})
	if err != nil {
		t.Fatalf("parseConfig: %v", err)
	}
	mounter := newFakeMounter()
	hc := newHTTPClient(cfg)
	hc.pollInterval = time.Millisecond
	return &doVolumeDriver{cfg: cfg, client: hc, mounts: mounter}, mounter
}

func mkVolume(name string) *types.Volume {
	return &types.Volume{
		Name:       name,
		Namespace:  "default",
		Size:       "5Gi",
		AccessMode: types.AccessModeRWO,
	}
}

// ============================================================================
// tests
// ============================================================================

func TestFactoryRegistration(t *testing.T) {
	d, err := driver.New(DriverName, map[string]any{})
	if err != nil {
		t.Fatalf("driver.New(do-volume): %v", err)
	}
	if d.Name() != DriverName {
		t.Fatalf("Name = %q; want %q", d.Name(), DriverName)
	}
	caps := d.Capabilities()
	if !caps.BlockDevice || !caps.Snapshots || !caps.Expand || caps.OnlineExpand {
		t.Fatalf("unexpected caps: %+v", caps)
	}
	if len(caps.AccessModes) != 1 || caps.AccessModes[0] != types.AccessModeRWO {
		t.Fatalf("expected only RWO, got %v", caps.AccessModes)
	}
}

func TestParseConfigErrors(t *testing.T) {
	cases := []struct {
		name string
		raw  map[string]any
	}{
		{"apiBaseURL bad type", map[string]any{"apiBaseURL": 42}},
		{"volumeNamePrefix bad type", map[string]any{"volumeNamePrefix": 42}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := parseConfig(tc.raw); err == nil {
				t.Fatalf("expected parseConfig error for %s", tc.name)
			}
		})
	}
}

func TestTokenRequired(t *testing.T) {
	fake := newFakeDO(t)
	ts := fake.server()
	defer ts.Close()
	d, _ := newTestDriver(t, fake, ts)
	// Omit apiToken — driver must surface a clear error rather than
	// hitting DO with no bearer token.
	_, err := d.Provision(context.Background(), driver.OpContext{
		Volume:     mkVolume("data"),
		Parameters: map[string]string{"region": "nyc3"},
	}, driver.ProvisionRequest{SizeBytes: 1 << 30})
	if err == nil || !strings.Contains(err.Error(), "parameters.apiToken is required") {
		t.Fatalf("expected apiToken-required error, got %v", err)
	}
}

func TestProvision_HappyPath(t *testing.T) {
	fake := newFakeDO(t)
	ts := fake.server()
	defer ts.Close()
	d, _ := newTestDriver(t, fake, ts)
	vol := mkVolume("data")
	opctx := nyc3OpCtx(vol)
	handle, err := d.Provision(context.Background(), opctx, driver.ProvisionRequest{
		SizeBytes: 5 * 1_000_000_000,
	})
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	if handle == "" {
		t.Fatalf("empty handle")
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.volumes) != 1 {
		t.Fatalf("expected 1 volume, got %d", len(fake.volumes))
	}
	for _, v := range fake.volumes {
		if v.Region.Slug != "nyc3" || v.SizeGigabytes != 5 {
			t.Fatalf("unexpected volume: %+v", v)
		}
		if !strings.HasPrefix(v.Name, "rune-default-data") {
			t.Fatalf("unexpected DO volume name %q", v.Name)
		}
	}
}

// TestProvision_AdoptsExistingVolumeOnConflict covers re-casting a
// service whose DO volume still exists: createVolume returns HTTP 409
// and Provision must adopt the existing volume rather than fail.
func TestProvision_AdoptsExistingVolumeOnConflict(t *testing.T) {
	fake := newFakeDO(t)
	ts := fake.server()
	defer ts.Close()
	d, _ := newTestDriver(t, fake, ts)
	opctx := nyc3OpCtx(mkVolume("data"))
	req := driver.ProvisionRequest{SizeBytes: 5 * 1_000_000_000}

	first, err := d.Provision(context.Background(), opctx, req)
	if err != nil {
		t.Fatalf("first Provision: %v", err)
	}

	// Second Provision of the same volume: DO answers 409; the driver
	// must adopt the existing volume and return the same handle.
	second, err := d.Provision(context.Background(), opctx, req)
	if err != nil {
		t.Fatalf("second Provision (adopt): %v", err)
	}
	if second != first {
		t.Fatalf("adopt returned handle %q, want existing %q", second, first)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.volumes) != 1 {
		t.Fatalf("expected 1 DO volume after adopt, got %d", len(fake.volumes))
	}
}

func TestProvision_RegionRequired(t *testing.T) {
	fake := newFakeDO(t)
	ts := fake.server()
	defer ts.Close()
	d, _ := newTestDriver(t, fake, ts)
	// Empty Parameters map — region is missing.
	_, err := d.Provision(context.Background(), driver.OpContext{
		Volume:     mkVolume("data"),
		Parameters: map[string]string{},
	}, driver.ProvisionRequest{SizeBytes: 1 << 30})
	if err == nil || !strings.Contains(err.Error(), "region") {
		t.Fatalf("expected region-required error, got %v", err)
	}
}

func TestProvision_AccessModeUnsupported(t *testing.T) {
	fake := newFakeDO(t)
	ts := fake.server()
	defer ts.Close()
	d, _ := newTestDriver(t, fake, ts)
	vol := mkVolume("data")
	vol.AccessMode = types.AccessModeRWX
	_, err := d.Provision(context.Background(), nyc3OpCtx(vol), driver.ProvisionRequest{SizeBytes: 1 << 30})
	if !errors.Is(err, driver.ErrAccessModeUnsupported) {
		t.Fatalf("expected ErrAccessModeUnsupported, got %v", err)
	}
}

func TestDelete_Idempotent(t *testing.T) {
	fake := newFakeDO(t)
	ts := fake.server()
	defer ts.Close()
	d, _ := newTestDriver(t, fake, ts)
	// missing handle -> no error
	opctx := driver.OpContext{Parameters: map[string]string{"apiToken": "test-token"}}
	if err := d.Delete(context.Background(), opctx, "vol-does-not-exist"); err != nil {
		t.Fatalf("expected idempotent Delete, got %v", err)
	}
	if err := d.Delete(context.Background(), driver.OpContext{}, ""); err != nil {
		t.Fatalf("empty handle Delete: %v", err)
	}
}

func TestAttachDetach(t *testing.T) {
	fake := newFakeDO(t)
	fake.addDroplet("node-a", 9001)
	ts := fake.server()
	defer ts.Close()
	d, _ := newTestDriver(t, fake, ts)
	vol := mkVolume("data")
	opctx := nyc3OpCtx(vol)
	handle, err := d.Provision(context.Background(), opctx, driver.ProvisionRequest{SizeBytes: 2 * 1_000_000_000})
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	dev, err := d.Attach(context.Background(), opctx, handle, "node-a")
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}
	if !strings.HasPrefix(string(dev), "/dev/disk/by-id/scsi-0DO_Volume_rune-default-data") {
		t.Fatalf("unexpected device path %q", dev)
	}
	// Idempotent re-attach: same droplet -> no error, no new attach action.
	if _, err := d.Attach(context.Background(), opctx, handle, "node-a"); err != nil {
		t.Fatalf("re-Attach: %v", err)
	}
	// Attach to a different droplet -> error
	fake.addDroplet("node-b", 9002)
	if _, err := d.Attach(context.Background(), opctx, handle, "node-b"); err == nil {
		t.Fatalf("expected attach-to-different-droplet error")
	}
	// Detach
	if err := d.Detach(context.Background(), opctx, handle, "node-a"); err != nil {
		t.Fatalf("Detach: %v", err)
	}
	// Detach idempotent
	if err := d.Detach(context.Background(), opctx, handle, "node-a"); err != nil {
		t.Fatalf("re-Detach: %v", err)
	}
}

// When the agent threads OpContext.NodeHostname through, the driver
// must use it for the /v2/droplets?name=... lookup instead of the
// Rune node ID — DO has no droplet named "node-<rand>" but it does
// have one named after the host's hostname.
func TestAttach_UsesNodeHostnameOverNodeID(t *testing.T) {
	fake := newFakeDO(t)
	fake.addDroplet("rune-edge-lon1", 570378027)
	ts := fake.server()
	defer ts.Close()
	d, _ := newTestDriver(t, fake, ts)
	vol := mkVolume("data")
	opctx := nyc3OpCtx(vol)
	opctx.NodeHostname = "rune-edge-lon1"
	handle, err := d.Provision(context.Background(), opctx, driver.ProvisionRequest{SizeBytes: 1 << 30})
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	// Pass a Rune-style NodeID that does NOT match any droplet name;
	// the lookup must succeed via NodeHostname.
	if _, err := d.Attach(context.Background(), opctx, handle, "node-5d7a0ab4bfe76078"); err != nil {
		t.Fatalf("Attach: %v", err)
	}
}

func TestAttach_UnknownNode(t *testing.T) {
	fake := newFakeDO(t)
	ts := fake.server()
	defer ts.Close()
	d, _ := newTestDriver(t, fake, ts)
	vol := mkVolume("d")
	opctx := nyc3OpCtx(vol)
	handle, _ := d.Provision(context.Background(), opctx, driver.ProvisionRequest{SizeBytes: 1 << 30})
	_, err := d.Attach(context.Background(), opctx, handle, "ghost")
	if err == nil || !strings.Contains(err.Error(), "no DO droplet") {
		t.Fatalf("expected no-droplet error, got %v", err)
	}
}

func TestMountUnmount(t *testing.T) {
	fake := newFakeDO(t)
	fake.addDroplet("node-a", 1)
	ts := fake.server()
	defer ts.Close()
	d, mounter := newTestDriver(t, fake, ts)
	vol := mkVolume("d")
	opctx := nyc3OpCtx(vol)
	handle, _ := d.Provision(context.Background(), opctx, driver.ProvisionRequest{SizeBytes: 1 << 30})
	dev, _ := d.Attach(context.Background(), opctx, handle, "node-a")
	target := "/var/lib/rune/mounts/" + string(handle)
	got, err := d.Mount(context.Background(), opctx, driver.MountOpts{
		Handle: handle,
		Device: dev,
		Target: driver.MountTarget(target),
		FsType: "ext4",
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
	if mounter.mounts[target] != string(dev) {
		t.Fatalf("not mounted: %+v", mounter.mounts)
	}
	if err := d.Unmount(context.Background(), opctx, got); err != nil {
		t.Fatalf("Unmount: %v", err)
	}
	if _, ok := mounter.mounts[target]; ok {
		t.Fatalf("still mounted after Unmount")
	}
}

func TestMount_ReadOnlySkipsFormat(t *testing.T) {
	fake := newFakeDO(t)
	ts := fake.server()
	defer ts.Close()
	d, mounter := newTestDriver(t, fake, ts)
	vol := mkVolume("d")
	opctx := nyc3OpCtx(vol)
	handle, _ := d.Provision(context.Background(), opctx, driver.ProvisionRequest{SizeBytes: 1 << 30})
	_, err := d.Mount(context.Background(), opctx, driver.MountOpts{
		Handle:   handle,
		Device:   "/dev/sda",
		Target:   "/mnt/x",
		ReadOnly: true,
	})
	if err != nil {
		t.Fatalf("Mount: %v", err)
	}
	if len(mounter.formatted) != 0 {
		t.Fatalf("expected no format on read-only mount, got %+v", mounter.formatted)
	}
}

func TestSnapshotAndRestore(t *testing.T) {
	fake := newFakeDO(t)
	ts := fake.server()
	defer ts.Close()
	d, _ := newTestDriver(t, fake, ts)
	vol := mkVolume("d")
	opctx := nyc3OpCtx(vol)
	handle, _ := d.Provision(context.Background(), opctx, driver.ProvisionRequest{SizeBytes: 2 * 1_000_000_000})
	snap := &types.Snapshot{Name: "s1", Namespace: "default"}
	snapHandle, err := d.Snapshot(context.Background(), opctx, driver.SnapshotRequest{
		Handle: handle, Snapshot: snap,
	})
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if snapHandle == "" {
		t.Fatalf("empty snapshot handle")
	}
	target := mkVolume("restored")
	restoreOpCtx := nyc3OpCtx(target)
	restoredHandle, err := d.RestoreFromSnapshot(context.Background(), restoreOpCtx, driver.RestoreRequest{
		Source:       snap,
		SourceHandle: snapHandle,
		SizeBytes:    2 * 1_000_000_000,
	})
	if err != nil {
		t.Fatalf("RestoreFromSnapshot: %v", err)
	}
	if restoredHandle == handle {
		t.Fatalf("restored handle should differ from source handle")
	}
}

func TestExpand(t *testing.T) {
	fake := newFakeDO(t)
	fake.addDroplet("node-a", 1)
	ts := fake.server()
	defer ts.Close()
	d, _ := newTestDriver(t, fake, ts)
	vol := mkVolume("d")
	opctx := nyc3OpCtx(vol)
	handle, _ := d.Provision(context.Background(), opctx, driver.ProvisionRequest{SizeBytes: 2 * 1_000_000_000})
	// Detached -> expand succeeds.
	if err := d.Expand(context.Background(), opctx, handle, "10G"); err != nil {
		t.Fatalf("Expand detached: %v", err)
	}
	// Attached -> ErrOnlineExpandUnsupported.
	if _, err := d.Attach(context.Background(), opctx, handle, "node-a"); err != nil {
		t.Fatalf("Attach: %v", err)
	}
	if err := d.Expand(context.Background(), opctx, handle, "20G"); !errors.Is(err, driver.ErrOnlineExpandUnsupported) {
		t.Fatalf("expected ErrOnlineExpandUnsupported when attached, got %v", err)
	}
}

// nyc3OpCtx builds an OpContext with the volume + a region=nyc3
// parameter map that every Provision-shaped test in this file needs.
// Keeping the construction in a helper means the bulk of each test
// body stays focused on the behaviour being asserted rather than on
// the boilerplate of building the request.
func nyc3OpCtx(vol *types.Volume) driver.OpContext {
	return driver.OpContext{
		Volume: vol,
		Parameters: map[string]string{
			"region":   "nyc3",
			"apiToken": "test-token",
		},
	}
}

func TestExpand_ParsesQuantityFormats(t *testing.T) {
	cases := map[string]int64{
		"5Gi":    5 * (1 << 30),
		"5G":     5_000_000_000,
		"1024Mi": 1024 * (1 << 20),
		"500":    500,
		"1Ti":    1 << 40,
	}
	for input, want := range cases {
		got, err := parseQuantity(input)
		if err != nil {
			t.Errorf("parseQuantity(%q): %v", input, err)
			continue
		}
		if got != want {
			t.Errorf("parseQuantity(%q) = %d, want %d", input, got, want)
		}
	}
	// Errors
	for _, bad := range []string{"", "abc", "5XB", "-5"} {
		if _, err := parseQuantity(bad); err == nil {
			t.Errorf("parseQuantity(%q) expected error", bad)
		}
	}
}

func TestBytesToGigabytes(t *testing.T) {
	cases := map[int64]int64{
		1:              1, // tiny -> rounds to 1GB minimum
		1_000_000_000:  1,
		1_500_000_000:  2, // round up
		10_000_000_000: 10,
	}
	for in, want := range cases {
		got, err := bytesToGigabytes(in)
		if err != nil {
			t.Errorf("bytesToGigabytes(%d): %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("bytesToGigabytes(%d) = %d, want %d", in, got, want)
		}
	}
	if _, err := bytesToGigabytes(0); err == nil {
		t.Fatalf("expected error for 0 bytes")
	}
}

func TestSanitizeDOName(t *testing.T) {
	cases := map[string]string{
		"rune-default-data":     "rune-default-data",
		"rune-Default/Data":     "rune-default-data",
		"rune-_-_-strip":        "rune-strip",
		"123-leading-digit":     "v123-leading-digit",
		strings.Repeat("a", 80): strings.Repeat("a", 64),
	}
	for in, want := range cases {
		if got := sanitizeDOName(in, 64); got != want {
			t.Errorf("sanitizeDOName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestActionPolling_Errored(t *testing.T) {
	fake := newFakeDO(t)
	fake.actionErr = true
	fake.addDroplet("n", 1)
	ts := fake.server()
	defer ts.Close()
	d, _ := newTestDriver(t, fake, ts)
	vol := mkVolume("d")
	opctx := nyc3OpCtx(vol)
	handle, _ := d.Provision(context.Background(), opctx, driver.ProvisionRequest{SizeBytes: 1 << 30})
	_, err := d.Attach(context.Background(), opctx, handle, "n")
	if err == nil || !strings.Contains(err.Error(), "errored") {
		t.Fatalf("expected errored action failure, got %v", err)
	}
}

func TestBearerTokenSent(t *testing.T) {
	fake := newFakeDO(t)
	ts := fake.server()
	defer ts.Close()
	d, _ := newTestDriver(t, fake, ts)
	if _, err := d.Provision(context.Background(), nyc3OpCtx(mkVolume("d")), driver.ProvisionRequest{SizeBytes: 1 << 30}); err != nil {
		t.Fatalf("Provision: %v", err)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.tokenSeen) == 0 {
		t.Fatalf("no Authorization header observed")
	}
	for _, tok := range fake.tokenSeen {
		if tok != "test-token" {
			t.Fatalf("got token %q, want test-token", tok)
		}
	}
}

// Sanity: server-error responses surface as Provision errors with HTTP code.
func TestProvision_ServerError(t *testing.T) {
	fake := newFakeDO(t)
	fake.hooks["/v2/volumes"] = func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"id":"server_error"}`, http.StatusInternalServerError)
	}
	ts := fake.server()
	defer ts.Close()
	d, _ := newTestDriver(t, fake, ts)
	_, err := d.Provision(context.Background(), nyc3OpCtx(mkVolume("d")), driver.ProvisionRequest{SizeBytes: 1 << 30})
	if err == nil || !strings.Contains(err.Error(), "HTTP 500") {
		t.Fatalf("expected HTTP 500 error, got %v", err)
	}
}

// TestClient_AuthFailureIsTypedAndTerminal: a rejected credential must be
// classifiable as driver.ErrAuth rather than an opaque HTTP string. Callers
// use it to tell an operator "fix the token" instead of describing a terminal
// failure as a transient "will retry" (#169).
func TestClient_AuthFailureIsTypedAndTerminal(t *testing.T) {
	for _, code := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		fake := newFakeDO(t)
		ts := fake.server()
		fake.hooks["/v2/volumes"] = func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(code)
			_, _ = w.Write([]byte(`{"id":"Unauthorized","message":"Unable to authenticate you"}`))
		}
		d, _ := newTestDriver(t, fake, ts)

		_, err := d.Attach(context.Background(), nyc3OpCtx(mkVolume("data")), "vol-123", "node-a")
		ts.Close()
		if err == nil {
			t.Fatalf("HTTP %d: expected an error", code)
		}
		if !errors.Is(err, driver.ErrAuth) {
			t.Errorf("HTTP %d: error is not classifiable as driver.ErrAuth: %v", code, err)
		}
		if !strings.Contains(err.Error(), "apiToken") {
			t.Errorf("HTTP %d: error should point at the credential to fix: %v", code, err)
		}
	}
}
