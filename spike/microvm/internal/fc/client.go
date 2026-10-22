// Package fc is a minimal Firecracker API client over its unix socket,
// covering only what the RUNE-322 spike drives. Field names follow
// firecracker.yaml (the swagger spec shipped with each release).
package fc

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"time"
)

type Client struct {
	sock string
	http *http.Client
}

func New(sock string) *Client {
	return &Client{
		sock: sock,
		http: &http.Client{
			Transport: &http.Transport{
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					var d net.Dialer
					return d.DialContext(ctx, "unix", sock)
				},
				DisableCompression: true,
			},
		},
	}
}

// WaitReady polls until the API socket accepts connections.
func (c *Client) WaitReady(ctx context.Context) error {
	for {
		if _, err := os.Stat(c.sock); err == nil {
			conn, err := net.Dial("unix", c.sock)
			if err == nil {
				conn.Close()
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("firecracker API socket %s: %w", c.sock, ctx.Err())
		case <-time.After(500 * time.Microsecond):
		}
	}
}

func (c *Client) do(ctx context.Context, method, path string, body any) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://localhost"+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		return fmt.Errorf("%s %s: %s: %s", method, path, resp.Status, bytes.TrimSpace(msg))
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	return nil
}

type MachineConfig struct {
	VCPUCount       int  `json:"vcpu_count"`
	MemSizeMiB      int  `json:"mem_size_mib"`
	SMT             bool `json:"smt"`
	TrackDirtyPages bool `json:"track_dirty_pages"`
}

type BootSource struct {
	KernelImagePath string `json:"kernel_image_path"`
	BootArgs        string `json:"boot_args,omitempty"`
	InitrdPath      string `json:"initrd_path,omitempty"`
}

type Drive struct {
	DriveID      string `json:"drive_id"`
	PathOnHost   string `json:"path_on_host"`
	IsRootDevice bool   `json:"is_root_device"`
	IsReadOnly   bool   `json:"is_read_only"`
}

type Vsock struct {
	GuestCID uint32 `json:"guest_cid"`
	UDSPath  string `json:"uds_path"`
}

type Balloon struct {
	AmountMiB             int  `json:"amount_mib"`
	DeflateOnOOM          bool `json:"deflate_on_oom"`
	StatsPollingIntervalS int  `json:"stats_polling_interval_s,omitempty"`
}

type SnapshotCreate struct {
	SnapshotType string `json:"snapshot_type"` // "Full" or "Diff"
	SnapshotPath string `json:"snapshot_path"`
	MemFilePath  string `json:"mem_file_path"`
}

type MemBackend struct {
	BackendType string `json:"backend_type"` // "File" or "Uffd"
	BackendPath string `json:"backend_path"`
}

type SnapshotLoad struct {
	SnapshotPath    string     `json:"snapshot_path"`
	MemBackend      MemBackend `json:"mem_backend"`
	TrackDirtyPages bool       `json:"track_dirty_pages,omitempty"`
	ResumeVM        bool       `json:"resume_vm"`
}

func (c *Client) PutMachineConfig(ctx context.Context, m MachineConfig) error {
	return c.do(ctx, http.MethodPut, "/machine-config", m)
}

func (c *Client) PutBootSource(ctx context.Context, b BootSource) error {
	return c.do(ctx, http.MethodPut, "/boot-source", b)
}

func (c *Client) PutDrive(ctx context.Context, d Drive) error {
	return c.do(ctx, http.MethodPut, "/drives/"+d.DriveID, d)
}

func (c *Client) PutVsock(ctx context.Context, v Vsock) error {
	return c.do(ctx, http.MethodPut, "/vsock", v)
}

// PutEntropy attaches a virtio-rng device so the guest has a fast entropy source.
func (c *Client) PutEntropy(ctx context.Context) error {
	return c.do(ctx, http.MethodPut, "/entropy", struct{}{})
}

func (c *Client) PutBalloon(ctx context.Context, b Balloon) error {
	return c.do(ctx, http.MethodPut, "/balloon", b)
}

func (c *Client) Start(ctx context.Context) error {
	return c.do(ctx, http.MethodPut, "/actions", map[string]string{"action_type": "InstanceStart"})
}

func (c *Client) Pause(ctx context.Context) error {
	return c.do(ctx, http.MethodPatch, "/vm", map[string]string{"state": "Paused"})
}

func (c *Client) Resume(ctx context.Context) error {
	return c.do(ctx, http.MethodPatch, "/vm", map[string]string{"state": "Resumed"})
}

func (c *Client) CreateSnapshot(ctx context.Context, s SnapshotCreate) error {
	return c.do(ctx, http.MethodPut, "/snapshot/create", s)
}

func (c *Client) LoadSnapshot(ctx context.Context, s SnapshotLoad) error {
	return c.do(ctx, http.MethodPut, "/snapshot/load", s)
}
