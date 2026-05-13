# Init Steps

Init steps are one-shot containers (or processes) that run **before**
the main container of a service starts. Each init step finishes — with
exit code 0 — before the next step or the main container starts. Use
them for one-time setup that the main process can't do for itself:
formatting a fresh volume, running a database migration, seeding
config from an external system, or waiting on a slow dependency.

## A first example

```yaml
service:
  name: api
  namespace: app
  image: my-api:latest
  volumes:
    - name: data
      mountPath: /var/lib/api
      claimTemplate:
        storageClassName: local
        size: "1Gi"
  initSteps:
    - name: migrate
      image: my-api:latest
      command: /usr/local/bin/migrate
      args: ["up"]
```

Every time the instance starts, Rune walks `initSteps` in declaration
order. Each step has its own container with its own image but inherits
the parent service's volumes, secret mounts, configmap mounts and
environment by default. The main `api` container starts only after
`migrate` exits 0.

### `command` vs `args` — Kubernetes semantics

`command` and `args` follow Kubernetes conventions, **not** Docker's
shell form:

- `command` is a single string. It **replaces** the image's
  `ENTRYPOINT`.
- `args` is a list of strings. It **replaces** the image's `CMD`
  and becomes the arguments passed to `command`.

This matters for images that wrap their real binary in something
like `tini --` or a custom entrypoint script. The step's `command`
will be used verbatim, so you do not need to re-state the
entrypoint chain:

```yaml
# Image: ghcr.io/tigerbeetle/tigerbeetle (ENTRYPOINT: tini -- /tigerbeetle)
# Container will run exactly: /tigerbeetle format --cluster=0 /data/0_0.tigerbeetle
initSteps:
  - name: format
    image: ghcr.io/tigerbeetle/tigerbeetle:0.16.30
    command: /tigerbeetle
    args: ["format", "--cluster=0", "/data/0_0.tigerbeetle"]
```

## When does a step run? — `runIf`

Init steps don't need to run on every start. `runIf` selects the
predicate; the default is `freshVolume`, which is the right choice for
"format on first boot" patterns:

| `type` | Runs when | Required fields |
| --- | --- | --- |
| `freshVolume` *(default)* | The instance's parent volume has never been initialised for this service. | — |
| `fileMissing` | A path inside a parent volume does not exist. | `path` (absolute), optionally `volume` |
| `always` | Every start. | — |

`freshVolume` is anchored on `Volume.Status.InitializedFor[<serviceID>]`,
which Rune stamps after the step succeeds. Restarting the instance
won't re-run the step; deleting and re-creating the volume will.

```yaml
initSteps:
  - name: format
    image: ghcr.io/tigerbeetle/tigerbeetle:0.16.30
    command: /tigerbeetle
    args: ["format", "--cluster=0", "--replica=0", "--replica-count=1", "/data/0_0.tigerbeetle"]
    runIf:
      type: fileMissing
      path: /data/0_0.tigerbeetle
```

## Filtering inherited mounts

A step inherits all parent volumes, secret mounts, and configmap
mounts by default. You can filter that down per step:

```yaml
initSteps:
  - name: seed
    image: my-tool:latest
    command: /seed
    volumes: [data]            # only mount the parent's "data" volume
    secretMounts: []           # mount no secrets, even if parent has some
    configmapMounts: [defaults] # only mount the "defaults" configmap
```

The convention:

- **omitted** (nil) → inherit all
- **`[]`** (empty) → inherit none
- **`[a, b]`** → only `a` and `b` from the parent

Each name must match a `name` declared on the parent service.

## Restart policy and timeout

```yaml
initSteps:
  - name: migrate
    image: my-api:latest
    command: /migrate
    restartPolicy: OnFailure   # default; bounded retries inside the step
    timeout: 5m                # 0 defers to the cast-level timeout
```

`restartPolicy: Never` fails the instance on any non-zero exit.

## Resources

Init for databases is often more I/O- and CPU-heavy than steady state.
You can override the parent's resource block for a single step:

```yaml
initSteps:
  - name: format
    image: tb
    command: /tigerbeetle
    args: ["format", "..."]
    resources:
      cpu:    { request: "2",    limit: "4"   }
      memory: { request: "2Gi",  limit: "4Gi" }
```

If `resources` is omitted, the step inherits the parent service's
`resources`.

## Security context (seccomp, capabilities, privileged)

Some workloads need to opt out of Docker's default seccomp profile or
add Linux capabilities. TigerBeetle's `format` uses `io_uring_setup(2)`,
which the default profile blocks; ScyllaDB and recent async runtimes
have similar requirements.

```yaml
initSteps:
  - name: format
    image: ghcr.io/tigerbeetle/tigerbeetle:0.16.30
    command: /tigerbeetle
    args: ["format", "--cluster=0", "--replica=0", "--replica-count=1", "/data/0_0.tigerbeetle"]
    securityContext:
      seccompProfile:
        type: unconfined
```

You can also use `securityContext` at the **service** level for the
main container.

Supported fields:

| Field | Effect |
| --- | --- |
| `seccompProfile.type` | `default` *(runtime default)*, `unconfined`, or `localhost`. |
| `seccompProfile.localhostProfile` | Absolute path to a JSON profile on the host. Required iff `type: localhost`. |
| `capAdd` | Linux capabilities to add (e.g. `SYS_NICE`, `NET_ADMIN`). |
| `capDrop` | Linux capabilities to drop. Applied after `capAdd`. |
| `privileged` | Full access to host devices and namespaces. |

### The `services.privileged` policy verb

`privileged: true` and `seccompProfile.type: unconfined` are
admin-gated. The Rune server rejects `CreateService` /
`UpdateService` requests that use them unless the calling subject
has the `services.privileged` verb. The built-in `readwrite` policy
grants `services.create` and `services.update` but **not**
`services.privileged`; the `root` policy (verb `*`) grants
everything.

Grant the verb to a specific user or token if you need to deploy a
workload that requires it, rather than handing out `root`.

## Observability

```bash
# Per-step status and progress on the service detail view
rune get service tb -n default

# Stream a step's combined stdout/stderr
rune logs tb --init-step format
```

Init step output is buffered up to 32 KiB into the structured log
when the step completes. Steps that need long-form streaming logs
will graduate to the main logging pipeline in a future revision.

## Process runtime

For services with `runtime: process` (i.e., not container-based),
init steps run as a subprocess of `runed` under the parent's process
context. `image` must be empty; `command` and `args` apply as usual.
`securityContext` (in the container sense) does not apply — use the
process runtime's own `securityContext` block on the parent for
UID/GID controls.

## Validation rules at a glance

- `name` must be a DNS-1123 label, unique within `initSteps`.
- `image` is required for container runtime; must be empty for process runtime.
- `command` is required.
- `runIf.type=freshVolume` requires the step to mount at least one parent volume.
- `runIf.type=fileMissing` requires `path` to be absolute and the step to mount a parent volume.
- `runIf.path` / `runIf.volume` are only valid with `type=fileMissing`.
- Volume / secretMount / configmapMount filter entries must reference parent names.
- `securityContext.seccompProfile.localhostProfile` must be an absolute path.

The CLI validates these at `rune cast` time before sending the
request. Policy-gated fields (`privileged`, `seccomp=unconfined`)
are additionally enforced server-side.

## Troubleshooting

**`unknown field 'initSteps' in service specification`** — your CLI
predates v0.0.1-dev.38. Update with `rune version` to confirm,
then upgrade.

**Service starts running but `rune logs --init-step` reports
"has no init steps"** — same root cause: a pre-v0.0.1-dev.38
client/server pair drops `initSteps` at the wire. Both ends must be
v0.0.1-dev.38 or newer.

**`access denied for resource: services verb: privileged`** — your
token has `services.create` but not `services.privileged`. Either
remove the privileged knob from the payload, or have an admin grant
the verb.

**Init container exits with `Operation not permitted` on `io_uring_setup`** —
Docker's default seccomp profile is blocking io_uring. Set
`securityContext.seccompProfile.type: unconfined` on the init step
(and ensure your token has `services.privileged`).

**Init step fails with `unknown subcommand: '<path>'` or sees its own
binary as the first argument** — your CLI/server pre-dates
v0.0.1-dev.39. Before the fix, `command` was appended to the
image's `ENTRYPOINT` instead of replacing it, so e.g.
`ENTRYPOINT ["tini", "--", "/foo"]` + `command: /foo` produced
`tini -- /foo /foo <args>`. Upgrade both client and server.
