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
	"github.com/runestack/rune/pkg/store/repos"
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
// pass, one authz check (RUNE-126).
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
		result, err = s.describeNode(ctx, req.Name)
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
		kv("IP", emptyDashStr(instanceDisplayIP(inst))),
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
		// Template vs observed generation is how an operator tells "the spec
		// changed" from "the reconciler has caught up" — surfaced only when
		// they disagree with Generation, to keep the common case quiet.
		if svc.Metadata.TemplateGeneration != 0 && svc.Metadata.TemplateGeneration != svc.Metadata.Generation {
			res.Identity = append(res.Identity,
				kv("Template generation", fmt.Sprintf("%d", svc.Metadata.TemplateGeneration)))
		}
		if svc.Metadata.ObservedGeneration != svc.Metadata.Generation {
			res.Identity = append(res.Identity,
				kv("Observed generation", fmt.Sprintf("%d (reconciling)", svc.Metadata.ObservedGeneration)))
		}
		res.Timestamps = timestampKVs(svc.Metadata.CreatedAt, nil, svc.Metadata.UpdatedAt)
	}

	// In-flight update (RUNE-042). Two fractions in operator words, plus the
	// planner's sentence — not the four-counter block, which is proto/dashboard
	// detail. Absent entirely when no update is running, so a steady service
	// reads exactly as it did before.
	if u := svc.Update; u != nil {
		res.Identity = append(res.Identity,
			kv("Update", describeUpdate(u)))
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

	if sec := serviceDiscoverySection(&svc); sec != nil {
		res.Sections = append(res.Sections, sec)
	}

	res.Hints = []string{fmt.Sprintf("rune logs %s -n %s --tail=50", svc.Name, svc.Namespace)}
	s.attachEvents(ctx, res, svc.Namespace, "Service", svc.Name)
	return res, nil
}

// serviceDiscoverySection renders how a service is reached, both east-west
// (in-cluster) and north-south (external ingress):
//
//   - Cluster: VIP + DNS name (<svc>.<ns>.rune) on the declared ports; the
//     dataplane proxies the VIP to a healthy instance. hostPort ports also
//     show the dev loopback form (127.0.0.1:<hostPort>).
//   - External: the ingress URL (the same thing `rune get ingress` shows),
//     present only when the service has spec.expose.host set.
//
// Returns nil when there's nothing useful to show, so headless/degenerate
// services don't get an empty block.
func serviceDiscoverySection(svc *types.Service) *generated.DescribeSection {
	vip := ""
	mode := ""
	if svc.Discovery != nil {
		vip = svc.Discovery.VIP
		mode = svc.Discovery.Mode
	}
	exposed := svc.Expose != nil && svc.Expose.Host != ""
	if vip == "" && len(svc.Ports) == 0 && !exposed {
		return nil
	}

	clusterDNS := fmt.Sprintf("%s.%s.rune", svc.Name, svc.Namespace)

	vipStr := vip
	if vipStr == "" {
		vipStr = "(not allocated)"
	}

	lines := []string{
		fmt.Sprintf("%-13s %s", "VIP:", vipStr),
		fmt.Sprintf("%-13s %s", "Cluster DNS:", clusterDNS),
	}
	if mode != "" {
		lines = append(lines, fmt.Sprintf("%-13s %s", "Mode:", mode))
	}

	if len(svc.Ports) > 0 {
		lines = append(lines, "Endpoints:")
		for _, p := range svc.Ports {
			ep := fmt.Sprintf("  %s:%d", clusterDNS, p.Port)
			if p.Name != "" {
				ep += fmt.Sprintf(" (%s)", p.Name)
			}
			// hostPort is the dev-mode escape hatch (macOS Docker Desktop):
			// the same container port is also published on host loopback.
			if p.HostPort > 0 {
				ep += fmt.Sprintf("  [dev: 127.0.0.1:%d]", p.HostPort)
			}
			lines = append(lines, ep)
		}
	}

	if exposed {
		lines = append(lines, fmt.Sprintf("%-13s %s", "External:", serviceExternalEndpoint(svc)))
	}

	return &generated.DescribeSection{Title: "Discovery", Lines: lines}
}

// serviceExternalEndpoint builds the ingress URL line shown under
// Discovery → External. Scheme is https when TLS is configured, http
// otherwise. The TLS mode and cert state are appended in brackets so a
// failing cert is visible without running `rune get ingress`.
func serviceExternalEndpoint(svc *types.Service) string {
	scheme := "http"
	tlsMode := ""
	if svc.Expose.TLS != nil {
		scheme = "https"
		switch {
		case svc.Expose.TLS.IsACME():
			tlsMode = types.ExposeTLSModeACME
		case svc.Expose.TLS.Secret != "":
			tlsMode = types.ExposeTLSModeManual
		}
	}
	url := fmt.Sprintf("%s://%s%s", scheme, svc.Expose.Host, svc.Expose.Path)

	var annotations []string
	if tlsMode != "" {
		annotations = append(annotations, "TLS: "+tlsMode)
	}
	if svc.IngressCert != nil && svc.IngressCert.State != "" {
		annotations = append(annotations, "cert: "+string(svc.IngressCert.State))
	}
	if len(annotations) > 0 {
		url += "  [" + strings.Join(annotations, ", ") + "]"
	}
	return url
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

// describeNode renders one node's inventory record.
//
// The record is cluster-scoped, so Describe passes no namespace and the
// events are read under the empty-namespace key the record is stored
// under.
func (s *DescribeService) describeNode(ctx context.Context, name string) (*generated.DescribeResult, error) {
	repo := repos.NewNodeRepo(s.store)
	node, err := s.resolveNode(ctx, repo, name)
	if err != nil {
		return nil, err
	}

	res := &generated.DescribeResult{
		Kind: "Node",
		Name: node.Name,
		// Status and Reason are DELIBERATELY LEFT EMPTY. Nothing
		// refreshes types.Node.Status or LastHeartbeat on a cadence, so
		// rendering them would show a blank status and a heartbeat
		// frozen at the last restart — which reads as a dead node on a
		// perfectly healthy box, and would be the first thing every
		// operator sees from this command. An unmaintained field is
		// worse than an absent one.
	}
	res.Identity = []*generated.DescribeKV{
		kv("ID", node.ID),
		kv("Address", node.Address),
	}
	if cap := nodeCapacityLine(node); cap != "" {
		res.Identity = append(res.Identity, kv("Capacity", cap))
	}
	res.Timestamps = timestampKVs(node.CreatedAt, nil, time.Time{})

	if len(node.Labels) > 0 {
		res.Sections = append(res.Sections, &generated.DescribeSection{
			Title: "Labels",
			Lines: sortedKeyValueLines(node.Labels),
		})
	}
	if sec := nodeDevicesSection(node); sec != nil {
		res.Sections = append(res.Sections, sec)
	}

	res.Hints = []string{fmt.Sprintf("rune get events --for node/%s", node.Name)}
	// The "" namespace is the cluster-scoped key the record is stored
	// under; without this the node's event trail is write-only.
	s.attachEvents(ctx, res, "", "Node", node.Name)
	return res, nil
}

// resolveNode looks the record up by its store key, then falls back to a
// scan matching ID or Name. `local` resolves to the single node on a
// one-node install: it is the literal every instance used to carry, and
// there is no node-listing command yet, so an operator has no other way
// to discover the minted ID.
func (s *DescribeService) resolveNode(ctx context.Context, repo *repos.NodeRepo, name string) (*types.Node, error) {
	if node, err := repo.Get(ctx, name); err == nil {
		return node, nil
	} else if !store.IsNotFoundError(err) {
		return nil, err
	}

	nodes, err := repo.List(ctx)
	if err != nil {
		return nil, err
	}
	for _, n := range nodes {
		if strings.EqualFold(n.ID, name) || strings.EqualFold(n.Name, name) {
			return n, nil
		}
	}
	if strings.EqualFold(name, types.LocalNodeIDFallback) && len(nodes) == 1 {
		return nodes[0], nil
	}
	return nil, fmt.Errorf("node %q: %w", name, errDescribeNotFound)
}

// nodeCapacityLine renders the node's CPU and memory, or "" when the
// agent could not determine them — an absent line beats a confident zero.
func nodeCapacityLine(node *types.Node) string {
	var parts []string
	if node.Resources.CPU > 0 {
		parts = append(parts, fmt.Sprintf("%.3g CPU", float64(node.Resources.CPU)/1000))
	}
	if node.Resources.Memory > 0 {
		parts = append(parts, types.FormatMemory(node.Resources.Memory)+" memory")
	}
	return strings.Join(parts, ", ")
}

// nodeDevicesSection renders the GPU block, or nil when this node has
// nothing to say about devices.
//
// Nil, not a "GPUs: none" line: a GPU-less box must not have a feature
// announcing itself to someone who did not ask for it. The three states
// behind "no GPUs" are distinct and only two of them produce output —
// never probed, and probe failed.
func nodeDevicesSection(node *types.Node) *generated.DescribeSection {
	switch {
	case node.DevicesProbedAt == nil:
		// Not an error, and specifically NOT "this node has no GPUs" —
		// the agent starts after the control plane, so this is the
		// window where the answer is not known yet.
		return &generated.DescribeSection{
			Title: "GPUs",
			Lines: []string{"not probed yet"},
		}
	case node.DeviceProbeError != "":
		// Quoted verbatim: without it, six distinct causes collapse
		// into one unactionable "no devices".
		return &generated.DescribeSection{
			Title: "GPUs",
			Lines: []string{"probe failed: " + node.DeviceProbeError},
		}
	case len(node.Devices) == 0:
		return nil
	}

	devices := append([]types.GPUDevice(nil), node.Devices...)
	sort.Slice(devices, func(i, j int) bool { return devices[i].Index < devices[j].Index })

	lines := make([]string, 0, len(devices))
	for _, d := range devices {
		line := fmt.Sprintf("%s  %s  %s", d.UUID, d.Product, humanGiB(d.VRAMBytes))
		if d.DriverVersion != "" {
			line += "  driver " + d.DriverVersion
		}
		if d.CUDAVersion != "" {
			line += "  CUDA " + d.CUDAVersion
		}
		if d.Missing {
			line += "  [missing]"
		}
		lines = append(lines, line)
	}
	return &generated.DescribeSection{Title: "GPUs", Lines: lines}
}

// humanGiB renders device memory the way the driver and every GPU
// datasheet do: "48Gi", not types.FormatMemory's "48.0Gi". A tenth of a
// gibibyte is noise on a card and the whole-number form is what an
// operator is comparing against the box's spec sheet.
func humanGiB(b int64) string {
	if b <= 0 {
		return "-"
	}
	return fmt.Sprintf("%.0fGi", float64(b)/float64(1<<30))
}

func sortedKeyValueLines(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	lines := make([]string, 0, len(keys))
	for _, k := range keys {
		lines = append(lines, fmt.Sprintf("%s: %s", k, m[k]))
	}
	return lines
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

// instanceDisplayIP returns the instance's IP for display, falling back to
// Metadata.ContainerIP. The fallback covers instances written before the
// controller began persisting the top-level IP field (and any runner that
// only records ContainerIP) so describe shows a real address either way.
func instanceDisplayIP(inst *types.Instance) string {
	if inst.IP != "" {
		return inst.IP
	}
	if inst.Metadata != nil {
		return inst.Metadata.ContainerIP
	}
	return ""
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

// describeUpdate renders an in-flight update the way an operator thinks about
// it: how many replicas carry the new template, how many are serving right
// now, how long it has been going, and what it is waiting on.
//
// "replaced" and "serving" rather than "updated"/"available": the latter pair
// is accountant vocabulary that means nothing until you have read the design.
func describeUpdate(u *types.UpdateStatus) string {
	elapsed := ""
	if !u.StartedAt.IsZero() {
		elapsed = fmt.Sprintf(", %s elapsed", time.Since(u.StartedAt).Round(time.Second))
	}
	out := fmt.Sprintf("%d/%d replaced · %d/%d serving%s",
		u.UpdatedReady, u.Desired, u.Available, u.Desired, elapsed)
	if u.Message != "" {
		out += " — " + u.Message
	}
	return out
}
