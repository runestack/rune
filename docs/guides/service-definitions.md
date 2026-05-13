# Service Definitions

This guide explains how to define services in Rune, including declaring dependencies between services.

## Basic service

```yaml
name: api
image: nginx:alpine
scale: 1
```

## Resources (CPU and Memory)

Rune follows Kubernetes-style resource strings. Both `request` and `limit` are optional and may be set independently. CPU values represent cores; memory values represent bytes with unit suffixes.

Example:

```yaml
name: api
image: my-api:latest
resources:
  cpu:
    request: "500m"   # 0.5 cores
    limit:   "1"      # 1 core
  memory:
    request: "256Mi"  # 268,435,456 bytes
    limit:   "2Gi"    # 2,147,483,648 bytes
```

CPU formats:
- "1" → 1 core (vCPU)
- "0.5" → 0.5 cores
- "500m" → 0.5 cores (millicores)

Memory formats (case-insensitive):
- Binary units: Ki, Mi, Gi, Ti, Pi, Ei (base-2). Example: `2Gi` (2,147,483,648 bytes)
- SI units: K, M, G, T, P, E (base-10). Example: `2.5G` (2,500,000,000 bytes)

Notes:
- If a value is omitted it is treated as 0 for scheduling/limits.
- Use binary units (Gi/Mi) for exact powers-of-two sizing; use SI (G/M) for decimal sizing.

## Declaring dependencies (MVP)

Dependencies can be declared using simple strings (same namespace), FQDN-like strings (service.namespace), or structured objects.

```yaml
name: api
namespace: default
image: my-api:latest
dependencies:
  - "db"                 # same namespace
  - "cache.shared"      # cross-namespace (service.namespace)
  - service: auth        # structured form
    namespace: security
```

Rune normalizes these into `service` and `namespace`. If `namespace` is omitted, it defaults to the service's own namespace.

### Readiness semantics (MVP)

- A dependency is Ready if:
  - The dependency service defines a readiness probe and at least one instance is Running and readiness=true; or
  - No readiness probe is defined and at least one instance is Running.
- The orchestrator delays instance creation for a service with dependencies until all dependencies are Ready.

### Delete safety (MVP)

- Deleting a service is blocked if other services depend on it.
- You can override with:

```bash
rune delete service <name> --no-dependencies
```

### CLI helpers

```bash
rune deps validate <service>           # format, existence, cycle checks
rune deps graph <service> --format=dot # visualize dependency graph
rune deps check <service>              # readiness check for dependencies
rune deps dependents <service>         # list services that depend on target
```

## Init steps

Init steps are one-shot containers that run before the main container
on each instance start. Use them for migrations, formatting a fresh
volume, or any one-time setup the main process can't do itself.

```yaml
service:
  name: api
  image: my-api:latest
  volumes:
    - name: data
      mountPath: /var/lib/api
      claimTemplate: { storageClassName: local, size: "1Gi" }
  initSteps:
    - name: migrate
      image: my-api:latest
      command: /usr/local/bin/migrate
      args: ["up"]
```

Each step gets its own image but inherits the parent's volumes,
secret mounts, configmap mounts, and environment by default. The
main container starts only after every applicable step exits 0.

The `runIf` field selects when a step runs (`freshVolume` is the
default, `fileMissing` and `always` are the other options). Steps
can override resources, restart policy, and timeout, and can filter
which inherited mounts they actually attach.

See the full guide: [Init Steps](init-steps.md).

## Security context

Use `securityContext` to opt out of the default seccomp profile, add
or drop Linux capabilities, or run privileged. Available on the
service level (applied to the main container) and on each init step.

```yaml
service:
  name: tigerbeetle
  image: ghcr.io/tigerbeetle/tigerbeetle:0.16.30
  # ...
  initSteps:
    - name: format
      image: ghcr.io/tigerbeetle/tigerbeetle:0.16.30
      command: /tigerbeetle
      args: ["format", "--cluster=0", "--replica=0", "--replica-count=1", "/data/0_0.tigerbeetle"]
      securityContext:
        seccompProfile: { type: unconfined }
```

Supported fields:

- `seccompProfile.type` — `default`, `unconfined`, or `localhost`
- `seccompProfile.localhostProfile` — host path to a JSON profile (with `type: localhost`)
- `capAdd` / `capDrop` — Linux capability names
- `privileged` — full access to host devices and namespaces

> **Admin-gated**: `privileged: true` and `seccompProfile.type: unconfined`
> require the `services.privileged` policy verb. The built-in
> `readwrite` policy does not grant it; `root` does. Without the
> verb the server returns `PermissionDenied: access denied for
> resource: services verb: privileged` from
> `rune cast` / `rune update`.

See [Init Steps → Security context](init-steps.md#security-context-seccomp-capabilities-privileged)
for the full table and troubleshooting.

