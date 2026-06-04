/* ============================================================
   RUNE — mock cluster data (invented, faithful to Rune's model)
   Swapped for live connect-web data in D6.
   ============================================================ */
import type { StatusKey } from "../lib/status";

export interface Node {
  name: string;
  role: string;
  status: string;
  cpu: number;
  mem: number;
  instances: number;
  addr: string;
}
export interface Service {
  name: string;
  ns: string;
  type: "container" | "process";
  status: StatusKey;
  ready: number;
  want: number;
  image: string;
  cpu: number;
  mem: number;
  restarts: number;
  age: string;
  ports: string[];
  runeset: string;
  policy: string;
  note?: string;
  stateful?: boolean;
  volume?: string;
  schedule?: string;
}
export interface Instance {
  id: string;
  svc: string;
  ns: string;
  node: string;
  status: StatusKey;
  cpu: number;
  mem: number;
  restarts: number;
  uptime: string;
  ip: string;
}
export interface Namespace {
  name: string;
  services: number;
  instances: number;
  cpu: number;
  mem: number;
  labels: string[];
  age: string;
}
export interface Volume {
  name: string;
  ns: string;
  size: string;
  used: number;
  status: string;
  class: string;
  node: string;
  svc: string;
  mode: string;
  age: string;
}
export interface StorageClass {
  name: string;
  provisioner: string;
  reclaim: string;
  expand: boolean;
  isDefault: boolean;
  volumes: number;
}
export interface SecretMount {
  svc: string;
  mode: "env" | "file" | "pull";
  at: string;
  v: number;
}
export interface Secret {
  name: string;
  ns: string;
  type: string;
  version: number;
  age: string;
  updated: string;
  castBy: string;
  rotated?: boolean;
  auto?: string;
  keys: { k: string; bytes: number }[];
  mounts: SecretMount[];
  usedBy: string[];
}
export interface ConfigMap {
  name: string;
  ns: string;
  version: number;
  age: string;
  updated: string;
  castBy: string;
  data: { k: string; value: string; file?: boolean }[];
  mounts: SecretMount[];
  usedBy: string[];
}
export interface Policy {
  name: string;
  ns: string;
  direction: string;
  targets: string;
  rules: number;
  mode: "deny" | "allow";
  desc?: string;
  from?: string;
  ports?: string;
}
export interface Role {
  name: string;
  scope: string;
  verbs: string;
  subjects: number;
  builtin: boolean;
}
export interface Principal {
  name: string;
  kind: string;
  email: string;
  role: string;
  mfa: boolean;
  last: string;
  type: "human" | "machine";
}
export interface ClusterEvent {
  t: string;
  kind: string;
  svc: string;
  ns: string;
  msg: string;
  status: StatusKey;
}
export interface LogLine {
  ts: string;
  level: "info" | "warn" | "error" | "debug";
  svc: string;
  msg: string;
}

const nodes: Node[] = [
  { name: "do-nyc3-01", role: "control-plane", status: "ready", cpu: 62, mem: 71, instances: 11, addr: "10.10.0.11" },
  { name: "do-nyc3-02", role: "worker", status: "ready", cpu: 48, mem: 54, instances: 9, addr: "10.10.0.12" },
  { name: "do-nyc3-03", role: "worker", status: "ready", cpu: 81, mem: 88, instances: 8, addr: "10.10.0.13" },
];

const services: Service[] = [
  { name: "web-gateway", ns: "production", type: "container", status: "run", ready: 3, want: 3, image: "ghcr.io/acme/web-gateway:1.24.0", cpu: 34, mem: 41, restarts: 0, age: "14d", ports: ["80/http", "443/https"], runeset: "acme-platform@2.3.1", policy: "allow-web-to-api" },
  { name: "api-core", ns: "production", type: "container", status: "run", ready: 4, want: 4, image: "ghcr.io/acme/api-core:3.11.2", cpu: 58, mem: 63, restarts: 1, age: "6d", ports: ["8080/http"], runeset: "acme-platform@2.3.1", policy: "allow-api-to-db" },
  { name: "auth-service", ns: "production", type: "container", status: "run", ready: 2, want: 2, image: "ghcr.io/acme/auth:2.0.7", cpu: 19, mem: 28, restarts: 0, age: "14d", ports: ["9000/grpc"], runeset: "acme-platform@2.3.1", policy: "allow-api-to-db" },
  { name: "payments", ns: "production", type: "container", status: "warn", ready: 2, want: 3, image: "ghcr.io/acme/payments:1.8.4", cpu: 44, mem: 52, restarts: 7, age: "2d", ports: ["8090/http"], runeset: "acme-platform@2.3.1", policy: "allow-api-to-db", note: "1 instance failing readiness probe" },
  { name: "worker-queue", ns: "production", type: "container", status: "run", ready: 5, want: 5, image: "ghcr.io/acme/worker:1.8.4", cpu: 71, mem: 60, restarts: 0, age: "2d", ports: [], runeset: "acme-platform@2.3.1", policy: "default-deny" },
  { name: "search-indexer", ns: "production", type: "container", status: "deploy", ready: 1, want: 2, image: "ghcr.io/acme/indexer:0.9.0", cpu: 27, mem: 38, restarts: 0, age: "3m", ports: ["7700/http"], runeset: "acme-platform@2.3.1", policy: "default-deny", note: "rolling update 1 → 0.9.0" },
  { name: "postgres-primary", ns: "production", type: "container", status: "run", ready: 1, want: 1, image: "postgres:16.3-alpine", cpu: 51, mem: 77, restarts: 0, age: "29d", ports: ["5432/tcp"], runeset: "data-tier@1.4.0", policy: "allow-api-to-db", stateful: true, volume: "pg-data" },
  { name: "redis-cache", ns: "production", type: "container", status: "run", ready: 2, want: 2, image: "redis:7.4-alpine", cpu: 12, mem: 22, restarts: 0, age: "29d", ports: ["6379/tcp"], runeset: "data-tier@1.4.0", policy: "allow-api-to-db", stateful: true, volume: "redis-data" },
  { name: "notifications", ns: "staging", type: "container", status: "run", ready: 1, want: 1, image: "ghcr.io/acme/notify:0.4.1", cpu: 8, mem: 14, restarts: 0, age: "1d", ports: ["8081/http"], runeset: "acme-platform@2.4.0-rc1", policy: "default-deny" },
  { name: "billing-cron", ns: "production", type: "process", status: "idle", ready: 0, want: 0, image: "acme/billing:1.2.0 (process)", cpu: 0, mem: 0, restarts: 0, age: "12d", ports: [], runeset: "acme-platform@2.3.1", policy: "default-deny", schedule: "0 */6 * * *", note: "next run in 2h 14m" },
  { name: "analytics-collector", ns: "observability", type: "container", status: "run", ready: 2, want: 2, image: "ghcr.io/acme/collector:1.1.0", cpu: 23, mem: 31, restarts: 0, age: "8d", ports: ["4317/grpc"], runeset: "observability@1.0.2", policy: "allow-observability-scrape" },
  { name: "grafana", ns: "observability", type: "container", status: "run", ready: 1, want: 1, image: "grafana/grafana:11.1.0", cpu: 6, mem: 18, restarts: 0, age: "8d", ports: ["3000/http"], runeset: "observability@1.0.2", policy: "allow-observability-scrape" },
  { name: "loki", ns: "observability", type: "container", status: "run", ready: 1, want: 1, image: "grafana/loki:3.1.0", cpu: 17, mem: 44, restarts: 0, age: "8d", ports: ["3100/http"], runeset: "observability@1.0.2", policy: "allow-observability-scrape", stateful: true, volume: "loki-storage" },
  { name: "ingress", ns: "ingress-system", type: "container", status: "run", ready: 2, want: 2, image: "traefik:3.1", cpu: 15, mem: 20, restarts: 0, age: "29d", ports: ["80/http", "443/https"], runeset: "edge@2.0.0", policy: "allow-web-to-api" },
];

const nodeNames = nodes.map((n) => n.name);
const instances: Instance[] = [];
services.forEach((svc, si) => {
  const total = Math.max(svc.want, svc.ready);
  for (let i = 0; i < total; i++) {
    let st: StatusKey = "run";
    if (svc.status === "warn" && i === svc.ready) st = "fail";
    else if (svc.status === "deploy" && i >= svc.ready) st = "deploy";
    else if (svc.status === "idle") st = "idle";
    instances.push({
      id: `${svc.name}-${(si * 991 + i * 137).toString(36).slice(-5)}`,
      svc: svc.name,
      ns: svc.ns,
      node: nodeNames[(i + si) % nodeNames.length],
      status: st,
      cpu: st === "run" ? Math.max(2, Math.round((svc.cpu * 0.9) / Math.max(1, total))) : st === "fail" ? 0 : Math.round(svc.cpu / total),
      mem: st === "run" ? Math.max(3, Math.round((svc.mem * 0.9) / Math.max(1, total))) : st === "fail" ? 0 : Math.round(svc.mem / total),
      restarts: st === "fail" ? svc.restarts : i === 0 ? svc.restarts : 0,
      uptime: st === "deploy" ? "12s" : st === "fail" ? "—" : svc.age,
      ip: `10.244.${si}.${10 + i}`,
    });
  }
});

const namespaces: Namespace[] = [
  { name: "production", services: 10, instances: 23, cpu: 64, mem: 71, labels: ["env:prod", "team:platform"], age: "29d" },
  { name: "staging", services: 1, instances: 1, cpu: 8, mem: 14, labels: ["env:staging"], age: "20d" },
  { name: "observability", services: 4, instances: 5, cpu: 22, mem: 38, labels: ["env:prod", "team:sre"], age: "8d" },
  { name: "ingress-system", services: 1, instances: 2, cpu: 15, mem: 20, labels: ["system:true"], age: "29d" },
  { name: "default", services: 0, instances: 0, cpu: 0, mem: 0, labels: [], age: "29d" },
];

const volumes: Volume[] = [
  { name: "pg-data", ns: "production", size: "20Gi", used: 71, status: "bound", class: "do-block-storage", node: "do-nyc3-01", svc: "postgres-primary", mode: "RWO", age: "29d" },
  { name: "redis-data", ns: "production", size: "4Gi", used: 22, status: "bound", class: "local-ssd", node: "do-nyc3-02", svc: "redis-cache", mode: "RWO", age: "29d" },
  { name: "loki-storage", ns: "observability", size: "50Gi", used: 44, status: "bound", class: "do-block-storage", node: "do-nyc3-03", svc: "loki", mode: "RWO", age: "8d" },
  { name: "uploads", ns: "production", size: "100Gi", used: 38, status: "bound", class: "do-block-storage", node: "do-nyc3-01", svc: "api-core", mode: "RWX", age: "14d" },
  { name: "pg-backup", ns: "production", size: "40Gi", used: 12, status: "available", class: "do-block-storage", node: "—", svc: "—", mode: "RWO", age: "1d" },
];

const storageClasses: StorageClass[] = [
  { name: "do-block-storage", provisioner: "dobs.csi.digitalocean.com", reclaim: "Retain", expand: true, isDefault: true, volumes: 4 },
  { name: "local-ssd", provisioner: "rune.io/local-path", reclaim: "Delete", expand: false, isDefault: false, volumes: 1 },
];

const secrets: Secret[] = [
  { name: "db-credentials", ns: "production", type: "Opaque", version: 3, age: "29d", updated: "29d", castBy: "ore", keys: [{ k: "POSTGRES_USER", bytes: 9 }, { k: "POSTGRES_PASSWORD", bytes: 24 }, { k: "DATABASE_URL", bytes: 61 }], mounts: [{ svc: "api-core", mode: "env", at: "envFrom", v: 3 }, { svc: "auth-service", mode: "env", at: "envFrom", v: 3 }, { svc: "worker-queue", mode: "file", at: "/etc/secrets/db", v: 3 }], usedBy: ["api-core", "auth-service", "worker-queue"] },
  { name: "stripe-api-key", ns: "production", type: "Opaque", version: 4, age: "21d", updated: "3d", castBy: "ci-deployer", rotated: true, keys: [{ k: "STRIPE_SECRET_KEY", bytes: 32 }, { k: "STRIPE_WEBHOOK_SECRET", bytes: 32 }], mounts: [{ svc: "payments", mode: "env", at: "envFrom", v: 3 }], usedBy: ["payments"] },
  { name: "jwt-signing-key", ns: "production", type: "Opaque", version: 2, age: "29d", updated: "29d", castBy: "ore", keys: [{ k: "JWT_PRIVATE_KEY", bytes: 1704 }, { k: "JWT_PUBLIC_KEY", bytes: 451 }], mounts: [{ svc: "auth-service", mode: "file", at: "/etc/secrets/jwt", v: 2 }, { svc: "api-core", mode: "env", at: "envFrom", v: 2 }], usedBy: ["auth-service", "api-core"] },
  { name: "ghcr-pull", ns: "production", type: "dockerconfigjson", version: 1, age: "29d", updated: "14d", castBy: "ore", keys: [{ k: ".dockerconfigjson", bytes: 412 }], mounts: [{ svc: "*", mode: "pull", at: "imagePullSecret", v: 1 }], usedBy: ["*"] },
  { name: "tls-acme", ns: "ingress-system", type: "tls", version: 5, age: "29d", updated: "5d", castBy: "acme-renewer", auto: "Managed by ACME · auto-renews 30d before expiry", keys: [{ k: "tls.crt", bytes: 1834 }, { k: "tls.key", bytes: 1704 }], mounts: [{ svc: "ingress", mode: "file", at: "/etc/tls", v: 5 }], usedBy: ["ingress"] },
  { name: "grafana-admin", ns: "observability", type: "Opaque", version: 1, age: "8d", updated: "8d", castBy: "ore", keys: [{ k: "GF_SECURITY_ADMIN_PASSWORD", bytes: 20 }], mounts: [{ svc: "grafana", mode: "env", at: "envFrom", v: 1 }], usedBy: ["grafana"] },
];

const configmaps: ConfigMap[] = [
  { name: "api-config", ns: "production", version: 4, age: "6d", updated: "6d", castBy: "ada.m", data: [{ k: "log.level", value: "info" }, { k: "app.yaml", file: true, value: "server:\n  port: 8080\n  workers: 4\nlogging:\n  level: info\n  format: json\ndatabase:\n  host: postgres-primary\n  pool: 25" }, { k: "feature.timeouts", file: true, value: "checkout_ms: 4000\nsearch_ms: 1200\nupstream_ms: 8000" }], mounts: [{ svc: "api-core", mode: "file", at: "/etc/config", v: 4 }], usedBy: ["api-core"] },
  { name: "feature-flags", ns: "production", version: 7, age: "12d", updated: "1d", castBy: "dele.k", data: [{ k: "flags.json", file: true, value: '{\n  "new_checkout": true,\n  "search_v2": true,\n  "beta_banner": false,\n  "legacy_export": false\n}' }], mounts: [{ svc: "api-core", mode: "file", at: "/etc/flags", v: 7 }, { svc: "web-gateway", mode: "env", at: "envFrom", v: 6 }], usedBy: ["api-core", "web-gateway"] },
  { name: "loki-conf", ns: "observability", version: 1, age: "8d", updated: "8d", castBy: "ore", data: [{ k: "loki.yaml", file: true, value: "auth_enabled: false\nserver:\n  http_listen_port: 3100\nschema_config:\n  configs:\n    - store: tsdb\n      schema: v13" }], mounts: [{ svc: "loki", mode: "file", at: "/etc/loki", v: 1 }], usedBy: ["loki"] },
];

const encryption = {
  algo: "AES-256-GCM",
  scheme: "envelope encryption",
  kek: { source: "/etc/runed/kek.key", mode: "0600", bytes: 32, loaded: "on server start · 29d ago" },
  dek: "fresh 256-bit DEK generated per secret version",
  aad: "namespace · name · version",
  versionsKept: 5,
};

const policies: Policy[] = [
  { name: "default-deny", ns: "production", direction: "ingress+egress", targets: "all pods", rules: 0, mode: "deny", desc: "Baseline deny-all; explicit allows layered on top." },
  { name: "allow-web-to-api", ns: "production", direction: "ingress", targets: "api-core", rules: 1, mode: "allow", from: "web-gateway", ports: "8080/http" },
  { name: "allow-api-to-db", ns: "production", direction: "egress", targets: "api-core, auth-service", rules: 2, mode: "allow", from: "→ postgres-primary, redis-cache", ports: "5432, 6379" },
  { name: "allow-observability-scrape", ns: "observability", direction: "ingress", targets: "all pods", rules: 1, mode: "allow", from: "analytics-collector", ports: "9090/metrics" },
];

const roles: Role[] = [
  { name: "cluster-admin", scope: "cluster", verbs: "* on *", subjects: 1, builtin: true },
  { name: "developer", scope: "namespace: production, staging", verbs: "get, list, cast, scale, logs, exec", subjects: 4, builtin: false },
  { name: "viewer", scope: "cluster", verbs: "get, list, logs", subjects: 6, builtin: true },
  { name: "ci-deployer", scope: "namespace: production", verbs: "cast, get, scale, restart", subjects: 1, builtin: false },
];

const principals: Principal[] = [
  { name: "ore", kind: "user", email: "ore@acme.io", role: "cluster-admin", mfa: true, last: "active now", type: "human" },
  { name: "dele.k", kind: "user", email: "dele@acme.io", role: "developer", mfa: true, last: "2h ago", type: "human" },
  { name: "ada.m", kind: "user", email: "ada@acme.io", role: "developer", mfa: true, last: "1d ago", type: "human" },
  { name: "sade.o", kind: "user", email: "sade@acme.io", role: "viewer", mfa: false, last: "4d ago", type: "human" },
  { name: "ci-deployer", kind: "service account", email: "system:ci-deployer", role: "ci-deployer", mfa: false, last: "8m ago", type: "machine" },
  { name: "tofu-state", kind: "service account", email: "system:tofu-state", role: "developer", mfa: false, last: "3h ago", type: "machine" },
];

const events: ClusterEvent[] = [
  { t: "just now", kind: "deploy", svc: "search-indexer", ns: "production", msg: "Rolling update started — <b>indexer:0.8.2</b> → <b>indexer:0.9.0</b>", status: "deploy" },
  { t: "2m ago", kind: "health", svc: "payments", ns: "production", msg: "Instance <span class='mono'>payments-7fk2d</span> failed readiness probe (HTTP 503)", status: "warn" },
  { t: "11m ago", kind: "scale", svc: "worker-queue", ns: "production", msg: "Scaled <b>worker-queue</b> from 3 → 5 instances", status: "run" },
  { t: "34m ago", kind: "cast", svc: "api-core", ns: "production", msg: "Deployed <b>api-core:3.11.2</b> by <span class='mono'>ci-deployer</span>", status: "run" },
  { t: "1h ago", kind: "secret", svc: "stripe-api-key", ns: "production", msg: "Secret <span class='mono'>stripe-api-key</span> rotated", status: "net" },
  { t: "2h ago", kind: "restart", svc: "payments", ns: "production", msg: "Instance <span class='mono'>payments-7fk2d</span> restarted (7th restart)", status: "warn" },
  { t: "5h ago", kind: "deploy", svc: "notifications", ns: "staging", msg: "Deployed <b>notify:0.4.1</b> to <b>staging</b>", status: "run" },
  { t: "Today 09:14", kind: "policy", svc: "ingress", ns: "ingress-system", msg: "Network policy <span class='mono'>allow-web-to-api</span> applied", status: "net" },
];

const logSvcs = ["api-core", "web-gateway", "auth-service", "payments", "worker-queue"];
const logTemplates: [LogLine["level"], string][] = [
  ["info", "request completed method=GET path=/v1/orders status=200 dur=42ms"],
  ["info", "request completed method=POST path=/v1/checkout status=201 dur=118ms"],
  ["debug", "cache hit key=user:48211 ttl=290s"],
  ["info", "request completed method=GET path=/healthz status=200 dur=1ms"],
  ["warn", "upstream latency high service=payments p99=820ms"],
  ["info", "db pool acquired conn=11/25 wait=0ms"],
  ["error", "checkout failed: payments upstream returned 503 (retry 1/3)"],
  ["info", "request completed method=GET path=/v1/catalog status=200 dur=64ms"],
  ["debug", "trace span flushed exporter=otlp endpoint=analytics-collector:4317"],
  ["info", "grpc auth.Verify ok subject=user:48211 dur=8ms"],
  ["warn", "rate limit applied client=10.244.1.18 bucket=api-anon"],
  ["info", "worker job processed queue=emails id=j_8821 dur=210ms"],
];
function makeLog(seedOffset: number): LogLine[] {
  const base = Date.now() - seedOffset;
  const lines: LogLine[] = [];
  for (let i = 0; i < 22; i++) {
    const tpl = logTemplates[(i * 7 + seedOffset) % logTemplates.length];
    const d = new Date(base - (22 - i) * 1400);
    const ts = d.toISOString().slice(11, 23);
    lines.push({ ts, level: tpl[0], svc: logSvcs[(i + seedOffset) % logSvcs.length], msg: tpl[1] });
  }
  return lines;
}

const cluster = { name: "rune-prod", context: "nyc3 · do", version: "rune v0.34.2", uptime: "29d 4h", nodes: nodes.length };

const totals = {
  services: services.length,
  healthy: services.filter((s) => s.status === "run").length,
  instances: instances.length,
  runningInstances: instances.filter((i) => i.status === "run").length,
  namespaces: namespaces.filter((n) => n.services > 0).length,
  cpu: 64,
  mem: 71,
  cpuCores: "12.8 / 24 vCPU",
  memGi: "45.1 / 64 GiB",
};

const cpuHistory = [38, 41, 44, 40, 52, 58, 55, 61, 57, 63, 68, 71, 66, 72, 64, 59, 62, 70, 74, 69, 64, 60, 63, 64];
const memHistory = [55, 57, 59, 58, 62, 64, 66, 68, 67, 69, 71, 73, 72, 74, 71, 68, 69, 72, 75, 73, 71, 70, 71, 71];

export const RUNE = {
  cluster,
  totals,
  nodes,
  services,
  instances,
  namespaces,
  volumes,
  storageClasses,
  secrets,
  configmaps,
  encryption,
  policies,
  roles,
  principals,
  events,
  makeLog,
  cpuHistory,
  memHistory,
  NS: ["production", "staging", "observability", "ingress-system", "default"],
};
