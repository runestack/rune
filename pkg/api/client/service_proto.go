// Service/Instance <-> protobuf conversion for the API client. Kept apart
// from service_client.go so the RPC surface there stays readable.

package client

import (
	"fmt"
	"math"
	"time"

	"github.com/runestack/rune/pkg/api/generated"
	"github.com/runestack/rune/pkg/types"
	"github.com/runestack/rune/pkg/utils"
)

// Helper functions for converting between types.Service and generated.Service
// serviceToProto converts a types.Service to a generated.Service proto message.
func ServiceToProto(service *types.Service) *generated.Service {
	if service == nil {
		return nil
	}

	protoService := &generated.Service{
		Id:                 service.ID,
		Name:               service.Name,
		Namespace:          service.Namespace,
		Image:              service.Image,
		Command:            service.Command,
		Scale:              utils.ToInt32NonNegative(service.Scale),
		Runtime:            string(service.Runtime),
		ImagePull:          service.ImagePull,
		ImagePullAnonymous: service.ImagePullAnonymous,
		StatusReason:       service.StatusReason,
		StatusMessage:      service.StatusMessage,
		Labels:             service.Labels,
	}

	if service.Metadata != nil {
		protoService.Metadata = &generated.ServiceMetadata{
			Generation:         service.Metadata.Generation,
			CreatedAt:          service.Metadata.CreatedAt.Format(time.RFC3339),
			UpdatedAt:          service.Metadata.UpdatedAt.Format(time.RFC3339),
			LastNonZeroScale:   utils.ToInt32NonNegative(service.Metadata.LastNonZeroScale),
			TemplateGeneration: service.Metadata.TemplateGeneration,
			ObservedGeneration: service.Metadata.ObservedGeneration,
		}
	}

	// Update strategy + drain grace (RUNE-042). Both are optional on the
	// wire: an unset strategy means rolling, and drain_seconds 0 means "not
	// specified" so the server applies the default.
	if service.UpdateStrategy != nil {
		protoService.UpdateStrategy = &generated.UpdateStrategy{
			Type: string(service.UpdateStrategy.Type),
		}
		if service.UpdateStrategy.MinServing != nil {
			// Sentinel: proto 0 means unset, so an explicit 0 travels as -1.
			// minServing 0 is meaningful — "no availability requirement" — and
			// must not silently become the derived default.
			if *service.UpdateStrategy.MinServing == 0 {
				protoService.UpdateStrategy.MinServing = -1
			} else {
				protoService.UpdateStrategy.MinServing = utils.ToInt32NonNegative(*service.UpdateStrategy.MinServing)
			}
		}
	}
	if service.DrainSeconds != nil {
		// Sentinel: proto 0 means "unset", so an explicit 0 travels as -1.
		// Without this an operator's `drainSeconds: 0` arrives as absent and
		// silently becomes the 5s default rather than the 1s floor the
		// validation message promises.
		if *service.DrainSeconds == 0 {
			protoService.DrainSeconds = -1
		} else {
			protoService.DrainSeconds = utils.ToInt32NonNegative(*service.DrainSeconds)
		}
	}
	if service.Update != nil {
		protoService.Update = &generated.UpdateStatus{
			TemplateGeneration: service.Update.TemplateGeneration,
			Desired:            utils.ToInt32NonNegative(service.Update.Desired),
			Updated:            utils.ToInt32NonNegative(service.Update.Updated),
			UpdatedReady:       utils.ToInt32NonNegative(service.Update.UpdatedReady),
			Available:          utils.ToInt32NonNegative(service.Update.Available),
			Outdated:           utils.ToInt32NonNegative(service.Update.Outdated),
			StartedAt:          service.Update.StartedAt.Format(time.RFC3339),
			LastProgressAt:     service.Update.LastProgressAt.Format(time.RFC3339),
			Message:            service.Update.Message,
		}
	}

	// Convert args
	if len(service.Args) > 0 {
		protoService.Args = make([]string, len(service.Args))
		copy(protoService.Args, service.Args)
	}

	// Convert environment variables
	if len(service.Env) > 0 {
		protoService.Env = make(map[string]string)
		for k, v := range service.Env {
			protoService.Env[k] = v
		}
	}

	// Convert envFrom sources
	if len(service.EnvFrom) > 0 {
		protoService.EnvFrom = make([]*generated.EnvFromSource, 0, len(service.EnvFrom))
		for _, src := range service.EnvFrom {
			protoService.EnvFrom = append(protoService.EnvFrom, &generated.EnvFromSource{
				SecretName:    src.SecretName,
				ConfigmapName: src.ConfigmapName,
				Namespace:     src.Namespace,
				Prefix:        src.Prefix,
			})
		}
	}

	// Convert ports
	if len(service.Ports) > 0 {
		protoService.Ports = make([]*generated.ServicePort, len(service.Ports))
		for i, port := range service.Ports {
			protoService.Ports[i] = &generated.ServicePort{
				Name:       port.Name,
				Port:       utils.ToInt32NonNegative(port.Port),
				TargetPort: utils.ToInt32NonNegative(port.TargetPort),
				Protocol:   port.Protocol,
				HostPort:   utils.ToInt32NonNegative(port.HostPort),
			}
		}
	}

	// Convert expose
	if service.Expose != nil {
		protoService.Expose = &generated.ServiceExpose{
			Port:       service.Expose.Port,
			Host:       service.Expose.Host,
			Path:       service.Expose.Path,
			AllowCidrs: service.Expose.AllowCIDRs,
		}
		if service.Expose.TLS != nil {
			protoService.Expose.Tls = &generated.ExposeServiceTLS{
				Secret: service.Expose.TLS.Secret,
				Auto:   service.Expose.TLS.Auto,
				Mode:   service.Expose.TLS.Mode,
			}
		}
		if service.Expose.ClientCert != nil {
			protoService.Expose.ClientCert = &generated.ExposeClientCert{
				CaSecret: service.Expose.ClientCert.CASecret,
				Mode:     service.Expose.ClientCert.Mode,
			}
		}
	}

	// Convert resources
	if service.Resources != (types.Resources{}) {
		protoService.Resources = &generated.Resources{
			Cpu: &generated.ResourceLimit{
				Request: service.Resources.CPU.Request,
				Limit:   service.Resources.CPU.Limit,
			},
			Memory: &generated.ResourceLimit{
				Request: service.Resources.Memory.Request,
				Limit:   service.Resources.Memory.Limit,
			},
		}
		// Absent stays absent: a nil request and an empty one mean opposite
		// things — no GPU at all, versus one whole device.
		if g := service.Resources.GPU; g != nil {
			// Saturate rather than wrap. Admission refuses a count larger
			// than the node has devices either way, but a wrap could turn
			// an absurd request into an admissible one.
			count := g.Count
			if count > math.MaxInt32 {
				count = math.MaxInt32
			}
			protoService.Resources.Gpu = &generated.GPURequest{
				Count:              int32(count), //nolint:gosec // G115: saturated above
				Vram:               g.VRAM,
				AllowHeterogeneous: g.AllowHeterogeneous,
			}
		}
	}

	// Convert secret mounts
	if len(service.SecretMounts) > 0 {
		protoService.SecretMounts = make([]*generated.SecretMount, len(service.SecretMounts))
		for i, m := range service.SecretMounts {
			items := make([]*generated.KeyToPath, 0, len(m.Items))
			for _, it := range m.Items {
				items = append(items, &generated.KeyToPath{Key: it.Key, Path: it.Path})
			}
			protoService.SecretMounts[i] = &generated.SecretMount{
				Name:       m.Name,
				MountPath:  m.MountPath,
				SecretName: m.SecretName,
				Items:      items,
			}
		}
	}

	// Convert configmap mounts
	if len(service.ConfigmapMounts) > 0 {
		protoService.ConfigmapMounts = make([]*generated.ConfigmapMount, len(service.ConfigmapMounts))
		for i, m := range service.ConfigmapMounts {
			items := make([]*generated.KeyToPath, 0, len(m.Items))
			for _, it := range m.Items {
				items = append(items, &generated.KeyToPath{Key: it.Key, Path: it.Path})
			}
			protoService.ConfigmapMounts[i] = &generated.ConfigmapMount{
				Name:          m.Name,
				MountPath:     m.MountPath,
				ConfigmapName: m.ConfigmapName,
				Items:         items,
			}
		}
	}

	// Convert volume mounts (RUNE-070/072). Exactly one of claim /
	// claim_template is set per mount; binding state lives on the Volume
	// resource itself and is not transported on the Service.
	if len(service.Volumes) > 0 {
		protoService.Volumes = make([]*generated.VolumeMount, len(service.Volumes))
		for i, m := range service.Volumes {
			pv := &generated.VolumeMount{
				Name:      m.Name,
				MountPath: m.MountPath,
				ReadOnly:  m.ReadOnly,
				SubPath:   m.SubPath,
				FsMode:    m.FSMode,
			}
			if m.FSUser != nil {
				pv.FsUser = utils.ToInt32NonNegative(*m.FSUser)
				pv.FsUserSet = true
			}
			if m.FSGroup != nil {
				pv.FsGroup = utils.ToInt32NonNegative(*m.FSGroup)
				pv.FsGroupSet = true
			}
			if m.Claim != nil {
				pv.Claim = &generated.VolumeClaim{Name: m.Claim.Name}
			}
			if m.ClaimTemplate != nil {
				pv.ClaimTemplate = &generated.VolumeClaimTemplate{
					StorageClassName: m.ClaimTemplate.StorageClassName,
					Size:             m.ClaimTemplate.Size,
					AccessMode:       string(m.ClaimTemplate.AccessMode),
					Parameters:       m.ClaimTemplate.Parameters,
					ReclaimPolicy:    string(m.ClaimTemplate.ReclaimPolicy),
				}
			}
			protoService.Volumes[i] = pv
		}
	}

	// Convert status
	switch service.Status {
	case types.ServiceStatusPending:
		protoService.Status = generated.ServiceStatus_SERVICE_STATUS_PENDING
	case types.ServiceStatusRunning:
		protoService.Status = generated.ServiceStatus_SERVICE_STATUS_RUNNING
	case types.ServiceStatusDeploying:
		protoService.Status = generated.ServiceStatus_SERVICE_STATUS_UPDATING
	case types.ServiceStatusFailed:
		protoService.Status = generated.ServiceStatus_SERVICE_STATUS_FAILED
	default:
		protoService.Status = generated.ServiceStatus_SERVICE_STATUS_UNSPECIFIED
	}

	// Convert health checks
	if service.Health != nil {
		protoService.Health = &generated.HealthCheck{}

		if service.Health.Liveness != nil {
			protoService.Health.Liveness = &generated.Probe{
				InitialDelaySeconds: utils.ToInt32NonNegative(service.Health.Liveness.InitialDelaySeconds),
				PeriodSeconds:       utils.ToInt32NonNegative(service.Health.Liveness.IntervalSeconds),
				TimeoutSeconds:      utils.ToInt32NonNegative(service.Health.Liveness.TimeoutSeconds),
				Host:                service.Health.Liveness.Host,
			}

			switch service.Health.Liveness.Type {
			case "http":
				protoService.Health.Liveness.Type = generated.ProbeType_PROBE_TYPE_HTTP
				protoService.Health.Liveness.Path = service.Health.Liveness.Path
				protoService.Health.Liveness.Port = utils.ToInt32NonNegative(service.Health.Liveness.Port)
			case "tcp":
				protoService.Health.Liveness.Type = generated.ProbeType_PROBE_TYPE_TCP
				protoService.Health.Liveness.Port = utils.ToInt32NonNegative(service.Health.Liveness.Port)
			case "exec":
				protoService.Health.Liveness.Type = generated.ProbeType_PROBE_TYPE_EXEC
				protoService.Health.Liveness.Command = service.Health.Liveness.Command
			}
		}

		if service.Health.Readiness != nil {
			protoService.Health.Readiness = &generated.Probe{
				InitialDelaySeconds: utils.ToInt32NonNegative(service.Health.Readiness.InitialDelaySeconds),
				PeriodSeconds:       utils.ToInt32NonNegative(service.Health.Readiness.IntervalSeconds),
				TimeoutSeconds:      utils.ToInt32NonNegative(service.Health.Readiness.TimeoutSeconds),
				Host:                service.Health.Readiness.Host,
			}

			switch service.Health.Readiness.Type {
			case "http":
				protoService.Health.Readiness.Type = generated.ProbeType_PROBE_TYPE_HTTP
				protoService.Health.Readiness.Path = service.Health.Readiness.Path
				protoService.Health.Readiness.Port = utils.ToInt32NonNegative(service.Health.Readiness.Port)
			case "tcp":
				protoService.Health.Readiness.Type = generated.ProbeType_PROBE_TYPE_TCP
				protoService.Health.Readiness.Port = utils.ToInt32NonNegative(service.Health.Readiness.Port)
			case "exec":
				protoService.Health.Readiness.Type = generated.ProbeType_PROBE_TYPE_EXEC
				protoService.Health.Readiness.Command = service.Health.Readiness.Command
			}
		}
	}

	// Dependencies
	if len(service.Dependencies) > 0 {
		protoService.Dependencies = make([]*generated.DependencyRef, 0, len(service.Dependencies))
		for _, d := range service.Dependencies {
			protoService.Dependencies = append(protoService.Dependencies, &generated.DependencyRef{
				Namespace: d.Namespace,
				Service:   d.Service,
				Secret:    d.Secret,
				Configmap: d.Configmap,
			})
		}
	}

	// InitSteps (RUNE-121) and main-container SecurityContext.
	if len(service.InitSteps) > 0 {
		protoService.InitSteps = make([]*generated.InitStep, 0, len(service.InitSteps))
		for i := range service.InitSteps {
			protoService.InitSteps = append(protoService.InitSteps, initStepToProto(&service.InitSteps[i]))
		}
	}
	if service.SecurityContext != nil {
		protoService.SecurityContext = securityContextToProto(service.SecurityContext)
	}

	// Instances are populated by GetService / ListServices so callers
	// (notably `rune cast`'s readiness wait) can see backing-instance
	// state in a single round-trip. Read-only on the wire — Create/Update
	// ignore this field server-side.
	if len(service.Instances) > 0 {
		protoService.Instances = make([]*generated.Instance, 0, len(service.Instances))
		for i := range service.Instances {
			protoService.Instances = append(protoService.Instances, embeddedInstanceToProto(&service.Instances[i]))
		}
	}

	if service.Discovery != nil {
		protoService.Discovery = &generated.ServiceDiscovery{
			Vip:                service.Discovery.VIP,
			Mode:               service.Discovery.Mode,
			LocalityPreference: service.Discovery.LocalityPreference,
		}
	}

	if service.IngressCert != nil {
		ic := service.IngressCert
		protoService.IngressCert = &generated.IngressCertStatus{
			Host:      ic.Host,
			State:     string(ic.State),
			IssuedAt:  formatProtoTime(ic.IssuedAt),
			ExpiresAt: formatProtoTime(ic.ExpiresAt),
			LastError: ic.LastError,
			NextRetry: formatProtoTime(ic.NextRetry),
		}
	}

	if service.NetworkPolicy != nil {
		protoService.NetworkPolicy = networkPolicyToProto(service.NetworkPolicy)
	}

	return protoService
}

// formatProtoTime renders an optional time as an RFC 3339 string ("" if nil),
// matching the string-timestamp convention used across the API surface.
func formatProtoTime(t *time.Time) string {
	if t == nil || t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

// networkPolicyFromProto is the inverse of networkPolicyToProto.
func networkPolicyFromProto(np *generated.ServiceNetworkPolicy) *types.ServiceNetworkPolicy {
	out := &types.ServiceNetworkPolicy{}
	peers := func(in []*generated.NetworkPolicyPeer) []types.NetworkPolicyPeer {
		ps := make([]types.NetworkPolicyPeer, 0, len(in))
		for _, p := range in {
			ps = append(ps, types.NetworkPolicyPeer{
				Service:         p.GetService(),
				Namespace:       p.GetNamespace(),
				ServiceSelector: p.GetServiceSelector(),
				CIDR:            p.GetCidr(),
			})
		}
		return ps
	}
	for _, r := range np.GetIngress() {
		out.Ingress = append(out.Ingress, types.IngressRule{From: peers(r.GetFrom()), Ports: r.GetPorts()})
	}
	for _, r := range np.GetEgress() {
		out.Egress = append(out.Egress, types.EgressRule{To: peers(r.GetTo()), Ports: r.GetPorts()})
	}
	return out
}

// networkPolicyToProto converts the embedded service network policy to its
// wire form. Peers carry whichever selector the operator set (service,
// namespace, serviceSelector, or cidr).
func networkPolicyToProto(np *types.ServiceNetworkPolicy) *generated.ServiceNetworkPolicy {
	out := &generated.ServiceNetworkPolicy{}
	peers := func(in []types.NetworkPolicyPeer) []*generated.NetworkPolicyPeer {
		ps := make([]*generated.NetworkPolicyPeer, 0, len(in))
		for i := range in {
			ps = append(ps, &generated.NetworkPolicyPeer{
				Service:         in[i].Service,
				Namespace:       in[i].Namespace,
				ServiceSelector: in[i].ServiceSelector,
				Cidr:            in[i].CIDR,
			})
		}
		return ps
	}
	for i := range np.Ingress {
		out.Ingress = append(out.Ingress, &generated.NetworkPolicyIngressRule{
			From:  peers(np.Ingress[i].From),
			Ports: np.Ingress[i].Ports,
		})
	}
	for i := range np.Egress {
		out.Egress = append(out.Egress, &generated.NetworkPolicyEgressRule{
			To:    peers(np.Egress[i].To),
			Ports: np.Egress[i].Ports,
		})
	}
	return out
}

func ProtoToService(proto *generated.Service) (*types.Service, error) {
	if proto == nil {
		return nil, fmt.Errorf("proto service is nil")
	}

	// Create an initial service with basic fields
	service := &types.Service{
		ID:                 proto.Id,
		Name:               proto.Name,
		Namespace:          proto.Namespace,
		Image:              proto.Image,
		Command:            proto.Command,
		Scale:              int(proto.Scale),
		Runtime:            types.RuntimeType(proto.Runtime),
		ImagePull:          proto.ImagePull,
		ImagePullAnonymous: proto.ImagePullAnonymous,
		StatusReason:       proto.StatusReason,
		StatusMessage:      proto.StatusMessage,
		Labels:             proto.Labels,
	}

	// Convert metadata
	if proto.Metadata != nil {
		if service.Metadata == nil {
			service.Metadata = &types.ServiceMetadata{}
		}
		service.Metadata.Generation = proto.Metadata.Generation
		service.Metadata.LastNonZeroScale = int(proto.Metadata.LastNonZeroScale)
		service.Metadata.TemplateGeneration = proto.Metadata.TemplateGeneration
		service.Metadata.ObservedGeneration = proto.Metadata.ObservedGeneration

		createdAt, err := utils.ParseTimestamp(proto.Metadata.CreatedAt)
		if err != nil {
			return nil, fmt.Errorf("failed to parse created_at timestamp: %w", err)
		}
		service.Metadata.CreatedAt = *createdAt

		updatedAt, err := utils.ParseTimestamp(proto.Metadata.UpdatedAt)
		if err != nil {
			return nil, fmt.Errorf("failed to parse updated_at timestamp: %w", err)
		}
		service.Metadata.UpdatedAt = *updatedAt
	}

	// Update strategy + drain grace (RUNE-042).
	if proto.UpdateStrategy != nil {
		service.UpdateStrategy = &types.UpdateStrategy{
			Type: types.UpdateStrategyType(proto.UpdateStrategy.Type),
		}
		if proto.UpdateStrategy.MinServing != 0 {
			ms := int(proto.UpdateStrategy.MinServing)
			if ms < 0 {
				ms = 0 // the explicit-zero sentinel
			}
			service.UpdateStrategy.MinServing = &ms
		}
	}
	if proto.DrainSeconds != 0 {
		drain := int(proto.DrainSeconds)
		if drain < 0 {
			drain = 0 // the explicit-zero sentinel; DrainWindow floors it
		}
		service.DrainSeconds = &drain
	}
	if proto.Update != nil {
		upd := &types.UpdateStatus{
			TemplateGeneration: proto.Update.TemplateGeneration,
			Desired:            int(proto.Update.Desired),
			Updated:            int(proto.Update.Updated),
			UpdatedReady:       int(proto.Update.UpdatedReady),
			Available:          int(proto.Update.Available),
			Outdated:           int(proto.Update.Outdated),
			Message:            proto.Update.Message,
		}
		if t, err := utils.ParseTimestamp(proto.Update.StartedAt); err == nil {
			upd.StartedAt = *t
		}
		if t, err := utils.ParseTimestamp(proto.Update.LastProgressAt); err == nil {
			upd.LastProgressAt = *t
		}
		service.Update = upd
	}

	// Convert args
	if len(proto.Args) > 0 {
		service.Args = make([]string, len(proto.Args))
		copy(service.Args, proto.Args)
	}

	// Convert environment variables
	if len(proto.Env) > 0 {
		service.Env = make(map[string]string)
		for k, v := range proto.Env {
			service.Env[k] = v
		}
	}

	// Convert envFrom sources
	if len(proto.EnvFrom) > 0 {
		service.EnvFrom = make([]types.EnvFromSource, 0, len(proto.EnvFrom))
		for _, src := range proto.EnvFrom {
			service.EnvFrom = append(service.EnvFrom, types.EnvFromSource{
				SecretName:    src.SecretName,
				ConfigmapName: src.ConfigmapName,
				Namespace:     src.Namespace,
				Prefix:        src.Prefix,
			})
		}
	}

	// Convert ports
	if len(proto.Ports) > 0 {
		service.Ports = make([]types.ServicePort, len(proto.Ports))
		for i, port := range proto.Ports {
			service.Ports[i] = types.ServicePort{
				Name:       port.Name,
				Port:       int(port.Port),
				TargetPort: int(port.TargetPort),
				Protocol:   port.Protocol,
				HostPort:   int(port.HostPort),
			}
		}
	}

	// Convert expose
	if proto.Expose != nil {
		service.Expose = &types.ServiceExpose{
			Port:       proto.Expose.Port,
			Host:       proto.Expose.Host,
			Path:       proto.Expose.Path,
			AllowCIDRs: proto.Expose.AllowCidrs,
		}
		if proto.Expose.Tls != nil {
			service.Expose.TLS = &types.ExposeServiceTLS{
				Secret: proto.Expose.Tls.Secret,
				Auto:   proto.Expose.Tls.Auto,
				Mode:   proto.Expose.Tls.Mode,
			}
		}
		if proto.Expose.ClientCert != nil {
			service.Expose.ClientCert = &types.ExposeClientCert{
				CASecret: proto.Expose.ClientCert.CaSecret,
				Mode:     proto.Expose.ClientCert.Mode,
			}
		}
	}

	// Convert resources
	if proto.Resources != nil {
		if proto.Resources.Cpu != nil {
			service.Resources.CPU = types.ResourceLimit{
				Request: proto.Resources.Cpu.Request,
				Limit:   proto.Resources.Cpu.Limit,
			}
		}
		if proto.Resources.Memory != nil {
			service.Resources.Memory = types.ResourceLimit{
				Request: proto.Resources.Memory.Request,
				Limit:   proto.Resources.Memory.Limit,
			}
		}
		if g := proto.Resources.Gpu; g != nil {
			service.Resources.GPU = &types.GPURequest{
				Count:              int(g.Count),
				VRAM:               g.Vram,
				AllowHeterogeneous: g.AllowHeterogeneous,
			}
		}
	}

	// Convert secret mounts
	if len(proto.SecretMounts) > 0 {
		service.SecretMounts = make([]types.SecretMount, len(proto.SecretMounts))
		for i, m := range proto.SecretMounts {
			items := make([]types.KeyToPath, 0, len(m.Items))
			for _, it := range m.Items {
				items = append(items, types.KeyToPath{Key: it.Key, Path: it.Path})
			}
			service.SecretMounts[i] = types.SecretMount{
				Name:       m.Name,
				MountPath:  m.MountPath,
				SecretName: m.SecretName,
				Items:      items,
			}
		}
	}

	// Convert configmap mounts
	if len(proto.ConfigmapMounts) > 0 {
		service.ConfigmapMounts = make([]types.ConfigmapMount, len(proto.ConfigmapMounts))
		for i, m := range proto.ConfigmapMounts {
			items := make([]types.KeyToPath, 0, len(m.Items))
			for _, it := range m.Items {
				items = append(items, types.KeyToPath{Key: it.Key, Path: it.Path})
			}
			service.ConfigmapMounts[i] = types.ConfigmapMount{
				Name:          m.Name,
				MountPath:     m.MountPath,
				ConfigmapName: m.ConfigmapName,
				Items:         items,
			}
		}
	}

	// Convert volume mounts.
	if len(proto.Volumes) > 0 {
		service.Volumes = make([]types.VolumeMount, len(proto.Volumes))
		for i, m := range proto.Volumes {
			vm := types.VolumeMount{
				Name:      m.Name,
				MountPath: m.MountPath,
				ReadOnly:  m.ReadOnly,
				SubPath:   m.SubPath,
				FSMode:    m.FsMode,
			}
			if m.FsUserSet {
				u := int(m.FsUser)
				vm.FSUser = &u
			}
			if m.FsGroupSet {
				g := int(m.FsGroup)
				vm.FSGroup = &g
			}
			if m.Claim != nil {
				vm.Claim = &types.VolumeClaim{Name: m.Claim.Name}
			}
			if m.ClaimTemplate != nil {
				vm.ClaimTemplate = &types.VolumeClaimTemplate{
					StorageClassName: m.ClaimTemplate.StorageClassName,
					Size:             m.ClaimTemplate.Size,
					AccessMode:       types.AccessMode(m.ClaimTemplate.AccessMode),
					Parameters:       m.ClaimTemplate.Parameters,
					ReclaimPolicy:    types.ReclaimPolicy(m.ClaimTemplate.ReclaimPolicy),
				}
			}
			service.Volumes[i] = vm
		}
	}

	// Convert status
	switch proto.Status {
	case generated.ServiceStatus_SERVICE_STATUS_PENDING:
		service.Status = types.ServiceStatusPending
	case generated.ServiceStatus_SERVICE_STATUS_RUNNING:
		service.Status = types.ServiceStatusRunning
	case generated.ServiceStatus_SERVICE_STATUS_UPDATING:
		service.Status = types.ServiceStatusDeploying
	case generated.ServiceStatus_SERVICE_STATUS_FAILED:
		service.Status = types.ServiceStatusFailed
	default:
		service.Status = types.ServiceStatusPending
	}

	// Convert health check
	if proto.Health != nil {
		service.Health = &types.HealthCheck{}

		if proto.Health.Liveness != nil {
			service.Health.Liveness = &types.Probe{
				InitialDelaySeconds: int(proto.Health.Liveness.InitialDelaySeconds),
				IntervalSeconds:     int(proto.Health.Liveness.PeriodSeconds),
				TimeoutSeconds:      int(proto.Health.Liveness.TimeoutSeconds),
				Host:                proto.Health.Liveness.Host,
			}

			switch proto.Health.Liveness.Type {
			case generated.ProbeType_PROBE_TYPE_HTTP:
				service.Health.Liveness.Type = "http"
				service.Health.Liveness.Path = proto.Health.Liveness.Path
				service.Health.Liveness.Port = int(proto.Health.Liveness.Port)
			case generated.ProbeType_PROBE_TYPE_TCP:
				service.Health.Liveness.Type = "tcp"
				service.Health.Liveness.Port = int(proto.Health.Liveness.Port)
			case generated.ProbeType_PROBE_TYPE_EXEC:
				service.Health.Liveness.Type = "exec"
				service.Health.Liveness.Command = proto.Health.Liveness.Command
			}
		}

		if proto.Health.Readiness != nil {
			service.Health.Readiness = &types.Probe{
				InitialDelaySeconds: int(proto.Health.Readiness.InitialDelaySeconds),
				IntervalSeconds:     int(proto.Health.Readiness.PeriodSeconds),
				TimeoutSeconds:      int(proto.Health.Readiness.TimeoutSeconds),
				Host:                proto.Health.Readiness.Host,
			}

			switch proto.Health.Readiness.Type {
			case generated.ProbeType_PROBE_TYPE_HTTP:
				service.Health.Readiness.Type = "http"
				service.Health.Readiness.Path = proto.Health.Readiness.Path
				service.Health.Readiness.Port = int(proto.Health.Readiness.Port)
			case generated.ProbeType_PROBE_TYPE_TCP:
				service.Health.Readiness.Type = "tcp"
				service.Health.Readiness.Port = int(proto.Health.Readiness.Port)
			case generated.ProbeType_PROBE_TYPE_EXEC:
				service.Health.Readiness.Type = "exec"
				service.Health.Readiness.Command = proto.Health.Readiness.Command
			}
		}
	}

	// Dependencies
	if len(proto.Dependencies) > 0 {
		service.Dependencies = make([]types.DependencyRef, 0, len(proto.Dependencies))
		for _, d := range proto.Dependencies {
			service.Dependencies = append(service.Dependencies, types.DependencyRef{
				Service:   d.Service,
				Namespace: d.Namespace,
				Secret:    d.Secret,
				Configmap: d.Configmap,
			})
		}
	}

	// InitSteps (RUNE-121) and main-container SecurityContext.
	if len(proto.InitSteps) > 0 {
		service.InitSteps = make([]types.InitStep, 0, len(proto.InitSteps))
		for _, p := range proto.InitSteps {
			service.InitSteps = append(service.InitSteps, initStepFromProto(p))
		}
	}
	if proto.SecurityContext != nil {
		service.SecurityContext = securityContextFromProto(proto.SecurityContext)
	}

	if proto.Discovery != nil {
		service.Discovery = &types.ServiceDiscovery{
			VIP:                proto.Discovery.Vip,
			Mode:               proto.Discovery.Mode,
			LocalityPreference: proto.Discovery.LocalityPreference,
		}
	}

	// IngressCert is read-only status (server-populated), so there is no reverse
	// mapping for it here. NetworkPolicy is spec — round-trip it so a service
	// created/updated through the proto API keeps its policy.
	if proto.NetworkPolicy != nil {
		service.NetworkPolicy = networkPolicyFromProto(proto.NetworkPolicy)
	}

	if len(proto.Instances) > 0 {
		service.Instances = make([]types.Instance, 0, len(proto.Instances))
		for _, pi := range proto.Instances {
			inst := embeddedInstanceFromProto(pi)
			if inst != nil {
				service.Instances = append(service.Instances, *inst)
			}
		}
	}

	return service, nil
}

// embeddedInstanceToProto / embeddedInstanceFromProto are the minimal
// converters used when an Instance is carried inside a Service response
// (GetService, ListServices). They cover the fields the CLI reads off
// service.Instances — primarily Status/Name/ID/StatusMessage. Callers
// needing the full instance object should still use ListInstances; this
// embed avoids a second round-trip for readiness checks but isn't a
// drop-in replacement for the dedicated instance RPCs.
func embeddedInstanceToProto(i *types.Instance) *generated.Instance {
	if i == nil {
		return nil
	}
	return &generated.Instance{
		Id:            i.ID,
		Runner:        string(i.Runner),
		Namespace:     i.Namespace,
		Name:          i.Name,
		ServiceId:     i.ServiceID,
		ServiceName:   i.ServiceName,
		NodeId:        i.NodeID,
		Labels:        i.Labels,
		Ip:            i.IP,
		Status:        instanceStatusToProto(i.Status),
		StatusMessage: i.StatusMessage,
		ContainerId:   i.ContainerID,
		Pid:           utils.ToInt32NonNegative(i.PID),
		CreatedAt:     formatInstanceTime(i.CreatedAt),
		UpdatedAt:     formatInstanceTime(i.UpdatedAt),
		Usage:         InstanceUsageToProto(i.Usage),
	}
}

// InstanceUsageToProto maps the transient live-usage sample onto the wire.
// nil in → nil out: an absent usage field means "unknown", which clients
// must render as unknown rather than 0.
func InstanceUsageToProto(u *types.InstanceUsage) *generated.InstanceUsage {
	if u == nil {
		return nil
	}
	return &generated.InstanceUsage{
		CpuPercent:    u.CPUPercent,
		MemUsedBytes:  u.MemUsedBytes,
		MemLimitBytes: u.MemLimitBytes,
	}
}

// formatInstanceTime renders a timestamp as RFC3339, or "" for the zero
// value so inlined instances don't serialize "0001-01-01T00:00:00Z".
func formatInstanceTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.Format(time.RFC3339)
}

func embeddedInstanceFromProto(p *generated.Instance) *types.Instance {
	if p == nil {
		return nil
	}
	inst := &types.Instance{
		ID:            p.Id,
		Runner:        types.RunnerType(p.Runner),
		Namespace:     p.Namespace,
		Name:          p.Name,
		ServiceID:     p.ServiceId,
		ServiceName:   p.ServiceName,
		NodeID:        p.NodeId,
		Labels:        p.Labels,
		IP:            p.Ip,
		Status:        instanceStatusFromProto(p.Status),
		StatusMessage: p.StatusMessage,
		ContainerID:   p.ContainerId,
		PID:           int(p.Pid),
	}
	if ts, err := parseTimestamp(p.CreatedAt); err == nil && ts != nil {
		inst.CreatedAt = *ts
	}
	if ts, err := parseTimestamp(p.UpdatedAt); err == nil && ts != nil {
		inst.UpdatedAt = *ts
	}
	return inst
}

func instanceStatusToProto(s types.InstanceStatus) generated.InstanceStatus {
	switch s {
	case types.InstanceStatusPending:
		return generated.InstanceStatus_INSTANCE_STATUS_PENDING
	case types.InstanceStatusCreated:
		return generated.InstanceStatus_INSTANCE_STATUS_CREATED
	case types.InstanceStatusStarting:
		return generated.InstanceStatus_INSTANCE_STATUS_STARTING
	case types.InstanceStatusRunning:
		return generated.InstanceStatus_INSTANCE_STATUS_RUNNING
	case types.InstanceStatusStopped:
		return generated.InstanceStatus_INSTANCE_STATUS_STOPPED
	case types.InstanceStatusFailed:
		return generated.InstanceStatus_INSTANCE_STATUS_FAILED
	case types.InstanceStatusExited:
		return generated.InstanceStatus_INSTANCE_STATUS_EXITED
	case types.InstanceStatusDeleted:
		return generated.InstanceStatus_INSTANCE_STATUS_DELETED
	default:
		return generated.InstanceStatus_INSTANCE_STATUS_PENDING
	}
}

func instanceStatusFromProto(s generated.InstanceStatus) types.InstanceStatus {
	switch s {
	case generated.InstanceStatus_INSTANCE_STATUS_PENDING:
		return types.InstanceStatusPending
	case generated.InstanceStatus_INSTANCE_STATUS_CREATED:
		return types.InstanceStatusCreated
	case generated.InstanceStatus_INSTANCE_STATUS_STARTING:
		return types.InstanceStatusStarting
	case generated.InstanceStatus_INSTANCE_STATUS_RUNNING:
		return types.InstanceStatusRunning
	case generated.InstanceStatus_INSTANCE_STATUS_STOPPED:
		return types.InstanceStatusStopped
	case generated.InstanceStatus_INSTANCE_STATUS_FAILED:
		return types.InstanceStatusFailed
	case generated.InstanceStatus_INSTANCE_STATUS_EXITED:
		return types.InstanceStatusExited
	case generated.InstanceStatus_INSTANCE_STATUS_DELETED:
		return types.InstanceStatusDeleted
	default:
		return types.InstanceStatusPending
	}
}

// initStepToProto converts a types.InitStep to its proto form.
func initStepToProto(s *types.InitStep) *generated.InitStep {
	if s == nil {
		return nil
	}
	p := &generated.InitStep{
		Name:          s.Name,
		Image:         s.Image,
		Command:       s.Command,
		Args:          append([]string(nil), s.Args...),
		Env:           cloneStringMap(s.Env),
		TimeoutNanos:  int64(s.Timeout),
		RestartPolicy: string(s.RestartPolicy),
	}
	for _, src := range s.EnvFrom {
		p.EnvFrom = append(p.EnvFrom, &generated.EnvFromSource{
			SecretName:    src.SecretName,
			ConfigmapName: src.ConfigmapName,
			Namespace:     src.Namespace,
			Prefix:        src.Prefix,
		})
	}
	// Preserve nil vs empty-slice distinction (see types.InitStep docs).
	if s.Volumes != nil {
		p.VolumesSet = true
		p.Volumes = append([]string(nil), s.Volumes...)
	}
	if s.SecretMounts != nil {
		p.SecretMountsSet = true
		p.SecretMounts = append([]string(nil), s.SecretMounts...)
	}
	if s.ConfigmapMounts != nil {
		p.ConfigmapMountsSet = true
		p.ConfigmapMounts = append([]string(nil), s.ConfigmapMounts...)
	}
	if s.Resources != nil {
		p.Resources = &generated.Resources{
			Cpu: &generated.ResourceLimit{
				Request: s.Resources.CPU.Request,
				Limit:   s.Resources.CPU.Limit,
			},
			Memory: &generated.ResourceLimit{
				Request: s.Resources.Memory.Request,
				Limit:   s.Resources.Memory.Limit,
			},
		}
	}
	if s.RunIf.Type != "" || s.RunIf.Path != "" || s.RunIf.Volume != "" {
		p.RunIf = &generated.RunIfSpec{
			Type:   string(s.RunIf.Type),
			Path:   s.RunIf.Path,
			Volume: s.RunIf.Volume,
		}
	}
	if s.SecurityContext != nil {
		p.SecurityContext = securityContextToProto(s.SecurityContext)
	}
	return p
}

func initStepFromProto(p *generated.InitStep) types.InitStep {
	s := types.InitStep{
		Name:          p.Name,
		Image:         p.Image,
		Command:       p.Command,
		Args:          append([]string(nil), p.Args...),
		Env:           cloneStringMap(p.Env),
		Timeout:       time.Duration(p.TimeoutNanos),
		RestartPolicy: types.InitStepRestartPolicy(p.RestartPolicy),
	}
	for _, src := range p.EnvFrom {
		s.EnvFrom = append(s.EnvFrom, types.EnvFromSource{
			SecretName:    src.SecretName,
			ConfigmapName: src.ConfigmapName,
			Namespace:     src.Namespace,
			Prefix:        src.Prefix,
		})
	}
	if p.VolumesSet {
		s.Volumes = append([]string{}, p.Volumes...)
	}
	if p.SecretMountsSet {
		s.SecretMounts = append([]string{}, p.SecretMounts...)
	}
	if p.ConfigmapMountsSet {
		s.ConfigmapMounts = append([]string{}, p.ConfigmapMounts...)
	}
	if p.Resources != nil {
		r := &types.Resources{}
		if p.Resources.Cpu != nil {
			r.CPU.Request = p.Resources.Cpu.Request
			r.CPU.Limit = p.Resources.Cpu.Limit
		}
		if p.Resources.Memory != nil {
			r.Memory.Request = p.Resources.Memory.Request
			r.Memory.Limit = p.Resources.Memory.Limit
		}
		s.Resources = r
	}
	if p.RunIf != nil {
		s.RunIf = types.RunIf{
			Type:   types.RunIfType(p.RunIf.Type),
			Path:   p.RunIf.Path,
			Volume: p.RunIf.Volume,
		}
	}
	if p.SecurityContext != nil {
		s.SecurityContext = securityContextFromProto(p.SecurityContext)
	}
	return s
}

func securityContextToProto(sc *types.SecurityContext) *generated.SecurityContext {
	if sc == nil {
		return nil
	}
	out := &generated.SecurityContext{
		CapAdd:     append([]string(nil), sc.CapAdd...),
		CapDrop:    append([]string(nil), sc.CapDrop...),
		Privileged: sc.Privileged,
	}
	if sc.SeccompProfile != nil {
		out.SeccompProfile = &generated.SeccompProfile{
			Type:             string(sc.SeccompProfile.Type),
			LocalhostProfile: sc.SeccompProfile.LocalhostProfile,
		}
	}
	return out
}

func securityContextFromProto(p *generated.SecurityContext) *types.SecurityContext {
	if p == nil {
		return nil
	}
	sc := &types.SecurityContext{
		CapAdd:     append([]string(nil), p.CapAdd...),
		CapDrop:    append([]string(nil), p.CapDrop...),
		Privileged: p.Privileged,
	}
	if p.SeccompProfile != nil {
		sc.SeccompProfile = &types.SeccompProfile{
			Type:             types.SeccompProfileType(p.SeccompProfile.Type),
			LocalhostProfile: p.SeccompProfile.LocalhostProfile,
		}
	}
	return sc
}

func cloneStringMap(m map[string]string) map[string]string {
	if m == nil {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}
