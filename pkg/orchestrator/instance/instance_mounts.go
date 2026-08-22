// Volume mount resolution: readiness waits, mount-target lookup, claim
// templates. Split from instance_controller.go (RUNE-311).

package instance

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/runestack/rune/pkg/store"
	"github.com/runestack/rune/pkg/store/repos"
	"github.com/runestack/rune/pkg/types"
)

// mountBinder owns secret/config/volume mount resolution (RUNE-311
// Phase 2). resolver and nodeID are read through the controller AT EACH
// USE, never captured: runed wires the real MountResolver — and, via
// SetEndpointPublisher, the node identity the mount path binds volumes
// with — into a LIVE controller after start (cmd/runed), so a value
// captured at construction would be the not-ready stub forever.
type mountBinder struct {
	store      store.Store
	secretRepo *repos.SecretRepo
	configRepo *repos.ConfigmapRepo
	resolver   func() MountResolver
	nodeID     func() string
}

func newMountBinder(st store.Store, secretRepo *repos.SecretRepo, configRepo *repos.ConfigmapRepo, resolver func() MountResolver, nodeID func() string) *mountBinder {
	return &mountBinder{store: st, secretRepo: secretRepo, configRepo: configRepo, resolver: resolver, nodeID: nodeID}
}

// Thin delegators: lifecycle code and existing tests call these on the
// controller; the logic lives on mountBinder.
func (c *Controller) resolveMounts(ctx context.Context, service *types.Service, instance *types.Instance) error {
	return c.mounts.resolveMounts(ctx, service, instance)
}

func (c *Controller) resolveVolumeMount(ctx context.Context, service *types.Service, instance *types.Instance, m types.VolumeMount) (types.ResolvedVolumeMount, error) {
	return c.mounts.resolveVolumeMount(ctx, service, instance, m)
}

// resolveMounts resolves secret and config mounts for an instance by fetching the actual data
func (b *mountBinder) resolveMounts(ctx context.Context, service *types.Service, instance *types.Instance) error {
	// Initialize metadata if not present
	if instance.Metadata == nil {
		instance.Metadata = &types.InstanceMetadata{}
	}

	// Resolve secret mounts
	if len(service.SecretMounts) > 0 {
		instance.Metadata.SecretMounts = make([]types.ResolvedSecretMount, 0, len(service.SecretMounts))

		for _, mount := range service.SecretMounts {
			// Determine secret name; default to mount.Name if SecretName is omitted
			secretName := mount.SecretName
			if secretName == "" {
				secretName = mount.Name
			}
			// Get the secret from the store
			secret, err := b.secretRepo.Get(ctx, service.Namespace, secretName)
			if err != nil {
				return fmt.Errorf("failed to get secret %s for mount %s: %w", secretName, mount.Name, err)
			}

			// Create resolved mount
			resolvedMount := types.ResolvedSecretMount{
				Name:      mount.Name,
				MountPath: mount.MountPath,
				Data:      secret.Data,
				Items:     mount.Items,
			}

			instance.Metadata.SecretMounts = append(instance.Metadata.SecretMounts, resolvedMount)
		}
	}

	// Resolve config mounts
	if len(service.ConfigmapMounts) > 0 {
		instance.Metadata.ConfigmapMounts = make([]types.ResolvedConfigmapMount, 0, len(service.ConfigmapMounts))

		for _, mount := range service.ConfigmapMounts {
			// Determine config name; default to mount.Name if ConfigName is omitted
			configName := mount.ConfigmapName
			if configName == "" {
				configName = mount.Name
			}
			// Get the config from the store
			config, err := b.configRepo.Get(ctx, service.Namespace, configName)
			if err != nil {
				return fmt.Errorf("failed to get config %s for mount %s: %w", configName, mount.Name, err)
			}

			// Create resolved mount
			resolvedMount := types.ResolvedConfigmapMount{
				Name:      mount.Name,
				MountPath: mount.MountPath,
				Data:      config.Data,
				Items:     mount.Items,
			}

			instance.Metadata.ConfigmapMounts = append(instance.Metadata.ConfigmapMounts, resolvedMount)
		}
	}

	// Resolve volume mounts.
	//
	// - Claim: looks up an existing Volume by name (cross-namespace via
	//   "ns/name"); fails if not yet Available.
	// - ClaimTemplate: idempotently creates a per-instance Volume named
	//   "<mount.Name>-<service.Name>-<ordinal>" with OwnerService set,
	//   then waits for the VolumeController to provision it.
	//
	// Mounts that resolve to a not-yet-Available volume surface as a
	// reconcile error so the instance status reports the cause; the
	// service reconciler retries on the next tick.
	if len(service.Volumes) > 0 {
		instance.Metadata.VolumeMounts = make([]types.ResolvedVolumeMount, 0, len(service.Volumes))
		for _, m := range service.Volumes {
			resolved, err := b.resolveVolumeMount(ctx, service, instance, m)
			if err != nil {
				return fmt.Errorf("failed to resolve volume mount %q: %w", m.Name, err)
			}
			instance.Metadata.VolumeMounts = append(instance.Metadata.VolumeMounts, resolved)
		}
	}

	return nil
}

// volumeReadyPollInterval / volumeReadyTimeout bound waitForVolumeReady.
// The timeout is deliberately modest: the reconcile loop is serial, so a
// long block here stalls every other service. Provisioning a volume to
// Available (a DO createVolume call + the VolumeController marking it)
// normally completes in seconds; a volume that needs longer falls back
// to the instance-create retry (recordCreateFailure / NextCreateAttemptAt).
const (
	volumeReadyPollInterval = 2 * time.Second
	volumeReadyTimeout      = 60 * time.Second
)

// mountTargetPollInterval / mountTargetTimeout bound waitForMountTarget.
// After stamping BoundNode the agent volumes Subsystem still needs a
// watch/reconcile tick to Attach+Mount and record the target; without
// this wait a fresh cast always races into LaunchFailed/VolumeNotReady.
//
// Vars rather than consts purely so tests can shorten the wait; nothing in
// production reassigns them.
var (
	mountTargetPollInterval = 500 * time.Millisecond
	mountTargetTimeout      = 45 * time.Second
)

// waitForVolumeReady polls the volume row until it is Available/Bound,
// reaches a terminal-failure status, or the timeout / ctx fires. It
// exists so an instance create racing ahead of asynchronous volume
// provisioning waits briefly rather than failing on a still-Pending
// volume.
func (b *mountBinder) waitForVolumeReady(ctx context.Context, ns, name string) (types.Volume, error) {
	deadline := time.Now().Add(volumeReadyTimeout)
	for {
		var vol types.Volume
		if err := b.store.Get(ctx, types.ResourceTypeVolume, ns, name, &vol); err != nil {
			return types.Volume{}, fmt.Errorf("get volume %s/%s: %w", ns, name, err)
		}
		switch vol.Status {
		case types.VolumeStatusAvailable, types.VolumeStatusBound:
			return vol, nil
		case types.VolumeStatusStalled, types.VolumeStatusFailed, types.VolumeStatusReleased:
			// Terminal — provisioning will not recover on its own.
			return types.Volume{}, fmt.Errorf("volume %s/%s is not ready (status=%s, reason=%q)", ns, name, vol.Status, vol.StatusReason)
		}
		// Pending / Provisioning / "" — still coming up.
		if time.Now().After(deadline) {
			return types.Volume{}, fmt.Errorf("volume %s/%s is not ready (status=%s, reason=%q): not Available after %s", ns, name, vol.Status, vol.StatusReason, volumeReadyTimeout)
		}
		select {
		case <-ctx.Done():
			return types.Volume{}, ctx.Err()
		case <-time.After(volumeReadyPollInterval):
		}
	}
}

// volumeMountKey matches the tracking key used by the agent volumes
// Subsystem (see internal/agent/volumes reconcile).
func volumeMountKey(vol types.Volume) string {
	if vol.ID != "" {
		return vol.ID
	}
	return vol.Namespace + "/" + vol.Name
}

// waitForMountTarget polls the agent MountResolver until the volume is
// mounted on this node or the timeout fires.
func (b *mountBinder) waitForMountTarget(ctx context.Context, vol types.Volume, ns, name string) (string, error) {
	key := volumeMountKey(vol)
	deadline := time.Now().Add(mountTargetTimeout)
	for {
		if target, ok := b.resolver().MountTargetFor(key); ok && target != "" {
			return target, nil
		}
		if time.Now().After(deadline) {
			// Include the agent's last bring-up failure when it can report
			// one. "not yet mounted (will retry)" on its own is actively
			// misleading for a non-transient cause like a rejected cloud
			// credential: it implies waiting will fix it, and hides the only
			// detail an operator can act on.
			if reporter, ok := b.resolver().(MountErrorReporter); ok {
				if cause, has := reporter.MountErrorFor(key); has && cause != "" {
					return "", fmt.Errorf("volume %s/%s not yet mounted on node %s: %s", ns, name, b.nodeID(), cause)
				}
			}
			return "", fmt.Errorf("volume %s/%s not yet mounted on node %s (will retry)", ns, name, b.nodeID())
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(mountTargetPollInterval):
		}
	}
}

// resolveVolumeMount converts a Service.VolumeMount into the runner-facing
// ResolvedVolumeMount by looking up (or auto-provisioning, for the
// ClaimTemplate form) the bound Volume and using its Handle as the
// host-side bind source.
func (b *mountBinder) resolveVolumeMount(ctx context.Context, service *types.Service, instance *types.Instance, m types.VolumeMount) (types.ResolvedVolumeMount, error) {
	switch {
	case m.Claim != nil && m.ClaimTemplate != nil:
		return types.ResolvedVolumeMount{}, fmt.Errorf("volume mount %q sets both claim and claimTemplate; pick one", m.Name)
	case m.Claim == nil && m.ClaimTemplate == nil:
		return types.ResolvedVolumeMount{}, fmt.Errorf("volume mount %q sets neither claim nor claimTemplate", m.Name)
	}

	// Resolve the Volume reference: ClaimTemplate drives idempotent
	// auto-creation, Claim is a straight lookup.
	var ns, name string
	if m.ClaimTemplate != nil {
		ns = service.Namespace
		// The per-replica volume binds to the instance's ordinal slot. Ordinal
		// is now carried explicitly on the instance (set at creation), not
		// parsed back out of the name — so the name format is free to change.
		ordinal := instance.Ordinal
		name = fmt.Sprintf("%s-%s-%d", m.Name, service.Name, ordinal)
		if err := b.ensureClaimTemplateVolume(ctx, service, m, name, ordinal); err != nil {
			return types.ResolvedVolumeMount{}, fmt.Errorf("ensure claim-template volume %s/%s: %w", ns, name, err)
		}
	} else {
		ns = service.Namespace
		name = m.Claim.Name
		if idx := strings.Index(name, "/"); idx > 0 {
			ns, name = name[:idx], name[idx+1:]
		}
	}

	// A claim-template volume is provisioned asynchronously by the
	// VolumeController. Briefly wait for it rather than failing the
	// instance create the instant it is still Pending — otherwise a
	// fresh `rune cast` of a stateful service always reports
	// LaunchFailed before provisioning has had a chance to finish.
	vol, err := b.waitForVolumeReady(ctx, ns, name)
	if err != nil {
		return types.ResolvedVolumeMount{}, err
	}

	// Bind the volume to this node + consuming instance so the agent-
	// side volumes Subsystem will Attach + Mount it. The Subsystem
	// gates on BoundNode == nodeID (see internal/agent/volumes/
	// subsystem.go shouldMount). BoundClaim records which instance
	// currently owns the binding — refreshed on every instance change
	// (e.g. `rune restart` cycling 1→0→1) so the row doesn't keep
	// pointing at a Deleted instance after a restart.
	if nodeID := b.nodeID(); nodeID != "" {
		newClaim := service.Namespace + "/" + instance.Name
		if vol.BoundNode != nodeID || vol.BoundClaim != newClaim {
			vol.BoundNode = nodeID
			vol.BoundClaim = newClaim
			vol.UpdatedAt = time.Now().UTC()
			if err := b.store.Update(ctx, types.ResourceTypeVolume, vol.Namespace, vol.Name, &vol); err != nil {
				return types.ResolvedVolumeMount{}, fmt.Errorf("bind volume %s/%s to node %s: %w", ns, name, nodeID, err)
			}
		}
	}

	// Resolve the bind source. When a MountResolver is wired (production
	// runed), the agent-side Subsystem owns the host-path mapping; we
	// require it to have reported a target before launching, because for
	// any driver where Handle is not a host path (do-volume, future cloud
	// drivers) a bare Handle is not a valid Docker bind source. The error
	// is transient — the service reconciler retries until the Subsystem
	// has finished Attach + Mount.
	//
	// When no MountResolver is wired (dev/standalone, tests), fall back
	// to Volume.Handle. That's the historical behaviour and is correct
	// for the in-tree local / local-host drivers where Handle == host
	// path.
	var source string
	if b.resolver() != nil {
		target, err := b.waitForMountTarget(ctx, vol, ns, name)
		if err != nil {
			return types.ResolvedVolumeMount{}, err
		}
		source = target
	} else {
		source = vol.Handle
	}
	if source == "" {
		return types.ResolvedVolumeMount{}, fmt.Errorf("volume %s/%s has no mount source", ns, name)
	}

	// Apply fsUser / fsGroup / fsMode to the mount root, idempotently.
	// Solves the "fresh ext4 owned by root, container runs as uid N,
	// EACCES on first write" pattern without an init-step chown. Only
	// runs when the operator opted in — absent fields are a no-op so
	// local-host mounts (operator-managed paths) aren't stomped on.
	if err := applyFSOwnership(source, m.FSUser, m.FSGroup, m.FSMode); err != nil {
		return types.ResolvedVolumeMount{}, fmt.Errorf("apply fs ownership on %s (volume %s/%s): %w", source, ns, name, err)
	}

	return types.ResolvedVolumeMount{
		Name:            m.Name,
		MountPath:       m.MountPath,
		Source:          source,
		VolumeName:      name,
		VolumeNamespace: ns,
		ReadOnly:        m.ReadOnly,
		SubPath:         m.SubPath,
	}, nil
}

// ensureClaimTemplateVolume creates the per-replica Volume row from a
// ClaimTemplate the first time it is observed. It is idempotent: a
// pre-existing volume with the same namespace+name is left alone (the
// VolumeController owns subsequent mutations).
func (b *mountBinder) ensureClaimTemplateVolume(ctx context.Context, service *types.Service, m types.VolumeMount, name string, ordinal int) error {
	ns := service.Namespace
	var existing types.Volume
	if err := b.store.Get(ctx, types.ResourceTypeVolume, ns, name, &existing); err == nil {
		return nil
	}

	tpl := m.ClaimTemplate
	now := time.Now().UTC()
	vol := &types.Volume{
		ID:               name,
		Name:             name,
		Namespace:        ns,
		StorageClassName: tpl.StorageClassName,
		Size:             tpl.Size,
		AccessMode:       tpl.AccessMode,
		Parameters:       tpl.Parameters,
		ReclaimPolicy:    tpl.ReclaimPolicy,
		OwnerService:     fmt.Sprintf("%s/%s", ns, service.Name),
		BoundClaim:       fmt.Sprintf("%s/%s/%d", service.Name, m.Name, ordinal),
		Status:           types.VolumeStatusPending,
		CreatedAt:        now,
		UpdatedAt:        now,
	}
	if err := b.store.Create(ctx, types.ResourceTypeVolume, ns, name, vol); err != nil {
		// A racing reconcile may have created it; tolerate that.
		var exists types.Volume
		if getErr := b.store.Get(ctx, types.ResourceTypeVolume, ns, name, &exists); getErr == nil {
			return nil
		}
		return err
	}
	return nil
}
