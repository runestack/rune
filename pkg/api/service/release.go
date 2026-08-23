package service

import (
	"context"
	"encoding/json"
	"sort"
	"time"

	"github.com/runestack/rune/pkg/api/generated"
	"github.com/runestack/rune/pkg/authz"
	"github.com/runestack/rune/pkg/log"
	"github.com/runestack/rune/pkg/release"
	"github.com/runestack/rune/pkg/releasectl"
	"github.com/runestack/rune/pkg/store"
	"github.com/runestack/rune/pkg/store/repos"
	"github.com/runestack/rune/pkg/types"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// ReleaseService implements the gRPC ReleaseService for stateful runeset
// releases (RUNESET_STATEFUL_RELEASES.md). Write/diff verbs (Cast, Plan,
// DeleteRelease, Rollback) flow through the releasectl.Controller, which runs
// the server-side 3-way reconcile (C1) against the orchestrator + store. Read
// verbs (List, Get, History) read directly from the ReleaseRepo.
//
// RBAC verb: resource "releases" with verbs derived per method in
// pkg/api/server/utils.go (create/list/get/delete/update).
type ReleaseService struct {
	generated.UnimplementedReleaseServiceServer
	ctl    *releasectl.Controller
	repo   *repos.ReleaseRepo
	logger log.Logger
}

// NewReleaseService builds the service from a constructed controller and store.
func NewReleaseService(ctl *releasectl.Controller, coreStore store.Store, logger log.Logger) *ReleaseService {
	return &ReleaseService{
		ctl:    ctl,
		repo:   repos.NewReleaseRepo(coreStore),
		logger: logger.WithComponent("release-service"),
	}
}

// Cast creates or upgrades a release via server-side reconcile.
func (s *ReleaseService) Cast(ctx context.Context, req *generated.CastRequest) (*generated.CastResponse, error) {
	if req == nil || req.Spec == nil {
		return nil, status.Error(codes.InvalidArgument, "spec is required")
	}
	spec, err := protoToSpec(req.Spec)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid spec: %v", err)
	}
	payloads, err := protoToPayloads(req.Spec.GetPayloads())
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid payloads: %v", err)
	}

	rel, plan, err := s.ctl.Cast(ctx, spec, payloads)
	if err != nil {
		if authz.IsDenied(err) {
			return nil, status.Error(codes.PermissionDenied, err.Error())
		}
		// A conflict plan is an actionable client error, not an internal fault.
		resp := &generated.CastResponse{Plan: toProtoPlan(plan)}
		return resp, status.Errorf(codes.FailedPrecondition, "cast: %v", err)
	}
	return &generated.CastResponse{
		Release: toProtoRelease(rel),
		Plan:    toProtoPlan(plan),
		Status:  &generated.Status{Code: int32(codes.OK)},
	}, nil
}

// Plan computes the reconcile plan without applying (dry-run / diff).
func (s *ReleaseService) Plan(ctx context.Context, req *generated.PlanRequest) (*generated.PlanResponse, error) {
	if req == nil || req.Spec == nil {
		return nil, status.Error(codes.InvalidArgument, "spec is required")
	}
	spec, err := protoToSpec(req.Spec)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid spec: %v", err)
	}
	plan, err := s.ctl.Plan(ctx, spec)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "plan: %v", err)
	}
	return &generated.PlanResponse{Plan: toProtoPlan(plan), Status: &generated.Status{Code: int32(codes.OK)}}, nil
}

// ListReleases returns releases, optionally across all namespaces.
func (s *ReleaseService) ListReleases(ctx context.Context, req *generated.ListReleasesRequest) (*generated.ListReleasesResponse, error) {
	var rels []*types.Release
	var err error
	if req.GetNamespace() == "" {
		rels, err = s.repo.ListAll(ctx)
	} else {
		rels, err = s.repo.List(ctx, req.GetNamespace())
	}
	if err != nil {
		return nil, status.Errorf(codes.Internal, "list releases: %v", err)
	}
	out := make([]*generated.Release, 0, len(rels))
	for _, r := range rels {
		if !req.GetIncludeUninstalled() && r.Status == types.ReleaseStatusUninstalled {
			continue
		}
		out = append(out, toProtoRelease(r))
	}
	return &generated.ListReleasesResponse{Releases: out, Status: &generated.Status{Code: int32(codes.OK)}}, nil
}

// GetRelease returns a single release record.
func (s *ReleaseService) GetRelease(ctx context.Context, req *generated.GetReleaseRequest) (*generated.ReleaseResponse, error) {
	if req == nil || req.Name == "" {
		return nil, status.Error(codes.InvalidArgument, "name is required")
	}
	rel, err := s.repo.GetByName(ctx, req.Namespace, req.Name)
	if err != nil {
		return nil, status.Errorf(codes.NotFound, "get release: %v", err)
	}
	return &generated.ReleaseResponse{Release: toProtoRelease(rel), Status: &generated.Status{Code: int32(codes.OK)}}, nil
}

// History returns the revision log of a release, newest first.
func (s *ReleaseService) History(ctx context.Context, req *generated.HistoryRequest) (*generated.HistoryResponse, error) {
	if req == nil || req.Name == "" {
		return nil, status.Error(codes.InvalidArgument, "name is required")
	}
	ref := string(types.ResourceTypeRelease) + ":" + req.Name + "." + req.Namespace + ".rune"
	versions, err := s.repo.History(ctx, ref)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "history: %v", err)
	}
	out := make([]*generated.Release, 0, len(versions))
	for _, v := range versions {
		rel, ok := historicalToRelease(v)
		if !ok {
			continue
		}
		out = append(out, toProtoRelease(rel))
	}
	// Newest first.
	sort.SliceStable(out, func(i, j int) bool { return out[i].Revision > out[j].Revision })
	return &generated.HistoryResponse{Revisions: out, Status: &generated.Status{Code: int32(codes.OK)}}, nil
}

// RevealReleaseValues returns one revision's merged value set.
//
// Separate from GetRelease/History for the same reason RevealSecret is
// separate from GetSecret: a value set routinely carries secret material
// (`--set postgres.password=...` rendered into a secret), while the read path
// is authorized by releases:get/list — which `readonly` holds. This RPC is
// gated on releases:reveal, granted by no builtin policy below admin.
func (s *ReleaseService) RevealReleaseValues(ctx context.Context, req *generated.RevealReleaseValuesRequest) (*generated.RevealReleaseValuesResponse, error) {
	if req == nil || req.Name == "" {
		return nil, status.Error(codes.InvalidArgument, "name is required")
	}

	rel, err := s.releaseAtRevision(ctx, req.Namespace, req.Name, int(req.GetRevision()))
	if err != nil {
		return nil, err
	}

	out := &generated.RevealReleaseValuesResponse{Status: &generated.Status{Code: int32(codes.OK)}}
	if len(rel.Values) > 0 {
		b, err := json.Marshal(rel.Values)
		if err != nil {
			return nil, status.Errorf(codes.Internal, "encode values: %v", err)
		}
		out.ValuesJson = string(b)
	}
	return out, nil
}

// releaseAtRevision resolves a release by name, optionally pinned to a
// historical revision. revision <= 0 means the current record.
func (s *ReleaseService) releaseAtRevision(ctx context.Context, namespace, name string, revision int) (*types.Release, error) {
	if revision <= 0 {
		rel, err := s.repo.GetByName(ctx, namespace, name)
		if err != nil {
			return nil, status.Errorf(codes.NotFound, "get release: %v", err)
		}
		return rel, nil
	}

	ref := string(types.ResourceTypeRelease) + ":" + name + "." + namespace + ".rune"
	versions, err := s.repo.History(ctx, ref)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "history: %v", err)
	}
	for _, v := range versions {
		rel, ok := historicalToRelease(v)
		if !ok {
			continue
		}
		if rel.Revision == revision {
			return rel, nil
		}
	}
	return nil, status.Errorf(codes.NotFound, "release %s/%s has no revision %d", namespace, name, revision)
}

// DeleteRelease uninstalls a release (soft by default, --purge to forget — D4).
func (s *ReleaseService) DeleteRelease(ctx context.Context, req *generated.DeleteReleaseRequest) (*generated.Status, error) {
	if req == nil || req.Name == "" {
		return nil, status.Error(codes.InvalidArgument, "name is required")
	}
	err := s.ctl.Uninstall(ctx, req.Namespace, req.Name, releasectl.UninstallOptions{
		KeepVolumes: req.GetKeepVolumes(),
		Purge:       req.GetPurge(),
	})
	if err != nil {
		return nil, status.Errorf(codes.Internal, "uninstall: %v", err)
	}
	return &generated.Status{Code: int32(codes.OK)}, nil
}

// Rollback rolls a release forward to a prior revision.
//
// Rollback is driven CLIENT-SIDE (see pkg/cli/cmd/cast_rollback.go): because
// rendering is client-side (C1), the CLI re-materializes the historical
// revision's reproducible source, re-renders it with the stored values, and
// casts it forward through the normal Cast RPC. The server therefore does not
// re-render here; this RPC remains Unimplemented as a deliberate signal that
// rollback orchestration lives in the client, not the server.
func (s *ReleaseService) Rollback(ctx context.Context, req *generated.RollbackReleaseRequest) (*generated.ReleaseResponse, error) {
	return nil, status.Error(codes.Unimplemented,
		"release rollback is driven client-side (rune release rollback); the server does not re-render historical sources")
}

// --- conversions ---

func protoToSpec(p *generated.ReleaseSpec) (release.ReleaseSpec, error) {
	spec := release.ReleaseSpec{
		Name:           p.GetName(),
		Namespace:      p.GetNamespace(),
		Source:         protoToSource(p.GetSource()),
		RenderedDigest: p.GetRenderedDigest(),
		Options:        release.Options{Adopt: p.GetAdopt()},
		Detach:         p.GetDetach(),
		Atomic:         p.GetAtomic(),
	}
	mf, err := protoToManifest(p.GetManifest())
	if err != nil {
		return spec, err
	}
	spec.Manifest = mf
	if vj := p.GetValuesJson(); vj != "" {
		if err := json.Unmarshal([]byte(vj), &spec.Values); err != nil {
			return spec, err
		}
	}
	for _, ref := range p.GetResources() {
		spec.Resources = append(spec.Resources, release.DesiredResource{Ref: protoToOwnerRef(ref)})
	}
	return spec, nil
}

func protoToPayloads(p *generated.RenderedPayloads) (releasectl.Payloads, error) {
	out := releasectl.Payloads{
		Services:   map[string]*types.Service{},
		Secrets:    map[string]*types.Secret{},
		Configmaps: map[string]*types.Configmap{},
		Volumes:    map[string]*types.Volume{},
	}
	if p == nil {
		return out, nil
	}
	for k, v := range p.GetServices() {
		var svc types.Service
		if err := json.Unmarshal([]byte(v), &svc); err != nil {
			return out, err
		}
		out.Services[k] = &svc
	}
	for k, v := range p.GetSecrets() {
		// Symmetric to the client's MarshalSecretPayload: restores Data, which
		// plain unmarshaling drops (json:"-").
		sec, err := types.UnmarshalSecretPayload([]byte(v))
		if err != nil {
			return out, err
		}
		out.Secrets[k] = sec
	}
	for k, v := range p.GetConfigmaps() {
		var cm types.Configmap
		if err := json.Unmarshal([]byte(v), &cm); err != nil {
			return out, err
		}
		out.Configmaps[k] = &cm
	}
	for k, v := range p.GetVolumes() {
		var vol types.Volume
		if err := json.Unmarshal([]byte(v), &vol); err != nil {
			return out, err
		}
		out.Volumes[k] = &vol
	}
	return out, nil
}

func protoToSource(p *generated.ReleaseSource) types.ReleaseSource {
	if p == nil {
		return types.ReleaseSource{}
	}
	return types.ReleaseSource{
		Type:     types.RunesetSourceType(p.GetType()),
		Location: p.GetLocation(),
		Ref:      p.GetRef(),
		Digest:   p.GetDigest(),
	}
}

func protoToManifest(p *generated.RunesetManifest) (types.RunesetManifest, error) {
	mf := types.RunesetManifest{}
	if p == nil {
		return mf, nil
	}
	mf.Name = p.GetName()
	mf.Version = p.GetVersion()
	mf.Description = p.GetDescription()
	mf.Namespace = p.GetNamespace()
	if dj := p.GetDefaultsJson(); dj != "" {
		if err := json.Unmarshal([]byte(dj), &mf.Defaults); err != nil {
			return mf, err
		}
	}
	return mf, nil
}

func protoToOwnerRef(p *generated.OwnerRef) types.OwnerRef {
	return types.OwnerRef{
		ResourceType: types.ResourceType(p.GetResourceType()),
		Namespace:    p.GetNamespace(),
		Name:         p.GetName(),
	}
}

func toProtoRelease(r *types.Release) *generated.Release {
	if r == nil {
		return nil
	}
	out := &generated.Release{
		Id:             r.ID,
		Name:           r.Name,
		Namespace:      r.Namespace,
		Status:         string(r.Status),
		Revision:       int32(r.Revision), //nolint:gosec // revision is a small monotonic counter
		Source:         toProtoSource(r.Source),
		Manifest:       toProtoManifest(r.Manifest),
		RenderedDigest: r.RenderedDigest,
		CreatedAt:      r.CreatedAt.Format(time.RFC3339),
		UpdatedAt:      r.UpdatedAt.Format(time.RFC3339),
	}
	out.HasValues = len(r.Values) > 0
	for _, ref := range r.Owns {
		out.Owns = append(out.Owns, toProtoOwnerRef(ref))
	}
	for _, ref := range r.References {
		out.References = append(out.References, toProtoOwnerRef(ref))
	}
	return out
}

func toProtoSource(s types.ReleaseSource) *generated.ReleaseSource {
	return &generated.ReleaseSource{
		Type:     string(s.Type),
		Location: s.Location,
		Ref:      s.Ref,
		Digest:   s.Digest,
	}
}

func toProtoManifest(m types.RunesetManifest) *generated.RunesetManifest {
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

func toProtoOwnerRef(r types.OwnerRef) *generated.OwnerRef {
	return &generated.OwnerRef{
		ResourceType: string(r.ResourceType),
		Namespace:    r.Namespace,
		Name:         r.Name,
	}
}

func toProtoPlan(p *release.Plan) *generated.Plan {
	if p == nil {
		return nil
	}
	out := &generated.Plan{
		Release:   p.Release,
		Namespace: p.Namespace,
		Applyable: p.Applyable(),
	}
	for i := range p.Changes {
		ch := &p.Changes[i]
		pc := &generated.PlannedChange{
			Ref:    toProtoOwnerRef(ch.Ref),
			Action: toProtoAction(ch.Action),
		}
		if ch.Conflict != nil {
			pc.ConflictReason = ch.Conflict.Reason
		}
		out.Changes = append(out.Changes, pc)
	}
	return out
}

func toProtoAction(a release.Action) generated.Action {
	switch a {
	case release.ActionCreate:
		return generated.Action_ACTION_CREATE
	case release.ActionUpdate:
		return generated.Action_ACTION_UPDATE
	case release.ActionPrune:
		return generated.Action_ACTION_PRUNE
	case release.ActionAdopt:
		return generated.Action_ACTION_ADOPT
	case release.ActionReference:
		return generated.Action_ACTION_REFERENCE
	default:
		return generated.Action_ACTION_UNSPECIFIED
	}
}

// historicalToRelease coerces a stored historical version back into a Release.
//
// The real store (BadgerStore) JSON-marshals each version's resource into an
// envelope and decodes it back into an interface{} — i.e. a
// map[string]interface{}, never a typed *types.Release. The typed cases below
// only match the in-memory fakes used by some unit tests; against the real
// store every record falls through to the JSON round-trip, which is what
// recovers the typed shape (mirrors historyToConfigmap / historyToStored).
func historicalToRelease(v store.HistoricalVersion) (*types.Release, bool) {
	switch r := v.Resource.(type) {
	case *types.Release:
		return r, r != nil
	case types.Release:
		return &r, true
	}
	b, err := json.Marshal(v.Resource)
	if err != nil {
		return nil, false
	}
	var rel types.Release
	if err := json.Unmarshal(b, &rel); err != nil {
		return nil, false
	}
	return &rel, true
}
