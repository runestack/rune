package dovolume

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// doClient is the minimal DigitalOcean API surface dovolume needs.
// Implemented by httpClient (production) and stubbed in tests.
//
// Every method takes ctx and respects ctx.Done(). Long-running actions
// (Attach, Detach, Resize) poll the actions endpoint until completion.
type doClient interface {
	createVolume(ctx context.Context, in createVolumeIn) (*doVolume, error)
	createVolumeFromSnapshot(ctx context.Context, in restoreVolumeIn) (*doVolume, error)
	getVolume(ctx context.Context, id string) (*doVolume, error)
	deleteVolume(ctx context.Context, id string) error
	volumeAction(ctx context.Context, id string, body map[string]any) (*doAction, error)
	getAction(ctx context.Context, id int64) (*doAction, error)
	createSnapshot(ctx context.Context, volumeID, name string) (*doSnapshot, error)
	deleteSnapshot(ctx context.Context, id string) error
	dropletByName(ctx context.Context, name string) (int64, error)
}

// doVolume mirrors the relevant fields of the DO Volume resource.
type doVolume struct {
	ID            string  `json:"id"`
	Name          string  `json:"name"`
	SizeGigabytes int64   `json:"size_gigabytes"`
	Region        doSlug  `json:"region"`
	DropletIDs    []int64 `json:"droplet_ids"`
	FilesystemTyp string  `json:"filesystem_type,omitempty"`
}

type doSlug struct {
	Slug string `json:"slug"`
}

type doAction struct {
	ID     int64  `json:"id"`
	Status string `json:"status"`
	Type   string `json:"type"`
}

type doSnapshot struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	SizeGigabytes int64  `json:"size_gigabytes"`
}

// createVolumeIn is the payload for POST /v2/volumes (new volume).
type createVolumeIn struct {
	Name           string `json:"name"`
	Region         string `json:"region"`
	SizeGigabytes  int64  `json:"size_gigabytes"`
	FilesystemType string `json:"filesystem_type,omitempty"`
	Description    string `json:"description,omitempty"`
}

// restoreVolumeIn is the payload for POST /v2/volumes (restore from snapshot).
type restoreVolumeIn struct {
	Name          string `json:"name"`
	Region        string `json:"region"`
	SizeGigabytes int64  `json:"size_gigabytes"`
	SnapshotID    string `json:"snapshot_id"`
}

// httpClient is the production doClient backed by net/http. Each call
// reads the bearer token from context (stashed by the driver via
// withToken before calling the client) so secret rotation takes effect
// on the next reconcile — the controller resolves the StorageClass
// parameters.apiToken secret ref on every call and the freshly
// resolved value flows through OpContext.
type httpClient struct {
	cfg  *Config
	http *http.Client

	// pollInterval governs action polling; tests shrink this. Default
	// 2s.
	pollInterval time.Duration
}

func newHTTPClient(cfg *Config) *httpClient {
	return &httpClient{
		cfg:          cfg,
		http:         &http.Client{Timeout: 30 * time.Second},
		pollInterval: 2 * time.Second,
	}
}

// tokenCtxKey is the context key under which the driver stashes the
// per-call bearer token for the HTTP client. Sourced from the
// StorageClass `parameters.apiToken` value (already resolved by the
// controller-side secret resolver).
type tokenCtxKey struct{}

func withToken(ctx context.Context, tok string) context.Context {
	return context.WithValue(ctx, tokenCtxKey{}, tok)
}

func tokenFromContext(ctx context.Context) (string, error) {
	v, _ := ctx.Value(tokenCtxKey{}).(string)
	if strings.TrimSpace(v) == "" {
		return "", errors.New("dovolume: parameters.apiToken is required (set on StorageClass.parameters, prefer a secret:<name>.<ns>.rune/<key> reference)")
	}
	return v, nil
}

// doRequest is the single HTTP call helper. Marshals body if non-nil,
// authenticates, sends, decodes the JSON response into out. 404 returns
// errDONotFound so Delete can swallow it for idempotency.
func (c *httpClient) doRequest(ctx context.Context, method, path string, body any, out any) error {
	tok, err := tokenFromContext(ctx)
	if err != nil {
		return err
	}
	var reader io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("dovolume: marshal request: %w", err)
		}
		reader = bytes.NewReader(buf)
	}
	url := c.cfg.APIBaseURL + path
	req, err := http.NewRequestWithContext(ctx, method, url, reader)
	if err != nil {
		return fmt.Errorf("dovolume: build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("dovolume: %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode == http.StatusNotFound {
		return errDONotFound
	}
	if resp.StatusCode >= 400 {
		return fmt.Errorf("dovolume: %s %s -> HTTP %d: %s", method, path, resp.StatusCode, strings.TrimSpace(string(respBody)))
	}
	if out != nil && len(respBody) > 0 {
		if err := json.Unmarshal(respBody, out); err != nil {
			return fmt.Errorf("dovolume: decode %s %s: %w", method, path, err)
		}
	}
	return nil
}

// errDONotFound is returned for HTTP 404 from the DO API. The driver
// translates it into driver.ErrNotFound at the boundary.
var errDONotFound = errors.New("dovolume: do resource not found")

// --- volume CRUD --------------------------------------------------------

func (c *httpClient) createVolume(ctx context.Context, in createVolumeIn) (*doVolume, error) {
	var resp struct {
		Volume *doVolume `json:"volume"`
	}
	if err := c.doRequest(ctx, "POST", "/v2/volumes", in, &resp); err != nil {
		return nil, err
	}
	if resp.Volume == nil {
		return nil, errors.New("dovolume: createVolume response missing volume")
	}
	return resp.Volume, nil
}

func (c *httpClient) createVolumeFromSnapshot(ctx context.Context, in restoreVolumeIn) (*doVolume, error) {
	var resp struct {
		Volume *doVolume `json:"volume"`
	}
	if err := c.doRequest(ctx, "POST", "/v2/volumes", in, &resp); err != nil {
		return nil, err
	}
	if resp.Volume == nil {
		return nil, errors.New("dovolume: createVolumeFromSnapshot response missing volume")
	}
	return resp.Volume, nil
}

func (c *httpClient) getVolume(ctx context.Context, id string) (*doVolume, error) {
	var resp struct {
		Volume *doVolume `json:"volume"`
	}
	if err := c.doRequest(ctx, "GET", "/v2/volumes/"+id, nil, &resp); err != nil {
		return nil, err
	}
	if resp.Volume == nil {
		return nil, errors.New("dovolume: getVolume response missing volume")
	}
	return resp.Volume, nil
}

func (c *httpClient) deleteVolume(ctx context.Context, id string) error {
	return c.doRequest(ctx, "DELETE", "/v2/volumes/"+id, nil, nil)
}

// --- actions ------------------------------------------------------------

func (c *httpClient) volumeAction(ctx context.Context, id string, body map[string]any) (*doAction, error) {
	var resp struct {
		Action *doAction `json:"action"`
	}
	if err := c.doRequest(ctx, "POST", "/v2/volumes/"+id+"/actions", body, &resp); err != nil {
		return nil, err
	}
	if resp.Action == nil {
		return nil, errors.New("dovolume: volumeAction response missing action")
	}
	return resp.Action, nil
}

func (c *httpClient) getAction(ctx context.Context, id int64) (*doAction, error) {
	var resp struct {
		Action *doAction `json:"action"`
	}
	if err := c.doRequest(ctx, "GET", "/v2/actions/"+strconv.FormatInt(id, 10), nil, &resp); err != nil {
		return nil, err
	}
	if resp.Action == nil {
		return nil, errors.New("dovolume: getAction response missing action")
	}
	return resp.Action, nil
}

// waitForAction polls getAction until the action reports completed or
// errored. Returns nil on completed, an error on errored or ctx
// expiry. The caller is responsible for setting a context deadline.
func waitForAction(ctx context.Context, c doClient, actionID int64, interval time.Duration) error {
	if interval <= 0 {
		interval = 2 * time.Second
	}
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("dovolume: wait for action %d: %w", actionID, ctx.Err())
		case <-timer.C:
		}
		act, err := c.getAction(ctx, actionID)
		if err != nil {
			return err
		}
		switch act.Status {
		case "completed":
			return nil
		case "errored":
			return fmt.Errorf("dovolume: action %d (%s) errored", actionID, act.Type)
		default:
			// in-progress — keep polling
		}
		timer.Reset(interval)
	}
}

// --- snapshots ----------------------------------------------------------

func (c *httpClient) createSnapshot(ctx context.Context, volumeID, name string) (*doSnapshot, error) {
	var resp struct {
		Snapshot *doSnapshot `json:"snapshot"`
	}
	body := map[string]any{"name": name}
	if err := c.doRequest(ctx, "POST", "/v2/volumes/"+volumeID+"/snapshots", body, &resp); err != nil {
		return nil, err
	}
	if resp.Snapshot == nil {
		return nil, errors.New("dovolume: createSnapshot response missing snapshot")
	}
	return resp.Snapshot, nil
}

func (c *httpClient) deleteSnapshot(ctx context.Context, id string) error {
	return c.doRequest(ctx, "DELETE", "/v2/snapshots/"+id, nil, nil)
}

// --- droplet lookup -----------------------------------------------------

// dropletByName returns the DO droplet ID for the droplet whose name
// matches. Returns errDONotFound when no droplet matches; an error
// when more than one matches (ambiguous — operator must rename).
//
// Used by Attach/Detach to translate Rune NodeID (which equals the
// droplet name on rune-on-DO clusters) into a DO droplet_id.
func (c *httpClient) dropletByName(ctx context.Context, name string) (int64, error) {
	var resp struct {
		Droplets []struct {
			ID   int64  `json:"id"`
			Name string `json:"name"`
		} `json:"droplets"`
	}
	// DO supports ?name= filtering on /v2/droplets.
	if err := c.doRequest(ctx, "GET", "/v2/droplets?name="+name+"&per_page=200", nil, &resp); err != nil {
		return 0, err
	}
	matches := make([]int64, 0, 2)
	for _, d := range resp.Droplets {
		if d.Name == name {
			matches = append(matches, d.ID)
		}
	}
	switch len(matches) {
	case 0:
		return 0, errDONotFound
	case 1:
		return matches[0], nil
	default:
		return 0, fmt.Errorf("dovolume: %d DO droplets match name %q (ambiguous)", len(matches), name)
	}
}
