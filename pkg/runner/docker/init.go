// Package docker — RUNE-121 init-step execution.
package docker

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"path/filepath"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/mount"

	"github.com/runestack/rune/pkg/log"
	runetypes "github.com/runestack/rune/pkg/types"
)

// initLogTailLimit caps how much of the step's combined stdout/stderr
// the runner buffers into the structured log. Init steps that need
// long-form logging will get a real log subsystem in S6 (RUNE-121).
const initLogTailLimit = 32 * 1024

// initContainerNamePrefix labels one-shot init containers so
// tryRemoveConflictingContainer / List / GC can recognise them.
const initContainerNamePrefix = "rune-init-"

// RunInit implements runner.Runner.RunInit for Docker. It runs one
// InitStep as a one-shot container that shares the parent instance's
// resolved volume / secret / config mounts (filtered per the step
// spec) and waits for it to terminate.
//
// On normal termination RunInit returns the container's exit code
// (zero on success). On RuntimeError (image pull failure, container
// create/start failure, log capture failure, etc.) it returns a
// non-nil error and the exit code is undefined.
//
// Cancellation of ctx kills the step container and removes it.
func (r *DockerRunner) RunInit(ctx context.Context, instance *runetypes.Instance, step runetypes.InitStep) (int, error) {
	if instance == nil {
		return 0, fmt.Errorf("invalid instance: nil pointer")
	}
	if step.Name == "" {
		return 0, fmt.Errorf("invalid init step: name is empty")
	}
	if step.Image == "" {
		return 0, fmt.Errorf("invalid init step %q: image is required for docker runner", step.Name)
	}
	if step.Command == "" {
		return 0, fmt.Errorf("invalid init step %q: command is required", step.Name)
	}

	// Honour the parent instance's image-pull policy. Init step images
	// are typically the same as the parent (e.g. tigerbeetle running
	// `format` then `start`) so the cache hit is the common case.
	pullPolicy := runetypes.ImagePullAlways
	if instance.Metadata != nil && instance.Metadata.ImagePull != "" {
		pullPolicy = instance.Metadata.ImagePull
	}
	if err := r.pullImage(ctx, step.Image, pullPolicy); err != nil {
		return 0, fmt.Errorf("init step %q: failed to pull image %s: %w", step.Name, step.Image, err)
	}

	containerConfig, hostConfig, err := r.initStepToContainerConfig(instance, step)
	if err != nil {
		return 0, fmt.Errorf("init step %q: %w", step.Name, err)
	}

	// One-shot containers get their own deterministic name so they're
	// identifiable in `docker ps -a` and don't collide with the main
	// container. Append a short timestamp suffix so retries (S3) don't
	// clash on the same name.
	name := fmt.Sprintf("%s%s-%s-%d",
		initContainerNamePrefix, instance.Name, step.Name, time.Now().UnixNano())

	resp, err := r.client.ContainerCreate(ctx, containerConfig, hostConfig, nil, nil, name)
	if err != nil {
		return 0, fmt.Errorf("init step %q: failed to create container: %w", step.Name, err)
	}
	containerID := resp.ID

	// Always remove the container before returning, even on error. Logs
	// have already been captured inline, so removal is safe.
	defer func() {
		// Use a fresh context so cleanup runs even if ctx was cancelled.
		rmCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if rmErr := r.client.ContainerRemove(rmCtx, containerID, container.RemoveOptions{Force: true}); rmErr != nil {
			r.logger.Warn("Failed to remove init step container",
				log.Str("container_id", containerID),
				log.Str("instance_id", instance.ID),
				log.Str("step", step.Name),
				log.Err(rmErr))
		}
	}()

	if err := r.client.ContainerStart(ctx, containerID, container.StartOptions{}); err != nil {
		return 0, fmt.Errorf("init step %q: failed to start container: %w", step.Name, err)
	}

	// Wait for the container to exit. Any of:
	//   - exit (success or non-zero)
	//   - context cancellation (caller deadline / explicit cancel)
	//   - docker error
	statusCh, errCh := r.client.ContainerWait(ctx, containerID, container.WaitConditionNotRunning)

	var exitCode int
	select {
	case <-ctx.Done():
		// Cancelled or deadline exceeded. Stop the container so the
		// deferred remove succeeds quickly.
		stopCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		zero := 0
		_ = r.client.ContainerStop(stopCtx, containerID, container.StopOptions{Timeout: &zero})
		// Capture whatever logs we have before returning.
		r.captureInitLogs(stopCtx, containerID, instance, step)
		return 0, fmt.Errorf("init step %q: cancelled: %w", step.Name, ctx.Err())

	case waitErr := <-errCh:
		if waitErr != nil {
			r.captureInitLogs(ctx, containerID, instance, step)
			return 0, fmt.Errorf("init step %q: container wait failed: %w", step.Name, waitErr)
		}
		// Channel closed without an error — fall through and try statusCh.
	case st := <-statusCh:
		if st.Error != nil {
			r.captureInitLogs(ctx, containerID, instance, step)
			return 0, fmt.Errorf("init step %q: container error: %s", step.Name, st.Error.Message)
		}
		exitCode = int(st.StatusCode)
	}

	// Capture logs for surfacing via structured logging. S6 will route
	// these to the log subsystem so `rune logs svc/<step>` returns
	// them.
	r.captureInitLogs(ctx, containerID, instance, step)

	r.logger.Info("Init step completed",
		log.Str("instance_id", instance.ID),
		log.Str("instance_name", instance.Name),
		log.Str("step", step.Name),
		log.Int("exit_code", exitCode))

	return exitCode, nil
}

// initStepToContainerConfig builds the docker create-container payload
// for one InitStep, filtering the parent instance's resolved mounts
// and merging step env on top of instance env.
func (r *DockerRunner) initStepToContainerConfig(instance *runetypes.Instance, step runetypes.InitStep) (*container.Config, *container.HostConfig, error) {
	// Build env: parent first, step overlays.
	env := make(map[string]string, len(instance.Environment)+len(step.Env))
	for k, v := range instance.Environment {
		env[k] = v
	}
	for k, v := range step.Env {
		env[k] = v
	}

	// Kubernetes semantics: step.Command → Entrypoint (replacing the
	// image's ENTRYPOINT), step.Args → Cmd. Without the explicit
	// Entrypoint override, Docker preserves the image's ENTRYPOINT and
	// appends our Cmd as its arguments — so e.g. images that wrap their
	// real binary in tini would run `tini -- /bin /bin <args...>` and
	// the binary would see itself as its first arg (RUNE-121 Bug C).
	containerConfig := &container.Config{
		Image:      step.Image,
		Entrypoint: []string{step.Command},
		Cmd:        append([]string(nil), step.Args...),
		Env:   formatEnvVars(env),
		Labels: map[string]string{
			"rune.managed":      "true",
			"rune.kind":         "init-step",
			"rune.namespace":    instance.Namespace,
			"rune.instance.id":  instance.ID,
			"rune.service.id":   instance.ServiceID,
			"rune.service.name": instance.ServiceName,
			"rune.init.step":    step.Name,
		},
		// Init steps must terminate. Tty would interfere with our log
		// capture and gives the wrong impression that the step is
		// interactive.
		Tty: false,
	}

	hostConfig := &container.HostConfig{}

	// Apply resources: step override wins, otherwise inherit from instance.
	res := instance.Resources
	if step.Resources != nil {
		res = step.Resources
	}
	if res != nil {
		applyResourceLimits(hostConfig, res)
	}

	// Filtered volume mounts. A nil filter (step.Volumes == nil)
	// inherits all parent volumes; an empty non-nil filter mounts none.
	if instance.Metadata != nil && len(instance.Metadata.VolumeMounts) > 0 {
		want := makeNameFilter(step.Volumes)
		for _, vm := range instance.Metadata.VolumeMounts {
			if !want(vm.Name) {
				continue
			}
			if vm.Source == "" || vm.MountPath == "" {
				return nil, nil, fmt.Errorf("invalid parent volume mount %q: source or mountPath empty", vm.Name)
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

	// Filtered secret mounts.
	if instance.Metadata != nil && len(instance.Metadata.SecretMounts) > 0 {
		want := makeNameFilter(step.SecretMounts)
		filtered := make([]runetypes.ResolvedSecretMount, 0, len(instance.Metadata.SecretMounts))
		for _, sm := range instance.Metadata.SecretMounts {
			if want(sm.Name) {
				filtered = append(filtered, sm)
			}
		}
		if len(filtered) > 0 {
			secretMounts, err := r.prepareSecretMounts(filtered)
			if err != nil {
				return nil, nil, fmt.Errorf("failed to prepare secret mounts: %w", err)
			}
			hostConfig.Mounts = append(hostConfig.Mounts, secretMounts...)
		}
	}

	// Filtered config mounts.
	if instance.Metadata != nil && len(instance.Metadata.ConfigmapMounts) > 0 {
		want := makeNameFilter(step.ConfigmapMounts)
		filtered := make([]runetypes.ResolvedConfigmapMount, 0, len(instance.Metadata.ConfigmapMounts))
		for _, cm := range instance.Metadata.ConfigmapMounts {
			if want(cm.Name) {
				filtered = append(filtered, cm)
			}
		}
		if len(filtered) > 0 {
			configMounts, err := r.prepareConfigmapsMounts(filtered)
			if err != nil {
				return nil, nil, fmt.Errorf("failed to prepare config mounts: %w", err)
			}
			hostConfig.Mounts = append(hostConfig.Mounts, configMounts...)
		}
	}

	// Init steps deliberately do NOT inherit DNS / network config of
	// the main service. They run in the default bridge — bootstrap
	// flows that need to talk to the cluster will get an opt-in
	// `network: serviceMesh` field in a future revision (RUNE-121
	// design §13).

	// Apply optional security context (seccomp / capabilities /
	// privileged). Privileged and seccomp=unconfined are gated
	// server-side; the runner only enforces structural correctness.
	applySecurityContext(hostConfig, step.SecurityContext)

	return containerConfig, hostConfig, nil
}

// applySecurityContext maps a types.SecurityContext onto a Docker
// HostConfig. Nil is a no-op so containers without a context retain
// runtime defaults.
func applySecurityContext(hostConfig *container.HostConfig, sc *runetypes.SecurityContext) {
	if sc == nil {
		return
	}
	if sc.Privileged {
		hostConfig.Privileged = true
	}
	if len(sc.CapAdd) > 0 {
		hostConfig.CapAdd = append(hostConfig.CapAdd, sc.CapAdd...)
	}
	if len(sc.CapDrop) > 0 {
		hostConfig.CapDrop = append(hostConfig.CapDrop, sc.CapDrop...)
	}
	if sp := sc.SeccompProfile; sp != nil {
		switch sp.Type {
		case runetypes.SeccompProfileUnconfined:
			hostConfig.SecurityOpt = append(hostConfig.SecurityOpt, "seccomp=unconfined")
		case runetypes.SeccompProfileLocalhost:
			if sp.LocalhostProfile != "" {
				hostConfig.SecurityOpt = append(hostConfig.SecurityOpt, "seccomp="+sp.LocalhostProfile)
			}
		}
	}
}

// makeNameFilter returns a predicate that matches names per the
// init-step filter convention:
//
//   - filter == nil      → inherit all
//   - filter == []       → inherit none
//   - filter == [a, b]   → inherit a and b
func makeNameFilter(filter []string) func(string) bool {
	if filter == nil {
		return func(string) bool { return true }
	}
	if len(filter) == 0 {
		return func(string) bool { return false }
	}
	set := make(map[string]struct{}, len(filter))
	for _, n := range filter {
		set[n] = struct{}{}
	}
	return func(n string) bool {
		_, ok := set[n]
		return ok
	}
}

// applyResourceLimits maps a Resources spec to docker host config. It
// mirrors the logic in instanceToContainerConfig so init steps and
// main containers share the same enforcement semantics.
func applyResourceLimits(hostConfig *container.HostConfig, res *runetypes.Resources) {
	cpuReqCores, _ := runetypes.ParseCPU(res.CPU.Request)
	cpuLimCores, _ := runetypes.ParseCPU(res.CPU.Limit)
	memReqBytes, _ := runetypes.ParseMemory(res.Memory.Request)
	memLimBytes, _ := runetypes.ParseMemory(res.Memory.Limit)

	if cpuReqCores > 0 {
		shares := int64(cpuReqCores * 1024)
		if shares < 2 {
			shares = 2
		}
		hostConfig.Resources.CPUShares = shares
	}
	if cpuLimCores > 0 {
		hostConfig.Resources.NanoCPUs = int64(cpuLimCores * 1e9)
		if hostConfig.Resources.NanoCPUs == 0 {
			hostConfig.Resources.CPUPeriod = 100000
			hostConfig.Resources.CPUQuota = int64(cpuLimCores * float64(hostConfig.Resources.CPUPeriod))
		}
	}
	if memReqBytes > 0 {
		hostConfig.Resources.MemoryReservation = memReqBytes
	}
	if memLimBytes > 0 {
		hostConfig.Resources.Memory = memLimBytes
	}
}

// captureInitLogs reads the container's combined stdout/stderr (capped
// at initLogTailLimit) and emits it via the structured logger. It is
// best-effort: if log retrieval fails, we log the failure and move on.
//
// S6 of RUNE-121 will route these into the log subsystem so they're
// addressable as `rune logs svc/<step>`.
func (r *DockerRunner) captureInitLogs(ctx context.Context, containerID string, instance *runetypes.Instance, step runetypes.InitStep) {
	logs, err := r.client.ContainerLogs(ctx, containerID, container.LogsOptions{
		ShowStdout: true,
		ShowStderr: true,
		Tail:       "all",
	})
	if err != nil {
		r.logger.Warn("Failed to fetch init step logs",
			log.Str("instance_id", instance.ID),
			log.Str("step", step.Name),
			log.Err(err))
		return
	}
	defer logs.Close()

	var buf bytes.Buffer
	// Cap reads at initLogTailLimit + 1 so we know if we truncated.
	limited := io.LimitReader(logs, initLogTailLimit+1)
	if _, err := io.Copy(&buf, limited); err != nil {
		r.logger.Warn("Failed to read init step logs",
			log.Str("instance_id", instance.ID),
			log.Str("step", step.Name),
			log.Err(err))
		return
	}
	output := buf.Bytes()
	truncated := false
	if len(output) > initLogTailLimit {
		output = output[:initLogTailLimit]
		truncated = true
	}
	r.logger.Info("Init step output",
		log.Str("instance_id", instance.ID),
		log.Str("instance_name", instance.Name),
		log.Str("step", step.Name),
		log.Bool("truncated", truncated),
		log.Str("output", string(output)))
}
