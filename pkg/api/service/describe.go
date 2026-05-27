package service

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/runestack/rune/pkg/api/generated"
	"github.com/runestack/rune/pkg/events"
	"github.com/runestack/rune/pkg/log"
	"github.com/runestack/rune/pkg/store"
	"github.com/runestack/rune/pkg/types"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// defaultDescribeEventLimit caps the Events block on a describe result.
const defaultDescribeEventLimit = 20

// errDescribeNotFound is the sentinel a per-kind collector returns when
// the target resource itself does not exist. The Describe RPC maps it
// to codes.NotFound. Reference-walk misses are never fatal — they are
// captured inside the result instead (DescribeReference.Unresolved).
var errDescribeNotFound = errors.New("describe target not found")

// DescribeService implements the gRPC DescribeService (RUNE-126).
//
// It assembles the one-shot diagnostic view for a single resource
// server-side: the target is read, its obvious references are walked
// one level deep against the same store, and the assembled
// DescribeResult is returned. A consistent snapshot under one read
// pass, one authz check — see _docs/designs/RUNE-126-Describe-Command.md.
type DescribeService struct {
	generated.UnimplementedDescribeServiceServer

	store    store.Store
	eventLog events.EventLog
	logger   log.Logger
}

// NewDescribeService constructs a DescribeService over the given store.
// eventLog is optional (nil-safe): when set, describe folds the most
// recent events for the target resource into the result (RUNE-126
// Phase 2). Tests that don't care about events pass nil.
func NewDescribeService(st store.Store, eventLog events.EventLog, logger log.Logger) *DescribeService {
	return &DescribeService{
		store:    st,
		eventLog: eventLog,
		logger:   logger.WithComponent("describe-service"),
	}
}

// Describe returns the assembled diagnostic view for one resource.
func (s *DescribeService) Describe(ctx context.Context, req *generated.DescribeRequest) (*generated.DescribeResponse, error) {
	if req.Name == "" {
		return nil, status.Error(codes.InvalidArgument, "resource name is required")
	}
	kind := strings.ToLower(strings.TrimSpace(req.Kind))
	ns := req.Namespace
	if ns == "" && kind != "node" {
		ns = DefaultNamespace
	}

	var (
		result *generated.DescribeResult
		err    error
	)
	switch kind {
	case "instance":
		result, err = s.describeInstance(ctx, ns, req.Name)
	case "service":
		result, err = s.describeService(ctx, ns, req.Name)
	case "volume":
		result, err = s.describeVolume(ctx, ns, req.Name)
	case "node":
		// RUNE-126 follow-up: nodes are not persisted store resources
		// on single-node, so there is nothing to describe yet.
		return nil, status.Error(codes.Unimplemented,
			"describe node is not available on single-node yet (RUNE-126 follow-up)")
	default:
		return nil, status.Errorf(codes.InvalidArgument, "cannot describe %q", req.Kind)
	}
	if err != nil {
		if errors.Is(err, errDescribeNotFound) || store.IsNotFoundError(err) {
			return nil, status.Errorf(codes.NotFound, "%s %q not found", kind, req.Name)
		}
		return nil, status.Errorf(codes.Internal, "describe %s: %v", kind, err)
	}
	return &generated.DescribeResponse{
		Result: result,
		Status: &generated.Status{Code: int32(codes.OK)},
	}, nil
}

// --- per-kind collectors -------------------------------------------------

func (s *DescribeService) describeInstance(ctx context.Context, ns, name string) (*generated.DescribeResult, error) {
	var instances []types.Instance
	if err := s.store.List(ctx, types.ResourceTypeInstance, ns, &instances); err != nil {
		return nil, err
	}
	// A logical instance slot (e.g. "flo-0") can have several records:
	// the live one plus Deleted tombstones from prior incarnations.
	// Describe the live record; fall back to the most recent tombstone
	// only when no live record exists.
	var inst *types.Instance
	for i := range instances {
		if instances[i].Name != name {
			continue
		}
		if inst == nil || betterDescribeInstance(&instances[i], inst) {
			inst = &instances[i]
		}
	}
	if inst == nil {
		return nil, fmt.Errorf("instance %s/%s: %w", ns, name, errDescribeNotFound)
	}

	res := &generated.DescribeResult{
		Kind:      "Instance",
		Name:      inst.Name,
		Namespace: inst.Namespace,
		Status:    string(inst.Status),
		Reason:    inst.StatusReason,
		Message:   inst.StatusMessage,
	}

	svcLabel := inst.ServiceName
	if inst.Metadata != nil && inst.Metadata.ServiceGeneration > 0 {
		svcLabel = fmt.Sprintf("%s (generation %d)", inst.ServiceName, inst.Metadata.ServiceGeneration)
	}
	res.Identity = []*generated.DescribeKV{
		kv("ID", inst.ID),
		kv("Service", svcLabel),
		kv("Node", emptyDashStr(inst.NodeID)),
		kv("Runner", string(inst.Runner)),
	}
	res.Timestamps = timestampKVs(inst.CreatedAt, inst.LastTransitionAt, inst.UpdatedAt)

	// Resources section.
	if inst.Resources != nil {
		var lines []string
		if l := resourceLine("CPU", inst.Resources.CPU); l != "" {
			lines = append(lines, l)
		}
		if l := resourceLine("Memory", inst.Resources.Memory); l != "" {
			lines = append(lines, l)
		}
		if len(lines) > 0 {
			res.Sections = append(res.Sections, &generated.DescribeSection{Title: "Resources", Lines: lines})
		}
	}

	// Volume mount reference walk.
	if inst.Metadata != nil && len(inst.Metadata.VolumeMounts) > 0 {
		mounts := append([]types.ResolvedVolumeMount(nil), inst.Metadata.VolumeMounts...)
		sort.Slice(mounts, func(i, j int) bool { return mounts[i].Name < mounts[j].Name })
		for _, vm := range mounts {
			vns := vm.VolumeNamespace
			if vns == "" {
				vns = inst.Namespace
			}
			ref := &generated.DescribeReference{
				Relation:  "volumeMount",
				Kind:      "volume",
				Name:      vm.VolumeName,
				Namespace: vns,
				Detail:    fmt.Sprintf("mount %q at %s", vm.Name, vm.MountPath),
			}
			var vol types.Volume
			if err := s.store.Get(ctx, types.ResourceTypeVolume, vns, vm.VolumeName, &vol); err != nil {
				ref.Unresolved = true
				ref.Detail = fmt.Sprintf("mount %q at %s [unresolved: %v]", vm.Name, vm.MountPath, err)
			} else {
				ref.Status = string(vol.Status)
				ref.StatusReason = vol.StatusReason
			}
			res.References = append(res.References, ref)
		}
	}

	// Parent service reference.
	if inst.ServiceName != "" {
		ref := &generated.DescribeReference{
			Relation: "service", Kind: "service", Name: inst.ServiceName, Namespace: inst.Namespace,
		}
		var svc types.Service
		if err := s.store.Get(ctx, types.ResourceTypeService, inst.Namespace, inst.ServiceName, &svc); err != nil {
			ref.Unresolved = true
			ref.Detail = fmt.Sprintf("[unresolved: %v]", err)
		} else {
			ref.Status = string(svc.Status)
			ref.StatusReason = svc.StatusReason
		}
		res.References = append(res.References, ref)
	}

	res.Hints = []string{fmt.Sprintf("rune logs %s -n %s --tail=50", inst.Name, inst.Namespace)}
	s.attachEvents(ctx, res, inst.Namespace, "Instance", inst.Name)
	return res, nil
}

func (s *DescribeService) describeService(ctx context.Context, ns, name string) (*generated.DescribeResult, error) {
	var svc types.Service
	if err := s.store.Get(ctx, types.ResourceTypeService, ns, name, &svc); err != nil {
		return nil, err
	}

	res := &generated.DescribeResult{
		Kind:      "Service",
		Name:      svc.Name,
		Namespace: svc.Namespace,
		Status:    string(svc.Status),
		Reason:    svc.StatusReason,
		Message:   svc.StatusMessage,
	}
	res.Identity = []*generated.DescribeKV{
		kv("ID", svc.ID),
		kv("Image", svc.Image),
		kv("Scale", fmt.Sprintf("%d", svc.Scale)),
	}
	if svc.Metadata != nil {
		res.Identity = append(res.Identity,
			kv("Generation", fmt.Sprintf("%d", svc.Metadata.Generation)))
		res.Timestamps = timestampKVs(svc.Metadata.CreatedAt, nil, svc.Metadata.UpdatedAt)
	}

	// Child instances: replica rollup + per-instance lines + references.
	var instances []types.Instance
	if err := s.store.List(ctx, types.ResourceTypeInstance, ns, &instances); err != nil {
		return nil, err
	}
	// Exclude Deleted tombstones — they are GC'd prior incarnations,
	// not part of the service's live replica set. Failed/Stalled
	// records are kept: they are debugging signal.
	mine := make([]types.Instance, 0, len(instances))
	for _, in := range instances {
		if in.ServiceName == svc.Name && in.Status != types.InstanceStatusDeleted {
			mine = append(mine, in)
		}
	}
	sort.Slice(mine, func(i, j int) bool { return mine[i].Name < mine[j].Name })

	byStatus := map[types.InstanceStatus]int{}
	ready := 0
	for _, in := range mine {
		byStatus[in.Status]++
		if in.Status == types.InstanceStatusRunning {
			ready++
		}
	}
	res.Identity = append(res.Identity,
		kv("Replicas", fmt.Sprintf("desired=%d ready=%d total=%d", svc.Scale, ready, len(mine))))

	for _, in := range mine {
		res.References = append(res.References, &generated.DescribeReference{
			Relation: "instance", Kind: "instance", Name: in.Name, Namespace: in.Namespace,
			Status: string(in.Status), StatusReason: in.StatusReason,
			Detail: in.StatusMessage,
		})
	}

	res.Hints = []string{fmt.Sprintf("rune logs %s -n %s --tail=50", svc.Name, svc.Namespace)}
	s.attachEvents(ctx, res, svc.Namespace, "Service", svc.Name)
	return res, nil
}

func (s *DescribeService) describeVolume(ctx context.Context, ns, name string) (*generated.DescribeResult, error) {
	var vol types.Volume
	if err := s.store.Get(ctx, types.ResourceTypeVolume, ns, name, &vol); err != nil {
		return nil, err
	}

	res := &generated.DescribeResult{
		Kind:      "Volume",
		Name:      vol.Name,
		Namespace: vol.Namespace,
		Status:    string(vol.Status),
		Reason:    vol.StatusReason,
		Message:   vol.Message,
	}
	res.Identity = []*generated.DescribeKV{
		kv("ID", vol.ID),
		kv("Class", vol.StorageClassName),
		kv("Size", vol.Size),
		kv("Access", string(vol.AccessMode)),
		kv("Bound to", emptyDashStr(vol.BoundClaim)),
		kv("Bound node", emptyDashStr(vol.BoundNode)),
		kv("Handle", emptyDashStr(vol.Handle)),
	}
	res.Timestamps = timestampKVs(vol.CreatedAt, nil, vol.UpdatedAt)

	// Driver parameters section. Controller-owned DriverParameters is the
	// authoritative snapshot; fall back to the user-supplied Parameters.
	params := vol.DriverParameters
	if len(params) == 0 {
		params = vol.Parameters
	}
	if len(params) > 0 {
		keys := make([]string, 0, len(params))
		for k := range params {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		var lines []string
		for _, k := range keys {
			v := params[k]
			lines = append(lines, fmt.Sprintf("%s: %s", k, v)+s.resolveSecretRefMark(ctx, vol.Namespace, v))
		}
		res.Sections = append(res.Sections, &generated.DescribeSection{Title: "Driver parameters", Lines: lines})
	}

	// StorageClass reference.
	if vol.StorageClassName != "" {
		ref := &generated.DescribeReference{
			Relation: "storageClass", Kind: "storageclass", Name: vol.StorageClassName,
		}
		var sc types.StorageClass
		if err := s.store.Get(ctx, types.ResourceTypeStorageClass, "", vol.StorageClassName, &sc); err != nil {
			ref.Unresolved = true
			ref.Detail = fmt.Sprintf("[unresolved: %v]", err)
		} else {
			ref.Status = "exists"
			ref.Detail = "driver=" + sc.Driver
		}
		res.References = append(res.References, ref)
	}

	res.Hints = []string{fmt.Sprintf("rune get volume %s -n %s -o yaml", vol.Name, vol.Namespace)}
	s.attachEvents(ctx, res, vol.Namespace, "Volume", vol.Name)
	return res, nil
}

// attachEvents folds the most recent events for the target into
// result.Events. Nil-safe (skips when no EventLog is wired); fold
// failures are logged and dropped — events are observability, never
// on the correctness path.
func (s *DescribeService) attachEvents(ctx context.Context, res *generated.DescribeResult, ns, kind, name string) {
	if s.eventLog == nil {
		return
	}
	evs, err := s.eventLog.ListByResource(ctx, ns, kind, name, defaultDescribeEventLimit)
	if err != nil {
		s.logger.Warn("describe: list events failed",
			log.Str("kind", kind), log.Str("name", name), log.Err(err))
		return
	}
	for _, e := range evs {
		res.Events = append(res.Events, &generated.DescribeEvent{
			Timestamp: e.LastSeen.UTC().Format(time.RFC3339),
			Level:     string(e.Level),
			Message:   formatEventMessage(e),
		})
	}
}

// formatEventMessage builds the human-facing line for a describe
// event — includes the fold count when >1 so an operator sees the
// retry rate at a glance.
func formatEventMessage(e types.Event) string {
	if e.Count > 1 {
		return fmt.Sprintf("%s (×%d) %s", e.Reason, e.Count, e.Message)
	}
	if e.Reason != "" {
		return e.Reason + " — " + e.Message
	}
	return e.Message
}

// resolveSecretRefMark returns a " ✓ resolved" / " ✗ missing" suffix when
// the parameter value is a Rune secret reference, or "" otherwise. The
// secret's *value* is never read — only its existence is probed.
func (s *DescribeService) resolveSecretRefMark(ctx context.Context, ns, value string) string {
	if !strings.HasPrefix(value, "secret:") {
		return ""
	}
	ref, err := types.ParseResourceRefWithDefaultNamespace(value, ns)
	if err != nil || ref.Name == "" {
		return "  ✗ unparseable secret ref"
	}
	refNS := ref.Namespace
	if refNS == "" {
		refNS = ns
	}
	var sec types.Secret
	if err := s.store.Get(ctx, types.ResourceTypeSecret, refNS, ref.Name, &sec); err != nil {
		return "  ✗ missing"
	}
	return "  ✓ resolved"
}

// --- small helpers -------------------------------------------------------

func kv(k, v string) *generated.DescribeKV { return &generated.DescribeKV{Key: k, Value: v} }

// betterDescribeInstance reports whether instance a is the better
// describe target than b for the same logical slot: a live record
// beats a Deleted tombstone, and among equals the most recently
// updated wins.
func betterDescribeInstance(a, b *types.Instance) bool {
	liveA := a.Status != types.InstanceStatusDeleted
	liveB := b.Status != types.InstanceStatusDeleted
	if liveA != liveB {
		return liveA
	}
	return a.UpdatedAt.After(b.UpdatedAt)
}

func emptyDashStr(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// timestampKVs builds the Created / LastTransition / LastUpdated block,
// skipping zero values. transition may be nil.
func timestampKVs(created time.Time, transition *time.Time, updated time.Time) []*generated.DescribeKV {
	var out []*generated.DescribeKV
	if !created.IsZero() {
		out = append(out, kv("Created", created.UTC().Format(time.RFC3339)))
	}
	if transition != nil && !transition.IsZero() {
		out = append(out, kv("Last transition", transition.UTC().Format(time.RFC3339)))
	}
	if !updated.IsZero() {
		out = append(out, kv("Last updated", updated.UTC().Format(time.RFC3339)))
	}
	return out
}

// resourceLine renders one "request X limit Y" line, or "" when unset.
func resourceLine(label string, rl types.ResourceLimit) string {
	if rl.Request == "" && rl.Limit == "" {
		return ""
	}
	parts := []string{label + ":"}
	if rl.Request != "" {
		parts = append(parts, "request "+rl.Request)
	}
	if rl.Limit != "" {
		parts = append(parts, "limit "+rl.Limit)
	}
	return strings.Join(parts, " ")
}
