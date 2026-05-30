package hcloudvolume

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// hcloudClient is the minimal Hetzner Cloud API surface hcloudvolume
// needs. Implemented by httpClient (production) and stubbed in tests.
//
// Every method takes ctx and respects ctx.Done(). Long-running actions
// (Attach, Detach, Resize) poll the actions endpoint until completion.
type hcloudClient interface {
	createVolume(ctx context.Context, in createVolumeIn) (*hcVolume, *hcAction, error)
	getVolume(ctx context.Context, id int64) (*hcVolume, error)
	deleteVolume(ctx context.Context, id int64) error
	attachVolume(ctx context.Context, volumeID, serverID int64) (*hcAction, error)
	detachVolume(ctx context.Context, volumeID int64) (*hcAction, error)
	resizeVolume(ctx context.Context, volumeID int64, sizeGB int64) (*hcAction, error)
	getAction(ctx context.Context, id int64) (*hcAction, error)
	serverByName(ctx context.Context, name string) (int64, error)
}

// hcVolume mirrors the relevant fields of the Hetzner Cloud Volume
// resource. Servers attaches a single ID per volume in Hetzner — volumes
// are RWO only.
type hcVolume struct {
	ID          int64  `json:"id"`
	Name        string `json:"name"`
	Size        int64  `json:"size"`             // GB (decimal)
	Server      *int64 `json:"server,omitempty"` // server ID when attached
	Location    hcLoc  `json:"location"`
	LinuxDevice string `json:"linux_device,omitempty"`
	Format      string `json:"format,omitempty"`
}

type hcLoc struct {
	Name string `json:"name"`
}

type hcAction struct {
	ID       int64  `json:"id"`
	Status   string `json:"status"` // running | success | error
	Command  string `json:"command"`
	ErrorMsg string `json:"-"`
}

// rawAction is the on-the-wire shape of an Action so we can pluck the
// error message when status == "error".
type rawAction struct {
	ID      int64  `json:"id"`
	Status  string `json:"status"`
	Command string `json:"command"`
	Error   *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func (r *rawAction) toAction() *hcAction {
	a := &hcAction{ID: r.ID, Status: r.Status, Command: r.Command}
	if r.Error != nil {
		a.ErrorMsg = fmt.Sprintf("%s: %s", r.Error.Code, r.Error.Message)
	}
	return a
}

// createVolumeIn is the payload for POST /v1/volumes.
type createVolumeIn struct {
	Name      string            `json:"name"`
	Size      int64             `json:"size"` // GB
	Location  string            `json:"location"`
	Format    string            `json:"format,omitempty"` // ext4, xfs — when set, Hetzner formats
	Automount bool              `json:"automount"`
	Labels    map[string]string `json:"labels,omitempty"`
}

// httpClient is the production hcloudClient backed by net/http. Each
// call reads the bearer token from context (stashed by the driver via
// withToken before calling the client) so secret rotation takes effect
// on the next reconcile.
type httpClient struct {
	cfg  *Config
	http *http.Client

	// pollInterval governs action polling; tests shrink this. Default 2s.
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
		return "", errors.New("hcloudvolume: parameters.apiToken is required (set on StorageClass.parameters, prefer a secret:<name>.<ns>.rune/<key> reference)")
	}
	return v, nil
}

// errHCNotFound is returned for HTTP 404 from the Hetzner Cloud API.
// The driver translates it into driver.ErrNotFound at the boundary.
var errHCNotFound = errors.New("hcloudvolume: hetzner resource not found")

// doRequest is the single HTTP call helper. Marshals body if non-nil,
// authenticates, sends, decodes the JSON response into out. 404 returns
// errHCNotFound so Delete can swallow it for idempotency.
func (c *httpClient) doRequest(ctx context.Context, method, path string, body any, out any) error {
	tok, err := tokenFromContext(ctx)
	if err != nil {
		return err
	}
	var reader io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("hcloudvolume: marshal request: %w", err)
		}
		reader = bytes.NewReader(buf)
	}
	url := c.cfg.APIBaseURL + path
	req, err := http.NewRequestWithContext(ctx, method, url, reader)
	if err != nil {
		return fmt.Errorf("hcloudvolume: build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("hcloudvolume: %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode == http.StatusNotFound {
		return errHCNotFound
	}
	if resp.StatusCode >= 400 {
		return fmt.Errorf("hcloudvolume: %s %s -> HTTP %d: %s", method, path, resp.StatusCode, strings.TrimSpace(string(respBody)))
	}
	if out != nil && len(respBody) > 0 {
		if err := json.Unmarshal(respBody, out); err != nil {
			return fmt.Errorf("hcloudvolume: decode %s %s: %w", method, path, err)
		}
	}
	return nil
}

// --- volume CRUD --------------------------------------------------------

func (c *httpClient) createVolume(ctx context.Context, in createVolumeIn) (*hcVolume, *hcAction, error) {
	var resp struct {
		Volume *hcVolume  `json:"volume"`
		Action *rawAction `json:"action"`
	}
	if err := c.doRequest(ctx, "POST", "/v1/volumes", in, &resp); err != nil {
		return nil, nil, err
	}
	if resp.Volume == nil {
		return nil, nil, errors.New("hcloudvolume: createVolume response missing volume")
	}
	var act *hcAction
	if resp.Action != nil {
		act = resp.Action.toAction()
	}
	return resp.Volume, act, nil
}

func (c *httpClient) getVolume(ctx context.Context, id int64) (*hcVolume, error) {
	var resp struct {
		Volume *hcVolume `json:"volume"`
	}
	if err := c.doRequest(ctx, "GET", "/v1/volumes/"+strconv.FormatInt(id, 10), nil, &resp); err != nil {
		return nil, err
	}
	if resp.Volume == nil {
		return nil, errors.New("hcloudvolume: getVolume response missing volume")
	}
	return resp.Volume, nil
}

func (c *httpClient) deleteVolume(ctx context.Context, id int64) error {
	return c.doRequest(ctx, "DELETE", "/v1/volumes/"+strconv.FormatInt(id, 10), nil, nil)
}

// --- actions ------------------------------------------------------------

func (c *httpClient) volumeAction(ctx context.Context, volumeID int64, action string, body map[string]any) (*hcAction, error) {
	var resp struct {
		Action *rawAction `json:"action"`
	}
	path := fmt.Sprintf("/v1/volumes/%d/actions/%s", volumeID, action)
	if err := c.doRequest(ctx, "POST", path, body, &resp); err != nil {
		return nil, err
	}
	if resp.Action == nil {
		return nil, fmt.Errorf("hcloudvolume: %s response missing action", action)
	}
	return resp.Action.toAction(), nil
}

func (c *httpClient) attachVolume(ctx context.Context, volumeID, serverID int64) (*hcAction, error) {
	return c.volumeAction(ctx, volumeID, "attach", map[string]any{
		"server":    serverID,
		"automount": false,
	})
}

func (c *httpClient) detachVolume(ctx context.Context, volumeID int64) (*hcAction, error) {
	return c.volumeAction(ctx, volumeID, "detach", map[string]any{})
}

func (c *httpClient) resizeVolume(ctx context.Context, volumeID, sizeGB int64) (*hcAction, error) {
	return c.volumeAction(ctx, volumeID, "resize", map[string]any{
		"size": sizeGB,
	})
}

func (c *httpClient) getAction(ctx context.Context, id int64) (*hcAction, error) {
	var resp struct {
		Action *rawAction `json:"action"`
	}
	if err := c.doRequest(ctx, "GET", "/v1/actions/"+strconv.FormatInt(id, 10), nil, &resp); err != nil {
		return nil, err
	}
	if resp.Action == nil {
		return nil, errors.New("hcloudvolume: getAction response missing action")
	}
	return resp.Action.toAction(), nil
}

// waitForAction polls getAction until the action reports success or
// error. Returns nil on success, an error on error or ctx expiry.
func waitForAction(ctx context.Context, c hcloudClient, actionID int64, interval time.Duration) error {
	if interval <= 0 {
		interval = 2 * time.Second
	}
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("hcloudvolume: wait for action %d: %w", actionID, ctx.Err())
		case <-timer.C:
		}
		act, err := c.getAction(ctx, actionID)
		if err != nil {
			return err
		}
		switch act.Status {
		case "success":
			return nil
		case "error":
			msg := act.ErrorMsg
			if msg == "" {
				msg = "unknown"
			}
			return fmt.Errorf("hcloudvolume: action %d (%s) errored: %s", actionID, act.Command, msg)
		default:
			// running — keep polling
		}
		timer.Reset(interval)
	}
}

// --- server lookup ------------------------------------------------------

// serverByName returns the Hetzner server ID for the server whose name
// matches. Returns errHCNotFound when no server matches; an error when
// more than one matches (ambiguous — operator must rename).
//
// Used by Attach/Detach to translate Rune NodeID (which equals the
// server name on rune-on-Hetzner clusters) into a Hetzner server_id.
func (c *httpClient) serverByName(ctx context.Context, name string) (int64, error) {
	var resp struct {
		Servers []struct {
			ID   int64  `json:"id"`
			Name string `json:"name"`
		} `json:"servers"`
	}
	q := "?name=" + url.QueryEscape(name)
	if err := c.doRequest(ctx, "GET", "/v1/servers"+q, nil, &resp); err != nil {
		return 0, err
	}
	matches := make([]int64, 0, 2)
	for _, s := range resp.Servers {
		if s.Name == name {
			matches = append(matches, s.ID)
		}
	}
	switch len(matches) {
	case 0:
		return 0, errHCNotFound
	case 1:
		return matches[0], nil
	default:
		return 0, fmt.Errorf("hcloudvolume: %d Hetzner servers match name %q (ambiguous)", len(matches), name)
	}
}
