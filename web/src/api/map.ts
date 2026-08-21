/* ============================================================
   proto → screen-shape mappers (pure, total, defensive).

   Every mapper takes a generated protobuf message and returns the
   corresponding interface from ./types so the screens render the same
   shape whether the data is mock or live. Missing fields degrade to sensible
   defaults; no mapper throws.
   ============================================================ */
import type { StatusKey } from "../lib/status";
import type {
  ConfigMap, Ingress, Instance, Namespace, NetPolicy, Node, Policy, Principal,
  Role, Secret, Service, StorageClass, Volume,
} from "../api/types";

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

/**
 * Aggregate live usage across a service's embedded instances.
 *
 * CPU: instance cpu_percent is already host-share (same denominator as node
 * CPU), so the service total is the SUM across instances, clamped to 100.
 *
 * Memory: percent = Σ used / denominator. When every instance reports the
 * same limit (the common cases: identical caps, or uncapped where the limit
 * equals host total) the denominator is that shared limit; otherwise it's
 * the sum of limits.
 */
function aggregateUsage(insts: PbInstance[]): { cpu: number; mem: number } {
  let cpu = 0;
  let usedSum = 0;
  let limitSum = 0;
  let sharedLimit = -1; // -1 unset · -2 mixed
  let any = false;
  for (const i of insts) {
    const u = i.usage;
    if (!u) continue;
    any = true;
    if (u.cpuPercent >= 0) cpu += u.cpuPercent;
    const used = Number(u.memUsedBytes);
    const limit = Number(u.memLimitBytes);
    if (limit > 0) {
      usedSum += used;
      limitSum += limit;
      if (sharedLimit === -1) sharedLimit = limit;
      else if (sharedLimit !== limit) sharedLimit = -2;
    }
  }
  if (!any) return { cpu: 0, mem: 0 };
  const denom = sharedLimit > 0 ? sharedLimit : limitSum;
  const mem = denom > 0 ? (usedSum / denom) * 100 : 0;
  return { cpu: Math.min(100, Math.round(cpu)), mem: Math.min(100, Math.round(mem)) };
}

export function mapService(svc: PbService): Service {
  const insts = svc.instances ?? [];
  const ready = insts.filter((i) => i.status === InstanceStatus.RUNNING).length;
  const want = svc.scale ?? insts.length;
  const restarts = insts.reduce((a, i) => a + (i.metadata?.restartCount ?? 0), 0);
  const runtime = (svc.runtime || "container").toLowerCase();
  const usage = aggregateUsage(insts);
  return {
    name: svc.name || "—",
    ns: svc.namespace || "default",
    type: runtime === "process" ? "process" : "container",
    status: mapServiceStatus(svc.status),
    ready,
    want,
    image: svc.image || (runtime === "process" ? `${svc.command || "process"} (process)` : "—"),
    cpu: usage.cpu,
    mem: usage.mem,
    restarts,
    age: ageFrom(svc.metadata?.createdAt),
    ports: portLabels(svc),
    runeset: svc.labels?.["rune.io/runeset"] || svc.labels?.runeset || "—",
    policy: svc.labels?.["rune.io/policy"] || "default",
    note: svc.statusMessage || undefined,
    reason: svc.statusReason || undefined,
    update: svc.update
      ? {
          replaced: svc.update.updatedReady,
          serving: svc.update.available,
          desired: svc.update.desired,
          message: svc.update.message || undefined,
        }
      : undefined,
    stateful: (svc.volumes?.length ?? 0) > 0,
    volume: svc.volumes?.[0]?.name || undefined,
    ingress: mapIngress(svc),
    netpol: mapNetPolicy(svc),
  };
}

/** Build the ingress view from expose.host (+ TLS + async cert). Returns
 *  undefined when the service isn't externally exposed. */
function mapIngress(svc: PbService): Ingress | undefined {
  const ex = svc.expose;
  if (!ex?.host) return undefined;
  const tls = ex.tls ? (ex.tls.mode || (ex.tls.auto ? "auto" : "manual")) : "";
  const scheme = ex.tls ? "https" : "http";
  const path = ex.path && ex.path !== "/" ? ex.path : "";
  const c = svc.ingressCert;
  return {
    host: ex.host,
    url: `${scheme}://${ex.host}${path}`,
    tls,
    path: ex.path || undefined,
    cert: c?.host
      ? { state: c.state || "Pending", expiresAt: c.expiresAt || undefined, lastError: c.lastError || undefined }
      : undefined,
  };
}

function mapNetPolicy(svc: PbService): NetPolicy | undefined {
  const np = svc.networkPolicy;
  const ingress = np?.ingress?.length ?? 0;
  const egress = np?.egress?.length ?? 0;
  if (!ingress && !egress) return undefined;
  return { ingress, egress };
}

/* ---------------- instance ---------------- */

export function mapInstance(i: PbInstance, svcNameById?: Map<string, string>): Instance {
  const svc = i.serviceName || (i.serviceId && svcNameById?.get(i.serviceId)) || "—";
  // Live usage from the runner (absent = unknown → 0). CPU is host-share
  // (same denominator as node CPU); memory percent is used/limit, where an
  // uncapped container's limit equals the host total.
  const u = i.usage;
  const cpu = u && u.cpuPercent >= 0 ? Math.round(u.cpuPercent) : 0;
  const memLimit = u ? Number(u.memLimitBytes) : 0;
  const mem = u && memLimit > 0 ? Math.min(100, Math.round((Number(u.memUsedBytes) / memLimit) * 100)) : 0;
  return {
    id: i.name || i.id || "—",
    svc,
    ns: i.namespace || "default",
    node: i.nodeId || i.runner || "—",
    status: mapInstanceStatus(i.status),
    cpu,
    mem,
    restarts: i.metadata?.restartCount ?? 0,
    uptime: i.status === InstanceStatus.RUNNING ? ageFrom(i.createdAt) : "—",
    ip: i.ip || "—",
  };
}

/* ---------------- namespace ---------------- */

export function mapNamespace(
  ns: PbNamespace,
  counts?: { services: number; instances: number; secrets: number; configs: number; volumes: number },
): Namespace {
  return {
    name: ns.name || "—",
    services: counts?.services ?? 0,
    instances: counts?.instances ?? 0,
    secrets: counts?.secrets ?? 0,
    configs: counts?.configs ?? 0,
    volumes: counts?.volumes ?? 0,
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
  // Live usage from the UsageReporter driver capability; capacity falls back
  // to the spec size server-side. 0 capacity = unknown → render 0%.
  const capacity = Number(v.capacityBytes);
  const used = capacity > 0 ? Math.min(100, Math.round((Number(v.usedBytes) / capacity) * 100)) : 0;
  return {
    name: v.name || "—",
    ns: v.namespace || "default",
    size: v.size || "—",
    used,
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
