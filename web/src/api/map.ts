/* ============================================================
   proto → screen-shape mappers (pure, total, defensive).

   Every mapper takes a generated protobuf message and returns the
   corresponding interface from ../mock/data so the screens render the same
   shape whether the data is mock or live. Missing fields degrade to sensible
   defaults; no mapper throws.
   ============================================================ */
import type { StatusKey } from "../lib/status";
import type {
  ConfigMap, Instance, Namespace, Node, Policy, Principal,
  Role, Secret, Service, StorageClass, Volume,
} from "../mock/data";

import { Service as PbService, ServiceStatus } from "../gen/pkg/api/proto/service_pb";
import { Instance as PbInstance, InstanceStatus } from "../gen/pkg/api/proto/instance_pb";
import { Namespace as PbNamespace } from "../gen/pkg/api/proto/namespace_pb";
import { Volume as PbVolume, StorageClass as PbStorageClass } from "../gen/pkg/api/proto/storage_pb";
import { Secret as PbSecret } from "../gen/pkg/api/proto/secret_pb";
import { Configmap as PbConfigmap } from "../gen/pkg/api/proto/configmap_pb";
import { Policy as PbPolicy, Subject as PbSubject, TokenInfo as PbTokenInfo } from "../gen/pkg/api/proto/admin_pb";
import { ComponentHealth, HealthStatus } from "../gen/pkg/api/proto/health_pb";

/* ---------------- helpers ---------------- */

/** Compact a human-friendly age string from an RFC-3339 / unix-seconds stamp. */
export function ageFrom(ts: string | number | bigint | undefined): string {
  if (ts === undefined || ts === null || ts === "") return "—";
  let ms: number;
  if (typeof ts === "string") {
    const n = Date.parse(ts);
    if (Number.isNaN(n)) return "—";
    ms = n;
  } else {
    // unix seconds (number or bigint)
    ms = Number(ts) * 1000;
  }
  const diff = Date.now() - ms;
  if (diff < 0 || !Number.isFinite(diff)) return "just now";
  const s = Math.floor(diff / 1000);
  if (s < 60) return "just now";
  const m = Math.floor(s / 60);
  if (m < 60) return `${m}m`;
  const h = Math.floor(m / 60);
  if (h < 24) return `${h}h`;
  const d = Math.floor(h / 24);
  return `${d}d`;
}

/** labels map → ["k:v", …] for display chips. */
function labelChips(labels: Record<string, string> | undefined): string[] {
  if (!labels) return [];
  return Object.entries(labels).map(([k, v]) => (v ? `${k}:${v}` : k));
}

/* ---------------- status enum → StatusKey ---------------- */

export function mapServiceStatus(s: ServiceStatus): StatusKey {
  switch (s) {
    case ServiceStatus.RUNNING: return "run";
    case ServiceStatus.PENDING: return "deploy";
    case ServiceStatus.UPDATING: return "deploy";
    case ServiceStatus.FAILED: return "fail";
    default: return "idle";
  }
}

export function mapInstanceStatus(s: InstanceStatus): StatusKey {
  switch (s) {
    case InstanceStatus.RUNNING: return "run";
    case InstanceStatus.PENDING:
    case InstanceStatus.CREATED:
    case InstanceStatus.STARTING: return "deploy";
    case InstanceStatus.STOPPING:
    case InstanceStatus.STOPPED:
    case InstanceStatus.EXITED:
    case InstanceStatus.DELETED: return "idle";
    case InstanceStatus.FAILED: return "fail";
    default: return "idle";
  }
}

/* ---------------- service ---------------- */

function portLabels(svc: PbService): string[] {
  return (svc.ports ?? []).map((p) => {
    const proto = (p.protocol || "tcp").toLowerCase();
    return `${p.port}/${proto}`;
  });
}

export function mapService(svc: PbService): Service {
  const insts = svc.instances ?? [];
  const ready = insts.filter((i) => i.status === InstanceStatus.RUNNING).length;
  const want = svc.scale ?? insts.length;
  const restarts = insts.reduce((a, i) => a + (i.metadata?.restartCount ?? 0), 0);
  const runtime = (svc.runtime || "container").toLowerCase();
  return {
    name: svc.name || "—",
    ns: svc.namespace || "default",
    type: runtime === "process" ? "process" : "container",
    status: mapServiceStatus(svc.status),
    ready,
    want,
    image: svc.image || (runtime === "process" ? `${svc.command || "process"} (process)` : "—"),
    cpu: 0,
    mem: 0,
    restarts,
    age: ageFrom(svc.metadata?.createdAt),
    ports: portLabels(svc),
    runeset: svc.labels?.["rune.io/runeset"] || svc.labels?.runeset || "—",
    policy: svc.labels?.["rune.io/policy"] || "default",
    note: svc.statusMessage || undefined,
    stateful: (svc.volumes?.length ?? 0) > 0,
    volume: svc.volumes?.[0]?.name || undefined,
  };
}

/* ---------------- instance ---------------- */

export function mapInstance(i: PbInstance, svcNameById?: Map<string, string>): Instance {
  const svc = i.serviceName || (i.serviceId && svcNameById?.get(i.serviceId)) || "—";
  return {
    id: i.name || i.id || "—",
    svc,
    ns: i.namespace || "default",
    node: i.nodeId || i.runner || "—",
    status: mapInstanceStatus(i.status),
    cpu: 0,
    mem: 0,
    restarts: i.metadata?.restartCount ?? 0,
    uptime: i.status === InstanceStatus.RUNNING ? ageFrom(i.createdAt) : "—",
    ip: i.ip || "—",
  };
}

/* ---------------- namespace ---------------- */

export function mapNamespace(
  ns: PbNamespace,
  counts?: { services: number; instances: number },
): Namespace {
  return {
    name: ns.name || "—",
    services: counts?.services ?? 0,
    instances: counts?.instances ?? 0,
    cpu: 0,
    mem: 0,
    labels: labelChips(ns.labels),
    age: ageFrom(ns.createdAt),
  };
}

/* ---------------- storage ---------------- */

function volumeStatusLabel(s: string): string {
  return (s || "unknown").toLowerCase();
}

export function mapVolume(v: PbVolume): Volume {
  const access = v.accessMode || "";
  const mode = access === "ReadWriteMany" ? "RWX" : access === "ReadOnlyMany" ? "ROX" : access ? "RWO" : "—";
  return {
    name: v.name || "—",
    ns: v.namespace || "default",
    size: v.size || "—",
    used: 0,
    status: volumeStatusLabel(v.status),
    class: v.storageClassName || "—",
    node: v.boundNode || "—",
    svc: v.ownerService || v.boundClaim || "—",
    mode,
    age: ageFrom(v.createdAt),
  };
}

export function mapStorageClass(c: PbStorageClass, volumeCount = 0): StorageClass {
  return {
    name: c.name || "—",
    provisioner: c.driver || "—",
    reclaim: c.reclaimPolicy || "—",
    expand: false,
    isDefault: !!c.default,
    volumes: volumeCount,
  };
}

/* ---------------- secret / configmap ---------------- */

/**
 * Map a metadata-only Secret (ListSecrets never returns plaintext). We list
 * key names from data_keys and report byte size 0 — plaintext is sealed and
 * MUST NOT be requested from the UI.
 */
export function mapSecret(s: PbSecret): Secret {
  const keys = (s.dataKeys ?? []).map((k) => ({ k, bytes: 0 }));
  return {
    name: s.name || "—",
    ns: s.namespace || "default",
    type: s.type || "Opaque",
    version: s.version || 1,
    age: ageFrom(s.createdAt),
    updated: ageFrom(s.updatedAt),
    castBy: "—",
    keys,
    mounts: [],
    usedBy: [],
  };
}

export function mapConfigmap(c: PbConfigmap): ConfigMap {
  const entries = Object.entries(c.data ?? {});
  return {
    name: c.name || "—",
    ns: c.namespace || "default",
    version: c.version || 1,
    age: ageFrom(c.createdAt),
    updated: ageFrom(c.updatedAt),
    castBy: "—",
    data: entries.map(([k, value]) => ({ k, value, file: value.includes("\n") })),
    mounts: [],
    usedBy: [],
  };
}

/* ---------------- identity / RBAC ---------------- */

const VERB_SET = (rules: PbPolicy["rules"]): string => {
  const verbs = new Set<string>();
  let resources = new Set<string>();
  for (const r of rules ?? []) {
    (r.verbs ?? []).forEach((v) => verbs.add(v));
    if (r.resource) resources.add(r.resource);
  }
  const v = [...verbs];
  const res = [...resources];
  const verbStr = v.length ? v.join(", ") : "—";
  const resStr = res.length === 0 ? "*" : res.length <= 3 ? res.join(", ") : `${res.length} resources`;
  return `${verbStr} on ${resStr}`;
};

export function mapPolicyToRole(p: PbPolicy, subjects = 0): Role {
  const rules = p.rules ?? [];
  const namespaces = new Set<string>();
  for (const r of rules) if (r.namespace) namespaces.add(r.namespace);
  const scope = namespaces.size === 0 || namespaces.has("*")
    ? "cluster"
    : `namespace: ${[...namespaces].join(", ")}`;
  return {
    name: p.name || "—",
    scope,
    verbs: VERB_SET(rules),
    subjects,
    builtin: !!p.builtin,
  };
}

/**
 * Map a policy to the network-policy table shape (Networking screen). This is a
 * coarse projection: Rune's admin policies are RBAC, not L3/L4 network policy,
 * but the screen renders them as the cluster's authorization rules.
 */
export function mapPolicy(p: PbPolicy): Policy {
  const rules = p.rules ?? [];
  const namespaces = new Set<string>();
  const resources = new Set<string>();
  for (const r of rules) {
    if (r.namespace) namespaces.add(r.namespace);
    if (r.resource) resources.add(r.resource);
  }
  const ns = namespaces.size === 0 || namespaces.has("*") ? "cluster" : [...namespaces].join(", ");
  return {
    name: p.name || "—",
    ns,
    direction: "rbac",
    targets: resources.size === 0 ? "all resources" : [...resources].slice(0, 4).join(", "),
    rules: rules.length,
    mode: rules.length === 0 ? "deny" : "allow",
    desc: p.description || undefined,
  };
}

export function mapPrincipal(s: PbSubject, roleName?: string): Principal {
  const kind = (s.kind || "user").toLowerCase();
  const machine = kind === "service" || kind === "machine";
  return {
    name: s.name || s.id || "—",
    kind: machine ? "service account" : "user",
    email: s.email || (machine ? `system:${s.name}` : "—"),
    role: roleName || s.policies?.[0] || "—",
    mfa: false,
    last: "—",
    type: machine ? "machine" : "human",
  };
}

/** Map a token to a machine principal (service accounts surface as tokens). */
export function mapTokenPrincipal(t: PbTokenInfo): Principal {
  const machine = (t.subjectType || "").toLowerCase() !== "user";
  return {
    name: t.name || t.id || "—",
    kind: machine ? "service account" : "user",
    email: t.description || `token:${t.id}`,
    role: t.kind || "—",
    mfa: false,
    last: t.lastUsedAt ? ageFrom(Number(t.lastUsedAt)) + " ago" : "never",
    type: machine ? "machine" : "human",
  };
}

/* ---------------- health / overview ---------------- */

export function healthStatusKey(s: HealthStatus): StatusKey {
  switch (s) {
    case HealthStatus.HEALTHY: return "run";
    case HealthStatus.DEGRADED: return "warn";
    case HealthStatus.UNHEALTHY: return "fail";
    default: return "idle";
  }
}

/** Map a "node"-type ComponentHealth to the Node card shape. */
export function mapNodeHealth(c: ComponentHealth, instanceCount = 0): Node {
  const res = c.resources;
  const cpuPct = res && res.cpuUsedPercent >= 0 ? Math.round(res.cpuUsedPercent) : 0;
  const memPct = res && res.memTotalBytes > 0n
    ? Math.round((Number(res.memUsedBytes) / Number(res.memTotalBytes)) * 100)
    : 0;
  return {
    name: c.name || c.id || "node",
    role: "node",
    status: c.status === HealthStatus.HEALTHY ? "ready" : "degraded",
    cpu: cpuPct,
    mem: memPct,
    instances: instanceCount,
    addr: "—",
  };
}
