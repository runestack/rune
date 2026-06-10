package client

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/runestack/rune/pkg/api/generated"
	"github.com/runestack/rune/pkg/log"
	"github.com/runestack/rune/pkg/release"
	"github.com/runestack/rune/pkg/types"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// CastPayloads carries the rendered resource objects for a Cast, keyed by
// OwnerRef.Key(). The CLI renders the castfile set into these before calling
// Cast; the server stamps ownership and applies them.
type CastPayloads struct {
	Services   map[string]*types.Service
	Secrets    map[string]*types.Secret
	Configmaps map[string]*types.Configmap
	Volumes    map[string]*types.Volume
}

// ReleaseClient talks to the ReleaseService for stateful runeset releases
// (RUNESET_STATEFUL_RELEASES.md): list, get, history, diff (Plan), uninstall
// (DeleteRelease), and rollback.
type ReleaseClient struct {
	client *Client
	logger log.Logger
	svc    generated.ReleaseServiceClient
}

// NewReleaseClient creates a new release client.
func NewReleaseClient(client *Client) *ReleaseClient {
	return &ReleaseClient{
		client: client,
		logger: client.logger.WithComponent("release-client"),
		svc:    generated.NewReleaseServiceClient(client.conn),
	}
}

// GetLogger returns the logger for this client.
func (c *ReleaseClient) GetLogger() log.Logger { return c.logger }

// PlannedChange is a render-friendly projection of a single planned reconcile
// change returned by Plan.
type PlannedChange struct {
	ResourceType string
	Namespace    string
	Name         string
	Action       string
	Conflict     string
}

// Plan is the render-friendly projection of a reconcile plan (diff).
type Plan struct {
	Release   string
	Namespace string
	Applyable bool
	Changes   []PlannedChange
}

// ListReleases lists releases. namespace="" lists across all namespaces.
func (c *ReleaseClient) ListReleases(namespace string, includeUninstalled bool) ([]*types.Release, error) {
	req := &generated.ListReleasesRequest{Namespace: namespace, IncludeUninstalled: includeUninstalled}
	ctx, cancel := c.client.Context()
	defer cancel()
	resp, err := c.svc.ListReleases(ctx, req)
	if err != nil {
		return nil, convertGRPCError("list releases", err)
	}
	if resp.Status != nil && resp.Status.Code != int32(codes.OK) {
		return nil, fmt.Errorf("API error: %s", resp.Status.Message)
	}
	out := make([]*types.Release, 0, len(resp.Releases))
	for _, r := range resp.Releases {
		rel, err := protoToRelease(r)
		if err != nil {
			c.logger.Warn("Failed to convert release", log.Err(err))
			continue
		}
		out = append(out, rel)
	}
	return out, nil
}

// GetRelease retrieves a single release by name.
func (c *ReleaseClient) GetRelease(namespace, name string) (*types.Release, error) {
	req := &generated.GetReleaseRequest{Name: name, Namespace: namespace}
	ctx, cancel := c.client.Context()
	defer cancel()
	resp, err := c.svc.GetRelease(ctx, req)
	if err != nil {
		statusErr, ok := status.FromError(err)
		if ok && statusErr.Code() == codes.NotFound {
			return nil, fmt.Errorf("release not found: %s/%s", namespace, name)
		}
		return nil, convertGRPCError("get release", err)
	}
	if resp.Status != nil && resp.Status.Code != int32(codes.OK) {
		return nil, fmt.Errorf("API error: %s", resp.Status.Message)
	}
	return protoToRelease(resp.Release)
}

// History returns the revision log of a release, newest first.
func (c *ReleaseClient) History(namespace, name string) ([]*types.Release, error) {
	req := &generated.HistoryRequest{Name: name, Namespace: namespace}
	ctx, cancel := c.client.Context()
	defer cancel()
	resp, err := c.svc.History(ctx, req)
	if err != nil {
		return nil, convertGRPCError("release history", err)
	}
	if resp.Status != nil && resp.Status.Code != int32(codes.OK) {
		return nil, fmt.Errorf("API error: %s", resp.Status.Message)
	}
	out := make([]*types.Release, 0, len(resp.Revisions))
	for _, r := range resp.Revisions {
		rel, err := protoToRelease(r)
		if err != nil {
			c.logger.Warn("Failed to convert release revision", log.Err(err))
			continue
		}
		out = append(out, rel)
	}
	return out, nil
}

// Diff computes the reconcile plan for a release without applying it. Today the
// CLI exercises this for an already-recorded release by supplying its current
// desired ref set; the full render-from-source path lands with the cast PR.
func (c *ReleaseClient) Diff(namespace, name string, resources []types.OwnerRef) (*Plan, error) {
	spec := &generated.ReleaseSpec{Name: name, Namespace: namespace}
	for _, ref := range resources {
		spec.Resources = append(spec.Resources, &generated.OwnerRef{
			ResourceType: string(ref.ResourceType),
			Namespace:    ref.Namespace,
			Name:         ref.Name,
		})
	}
	req := &generated.PlanRequest{Spec: spec}
	ctx, cancel := c.client.Context()
	defer cancel()
	resp, err := c.svc.Plan(ctx, req)
	if err != nil {
		return nil, convertGRPCError("release diff", err)
	}
	if resp.Status != nil && resp.Status.Code != int32(codes.OK) {
		return nil, fmt.Errorf("API error: %s", resp.Status.Message)
	}
	return protoToPlan(resp.Plan), nil
}

// ErrDetachWouldPrune mirrors release.ErrDetachWouldPrune for callers that want
// to detect a refused-detach precondition without string matching.
var ErrDetachWouldPrune = release.ErrDetachWouldPrune

// Cast runs a server-side reconcile for a fully-rendered release spec + payloads
// (C1: client renders, server plans+applies). It returns the recorded Release
// and the executed Plan. A plan with unresolved ownership conflicts, or a detach
// that would prune, surfaces as a FailedPrecondition carrying the Plan.
func (c *ReleaseClient) Cast(spec release.ReleaseSpec, payloads CastPayloads, timeout time.Duration) (*types.Release, *Plan, error) {
	pspec := specToProto(spec)
	pspec.Payloads = payloadsToProto(payloads)
	req := &generated.CastRequest{Spec: pspec}
	// Bound the whole release by the caller's --timeout (aggregate, §4) rather
	// than the default per-call timeout.
	ctx, cancel := c.client.ContextWithTimeout(timeout)
	defer cancel()
	resp, err := c.svc.Cast(ctx, req)
	if err != nil {
		// FailedPrecondition carries an actionable plan (conflicts / detach-prune).
		var plan *Plan
		if resp != nil {
			plan = protoToPlan(resp.GetPlan())
		}
		return nil, plan, classifyCastError("cast", err)
	}
	if resp.Status != nil && resp.Status.Code != int32(codes.OK) {
		return nil, protoToPlan(resp.GetPlan()), fmt.Errorf("API error: %s", resp.Status.Message)
	}
	rel, err := protoToRelease(resp.GetRelease())
	if err != nil {
		return nil, protoToPlan(resp.GetPlan()), err
	}
	return rel, protoToPlan(resp.GetPlan()), nil
}

// PlanSpec computes the reconcile plan for a fully-rendered spec WITHOUT applying
// it — the online dry-run / `release diff` path (C4). Unlike Diff (which plans
// against a recorded release's owned set), this plans the freshly-rendered
// desired set from source.
func (c *ReleaseClient) PlanSpec(spec release.ReleaseSpec) (*Plan, error) {
	req := &generated.PlanRequest{Spec: specToProto(spec)}
	ctx, cancel := c.client.Context()
	defer cancel()
	resp, err := c.svc.Plan(ctx, req)
	if err != nil {
		return nil, convertGRPCError("plan", err)
	}
	if resp.Status != nil && resp.Status.Code != int32(codes.OK) {
		return nil, fmt.Errorf("API error: %s", resp.Status.Message)
	}
	return protoToPlan(resp.Plan), nil
}

// classifyCastError maps a Cast gRPC error to a stable client error. The server
// reports detach-would-prune and conflicts as FailedPrecondition with the cause
// in the message; we re-surface ErrDetachWouldPrune as the typed sentinel.
func classifyCastError(op string, err error) error {
	if st, ok := status.FromError(err); ok {
		if st.Code() == codes.FailedPrecondition && strings.Contains(st.Message(), "detach") && strings.Contains(st.Message(), "prune") {
			return ErrDetachWouldPrune
		}
		if st.Code() == codes.FailedPrecondition {
			return errors.New(st.Message())
		}
	}
	return convertGRPCError(op, err)
}

// specToProto converts a pure-core release.ReleaseSpec into its proto form
// (without payloads — Cast adds those separately).
func specToProto(spec release.ReleaseSpec) *generated.ReleaseSpec {
	out := &generated.ReleaseSpec{
		Name:           spec.Name,
		Namespace:      spec.Namespace,
		Source:         sourceToProto(spec.Source),
		Manifest:       manifestToProto(spec.Manifest),
		RenderedDigest: spec.RenderedDigest,
		Adopt:          spec.Options.Adopt,
		Detach:         spec.Detach,
		Atomic:         spec.Atomic,
	}
	if len(spec.Values) > 0 {
		if b, err := json.Marshal(spec.Values); err == nil {
			out.ValuesJson = string(b)
		}
	}
	for _, d := range spec.Resources {
		out.Resources = append(out.Resources, &generated.OwnerRef{
			ResourceType: string(d.Ref.ResourceType),
			Namespace:    d.Ref.Namespace,
			Name:         d.Ref.Name,
		})
	}
	return out
}

func sourceToProto(s types.ReleaseSource) *generated.ReleaseSource {
	return &generated.ReleaseSource{
		Type:     string(s.Type),
		Location: s.Location,
		Ref:      s.Ref,
		Digest:   s.Digest,
	}
}

func manifestToProto(m types.RunesetManifest) *generated.RunesetManifest {
	out := &generated.RunesetManifest{
		Name:        m.Name,
		Version:     m.Version,
		Description: m.Description,
		Namespace:   m.Namespace,
	}
	if len(m.Defaults) > 0 {
		if b, err := json.Marshal(m.Defaults); err == nil {
			out.DefaultsJson = string(b)
		}
	}
	return out
}

func payloadsToProto(p CastPayloads) *generated.RenderedPayloads {
	out := &generated.RenderedPayloads{
		Services:   map[string]string{},
		Secrets:    map[string]string{},
		Configmaps: map[string]string{},
		Volumes:    map[string]string{},
	}
	for k, v := range p.Services {
		if b, err := json.Marshal(v); err == nil {
			out.Services[k] = string(b)
		}
	}
	for k, v := range p.Secrets {
		if b, err := json.Marshal(v); err == nil {
			out.Secrets[k] = string(b)
		}
	}
	for k, v := range p.Configmaps {
		if b, err := json.Marshal(v); err == nil {
			out.Configmaps[k] = string(b)
		}
	}
	for k, v := range p.Volumes {
		if b, err := json.Marshal(v); err == nil {
			out.Volumes[k] = string(b)
		}
	}
	return out
}

// DeleteRelease uninstalls a release (soft by default; purge to forget — D4).
func (c *ReleaseClient) DeleteRelease(namespace, name string, keepVolumes, purge bool) error {
	req := &generated.DeleteReleaseRequest{
		Name:        name,
		Namespace:   namespace,
		KeepVolumes: keepVolumes,
		Purge:       purge,
	}
	ctx, cancel := c.client.Context()
	defer cancel()
	resp, err := c.svc.DeleteRelease(ctx, req)
	if err != nil {
		return convertGRPCError("delete release", err)
	}
	if resp.Code != int32(codes.OK) {
		return fmt.Errorf("API error: %s", resp.Message)
	}
	return nil
}

// Rollback rolls a release forward to a prior revision. The server currently
// returns Unimplemented (the re-render path lands with the cast PR).
func (c *ReleaseClient) Rollback(namespace, name string, revision int) (*types.Release, error) {
	req := &generated.RollbackReleaseRequest{Name: name, Namespace: namespace, Revision: int32(revision)} //nolint:gosec // revision bounded by caller
	ctx, cancel := c.client.Context()
	defer cancel()
	resp, err := c.svc.Rollback(ctx, req)
	if err != nil {
		return nil, convertGRPCError("rollback release", err)
	}
	if resp.Status != nil && resp.Status.Code != int32(codes.OK) {
		return nil, fmt.Errorf("API error: %s", resp.Status.Message)
	}
	return protoToRelease(resp.Release)
}

// --- conversions ---

func protoToRelease(p *generated.Release) (*types.Release, error) {
	if p == nil {
		return nil, nil
	}
	rel := &types.Release{
		ID:             p.GetId(),
		Name:           p.GetName(),
		Namespace:      p.GetNamespace(),
		Status:         types.ReleaseStatus(p.GetStatus()),
		Revision:       int(p.GetRevision()),
		RenderedDigest: p.GetRenderedDigest(),
	}
	if src := p.GetSource(); src != nil {
		rel.Source = types.ReleaseSource{
			Type:     types.RunesetSourceType(src.GetType()),
			Location: src.GetLocation(),
			Ref:      src.GetRef(),
			Digest:   src.GetDigest(),
		}
	}
	if mf := p.GetManifest(); mf != nil {
		rel.Manifest = types.RunesetManifest{
			Name:        mf.GetName(),
			Version:     mf.GetVersion(),
			Description: mf.GetDescription(),
			Namespace:   mf.GetNamespace(),
		}
		if dj := mf.GetDefaultsJson(); dj != "" {
			_ = json.Unmarshal([]byte(dj), &rel.Manifest.Defaults)
		}
	}
	if vj := p.GetValuesJson(); vj != "" {
		_ = json.Unmarshal([]byte(vj), &rel.Values)
	}
	for _, ref := range p.GetOwns() {
		rel.Owns = append(rel.Owns, protoToOwnerRef(ref))
	}
	for _, ref := range p.GetReferences() {
		rel.References = append(rel.References, protoToOwnerRef(ref))
	}
	if ca := p.GetCreatedAt(); ca != "" {
		if t, err := time.Parse(time.RFC3339, ca); err == nil {
			rel.CreatedAt = t
		}
	}
	if ua := p.GetUpdatedAt(); ua != "" {
		if t, err := time.Parse(time.RFC3339, ua); err == nil {
			rel.UpdatedAt = t
		}
	}
	return rel, nil
}

func protoToOwnerRef(p *generated.OwnerRef) types.OwnerRef {
	return types.OwnerRef{
		ResourceType: types.ResourceType(p.GetResourceType()),
		Namespace:    p.GetNamespace(),
		Name:         p.GetName(),
	}
}

func protoToPlan(p *generated.Plan) *Plan {
	if p == nil {
		return nil
	}
	out := &Plan{
		Release:   p.GetRelease(),
		Namespace: p.GetNamespace(),
		Applyable: p.GetApplyable(),
	}
	for _, ch := range p.GetChanges() {
		pc := PlannedChange{
			Action:   actionString(ch.GetAction()),
			Conflict: ch.GetConflictReason(),
		}
		if ref := ch.GetRef(); ref != nil {
			pc.ResourceType = ref.GetResourceType()
			pc.Namespace = ref.GetNamespace()
			pc.Name = ref.GetName()
		}
		out.Changes = append(out.Changes, pc)
	}
	return out
}

func actionString(a generated.Action) string {
	switch a {
	case generated.Action_ACTION_CREATE:
		return "create"
	case generated.Action_ACTION_UPDATE:
		return "update"
	case generated.Action_ACTION_PRUNE:
		return "prune"
	case generated.Action_ACTION_ADOPT:
		return "adopt"
	case generated.Action_ACTION_REFERENCE:
		return "reference"
	default:
		return "unknown"
	}
}
