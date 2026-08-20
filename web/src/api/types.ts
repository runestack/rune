/* Domain types for the dashboard — the screen-facing shapes that the data
   hooks map proto responses into (see api/map.ts). */
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
/** Progress of an in-flight rolling update. Mirrors the server's
 *  UpdateStatus, reduced to what the dashboard renders. */
export interface ServiceUpdate {
  /** Replicas already carrying the new template AND serving. */
  replaced: number;
  /** Replicas serving right now, any template. */
  serving: number;
  /** Desired replica count. */
  desired: number;
  /** One-sentence explanation of what the update is waiting on. */
  message?: string;
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
  /** Reason slug behind a non-Running status, e.g. "UpdateStalled". */
  reason?: string;
  /** In-flight rolling update; undefined when none is running (RUNE-042). */
  update?: ServiceUpdate;
  stateful?: boolean;
  volume?: string;
  schedule?: string;
  ingress?: Ingress;
  netpol?: NetPolicy;
}

/** External exposure (ingress) for a service: the host it's reachable at plus
 *  TLS + async cert state. Absent when the service has no `expose.host`. */
export interface Ingress {
  host: string;
  url: string; // scheme://host[path]
  tls: string; // "manual" | "auto" | "" (plain http)
  path?: string;
  cert?: IngressCert;
}
export interface IngressCert {
  state: string; // "Pending" | "Issued" | "Failed"
  expiresAt?: string; // RFC3339
  lastError?: string;
}
/** Service-to-service network policy rule counts (ingress/egress). */
export interface NetPolicy {
  ingress: number;
  egress: number;
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
  // Data resources living in the namespace. A namespace is only truly "empty"
  // when it has none of these AND no services — a secrets-only namespace
  // (e.g. cert/credential stores) still has content.
  secrets: number;
  configs: number;
  volumes: number;
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
