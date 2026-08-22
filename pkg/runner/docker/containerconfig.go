// Package docker — Spec -> container config translation. Split from runner.go (RUNE-312).
package docker

import (
	"fmt"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/go-connections/nat"
	"github.com/runestack/rune/pkg/log"
	runetypes "github.com/runestack/rune/pkg/types"
)

// instanceToContainerConfig converts a Rune instance to Docker container config.
func (r *DockerRunner) instanceToContainerConfig(instance *runetypes.Instance) (*container.Config, *container.HostConfig, error) {
	// Extract service ID from the instance
	serviceID := instance.ServiceID
	if serviceID == "" {
		return nil, nil, fmt.Errorf("service ID is required")
	}

	// Get the image from environment variables
	var image string
	if instance.Metadata != nil {
		image = instance.Metadata.Image
	}

	// Validate that we have an image
	if image == "" {
		return nil, nil, fmt.Errorf("no image specified for instance %s", instance.ID)
	}

	r.logger.Debug("Using image for instance",
		log.Str("instance", instance.ID),
		log.Str("image", image))

	// Configure the container
	containerConfig := &container.Config{
		Image: image,
		Labels: map[string]string{
			"rune.managed":     "true",
			"rune.namespace":   instance.Namespace,
			"rune.instance.id": instance.ID,
			"rune.service.id":  serviceID,
			// Service name, NOT the instance name. The orphan reaper
			// matches running containers to their service by this value
			// (via Instance.ServiceName reconstructed in List); storing
			// the instance name here left replaced containers unreapable.
			"rune.service.name":  instance.ServiceName,
			"rune.instance.name": instance.Name,
		},
		Env: formatEnvVars(instance.Environment),
	}

	// Apply the service's command/args to the container.
	// Service.Command → Entrypoint (replaces the image's ENTRYPOINT);
	// Service.Args → Cmd (replaces the image's CMD).
	// Empty fields leave the image's baked-in defaults untouched.
	//
	// instance.Exec.Command, when set, is a `rune exec` ad-hoc
	// override and takes precedence over the spec — it replaces both
	// Entrypoint and Cmd with the literal command line the user
	// passed to `rune exec`.
	if instance.Metadata != nil {
		if instance.Metadata.Command != "" {
			containerConfig.Entrypoint = []string{instance.Metadata.Command}
		}
		if len(instance.Metadata.Args) > 0 {
			containerConfig.Cmd = append([]string(nil), instance.Metadata.Args...)
		}
	}
	if instance.Exec != nil && len(instance.Exec.Command) > 0 {
		containerConfig.Entrypoint = nil
		containerConfig.Cmd = instance.Exec.Command
	}

	// Configure host config with mounts and resources. LogConfig caps
	// per-container log growth on the host (json-file rotation).
	hostConfig := &container.HostConfig{LogConfig: r.config.logConfig()}

	// Map resource requests/limits to Docker host config if provided.
	// Shared with init steps (init.go) so both paths keep identical
	// enforcement semantics — this block was a verbatim duplicate of
	// applyResourceLimits before RUNE-312 Phase 2 deduplicated it.
	if instance != nil && instance.Resources != nil {
		applyResourceLimits(hostConfig, instance.Resources)
	}

	// Handle secret and config mounts
	if instance.Metadata != nil {
		// Process secret mounts
		if len(instance.Metadata.SecretMounts) > 0 {
			secretMounts, err := r.prepareSecretMounts(instance.Metadata.SecretMounts)
			if err != nil {
				return nil, nil, fmt.Errorf("failed to prepare secret mounts: %w", err)
			}
			hostConfig.Mounts = append(hostConfig.Mounts, secretMounts...)
		}

		// Process config mounts
		if len(instance.Metadata.ConfigmapMounts) > 0 {
			configMounts, err := r.prepareConfigmapsMounts(instance.Metadata.ConfigmapMounts)
			if err != nil {
				return nil, nil, fmt.Errorf("failed to prepare config mounts: %w", err)
			}
			hostConfig.Mounts = append(hostConfig.Mounts, configMounts...)
		}

		// Process volume mounts. Source is the host-side path
		// produced by the storage driver (a managed directory, an
		// operator host path, or a block-device mount target). We only
		// emit bind mounts here; agent-side Attach/Mount/Unmount/Detach
		// for block-device drivers lives in the agent.
		if len(instance.Metadata.VolumeMounts) > 0 {
			for _, vm := range instance.Metadata.VolumeMounts {
				if vm.Source == "" || vm.MountPath == "" {
					return nil, nil, fmt.Errorf("invalid volume mount %q: source or mountPath empty", vm.Name)
				}
				source := vm.Source
				if vm.SubPath != "" {
					source = filepath.Join(source, vm.SubPath)
				}
				hostConfig.Mounts = append(hostConfig.Mounts, mount.Mount{
					Type:     mount.TypeBind,
					Source:   source,
					Target:   vm.MountPath,
					ReadOnly: vm.ReadOnly,
				})
			}
		}
	}

	// Inject the agent's embedded DNS (RUNE-063) when it has been
	// activated. Activation is gated by the agent on data-plane
	// readiness so containers never resolve names to unreachable
	// VIPs. --dns-search ensures bare names like "db" resolve to
	// "db.<namespace>.rune".
	r.dnsMu.RLock()
	dnsServers := append([]string(nil), r.dnsServers...)
	dnsSearch := append([]string(nil), r.dnsSearch...)
	r.dnsMu.RUnlock()
	if len(dnsServers) > 0 {
		hostConfig.DNS = append(hostConfig.DNS, dnsServers...)
		// Per-instance namespace search domain
		if instance.Namespace != "" {
			hostConfig.DNSSearch = append(hostConfig.DNSSearch, instance.Namespace+".rune")
		}
		hostConfig.DNSSearch = append(hostConfig.DNSSearch, dnsSearch...)
	}

	// Expose the service port inside the container so the edge
	// ingress controller can dial it over Docker networking. We
	// never publish a host port from the runner: when expose.host
	// is set, the ingress listener already owns :80/:443 on the
	// host and proxies in by container IP. Operators who want a
	// raw host bind for a non-ingress service can declare it via
	// the runner-level mechanism (TBD); the MVP host-bind escape
	// hatch (`expose.hostPort`) was removed in favor of a single
	// well-defined ingress path.
	if instance.Metadata != nil && instance.Metadata.Expose != nil && len(instance.Metadata.Ports) > 0 {
		// Resolve the referenced port by name or number
		var svcPort *runetypes.ServicePort
		ref := instance.Metadata.Expose.Port
		for i := range instance.Metadata.Ports {
			p := &instance.Metadata.Ports[i]
			if p.Name == ref {
				svcPort = p
				break
			}
			if ref != "" {
				if n, err := strconv.Atoi(ref); err == nil && n == p.Port {
					svcPort = p
					break
				}
			}
		}
		if svcPort != nil {
			proto := strings.ToLower(strings.TrimSpace(svcPort.Protocol))
			if proto == "" {
				proto = "tcp"
			}
			if proto == "tcp" {
				containerPort := nat.Port(fmt.Sprintf("%d/%s", svcPort.Port, proto))
				if containerConfig.ExposedPorts == nil {
					containerConfig.ExposedPorts = nat.PortSet{}
				}
				containerConfig.ExposedPorts[containerPort] = struct{}{}
				// No host-port publication; the ingress controller
				// reaches the container by container IP.
				instance.Metadata.ExposedHost = ""
				instance.Metadata.ExposedHostPort = 0
			}
		}
	}

	// Publish any port with hostPort > 0 to 127.0.0.1:<hostPort> on the
	// host. Dev-mode escape hatch for platforms where the cluster
	// dataplane cannot reach container bridge IPs from the host
	// (macOS Docker Desktop). Production services should reach each
	// other via the cluster VIP / ingress instead.
	if instance.Metadata != nil && len(instance.Metadata.Ports) > 0 {
		for _, p := range instance.Metadata.Ports {
			if p.HostPort <= 0 {
				continue
			}
			proto := strings.ToLower(strings.TrimSpace(p.Protocol))
			if proto == "" {
				proto = "tcp"
			}
			cport, err := nat.NewPort(proto, strconv.Itoa(p.Port))
			if err != nil {
				return nil, nil, fmt.Errorf("invalid container port %d/%s: %w", p.Port, proto, err)
			}
			if containerConfig.ExposedPorts == nil {
				containerConfig.ExposedPorts = nat.PortSet{}
			}
			containerConfig.ExposedPorts[cport] = struct{}{}
			if hostConfig.PortBindings == nil {
				hostConfig.PortBindings = nat.PortMap{}
			}
			hostConfig.PortBindings[cport] = append(hostConfig.PortBindings[cport], nat.PortBinding{
				HostIP:   "127.0.0.1",
				HostPort: strconv.Itoa(p.HostPort),
			})
		}
	}

	// Apply optional security context (seccomp / capabilities /
	// privileged). Privileged and seccomp=unconfined are gated
	// server-side; the runner only enforces structural correctness.
	applySecurityContext(hostConfig, instance.SecurityContext)

	return containerConfig, hostConfig, nil
}

// formatEnvVars formats a map of environment variables into a slice of "key=value" strings.
func formatEnvVars(env map[string]string) []string {
	result := make([]string, 0, len(env))
	for k, v := range env {
		result = append(result, fmt.Sprintf("%s=%s", k, v))
	}
	return result
}
