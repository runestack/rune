/* ============================================================
   RuneSight — mocked metadata for surfaces without a backend yet.

   The live log query / histogram / facets come from ObserveService
   (see ../../api/observe). Saved Views, Alert Rules and Notification
   Channels have no server seam yet, so we ship the design's mock data
   (ported from the design bundle's runesight-data.js) to render the
   Saved Queries panel, the Saved Views screen, and the nav badges.
   ============================================================ */
import type { LogQuery } from "../../api/observe";
import { normQuery, toLogQL } from "../../api/observe";
import type { IconName } from "../../components";

export interface SavedView {
  id: string;
  name: string;
  icon: IconName;
  q: Partial<LogQuery>;
  owner: string;
  last: string;
  pinned: boolean;
  /** approximate 24h line count — mocked since there's no aggregate seam */
  count: number;
  desc?: string;
}

export interface AlertRule {
  id: string;
  name: string;
  q: Partial<LogQuery>;
  cond: string;
  status: "firing" | "pending" | "ok" | "paused";
  value: string;
  since?: string;
  owner: string;
  chan: string;
}

export interface Channel {
  id: string;
  label: string;
  kind: "webhook" | "slack" | "email";
  state: "healthy" | "degraded" | "down";
  last: string;
}

/* ---- saved views (design: RSIGHT.savedViews) ---- */
export const SAVED_VIEWS: SavedView[] = [
  { id: "v1", name: "Payment errors", icon: "alert", q: { services: ["payments"], levels: ["error", "warn"] }, owner: "ore", last: "2m ago", pinned: true, count: 123 },
  { id: "v2", name: "Checkout 503s", icon: "bolt", q: { text: "503" }, owner: "dele.k", last: "18m ago", pinned: true, count: 151 },
  { id: "v3", name: "Auth failures", icon: "shield", q: { services: ["auth-service"], levels: ["error"] }, owner: "ada.m", last: "1h ago", pinned: false, count: 17 },
  { id: "v4", name: "Slow upstreams", icon: "clock", q: { levels: ["warn"], text: "latency" }, owner: "ore", last: "3h ago", pinned: false, count: 128 },
  { id: "v5", name: "5xx — api-core", icon: "health", q: { namespaces: ["production"], services: ["api-core"], text: "500" }, owner: "sre", last: "yesterday", pinned: false, count: 7 },
  { id: "v6", name: "Deploy activity — indexer", icon: "refresh", q: { services: ["search-indexer"] }, owner: "ci-deployer", last: "3m ago", pinned: false, count: 64 },
];

/* ---- notification channels (design: RSIGHT.channels) ---- */
export const CHANNELS: Channel[] = [
  { id: "pagerduty", label: "pagerduty", kind: "webhook", state: "healthy", last: "2m ago" },
  { id: "#sre", label: "#sre", kind: "slack", state: "healthy", last: "18m ago" },
  { id: "#deploys", label: "#deploys", kind: "slack", state: "healthy", last: "1d ago" },
  { id: "opsgenie", label: "opsgenie", kind: "webhook", state: "degraded", last: "3h ago · 1 retry" },
  { id: "sre@runestack.io", label: "sre@runestack.io", kind: "email", state: "healthy", last: "4h ago" },
];

/* ---- alert rules (design: RSIGHT.alerts) ---- */
export const ALERTS: AlertRule[] = [
  { id: "a1", name: "Payment error rate", q: { services: ["payments"], levels: ["error"] }, cond: "> 10 errors / 5m", status: "firing", value: "17 / 5m", since: "12:40", owner: "ore", chan: "webhook:pagerduty" },
  { id: "a2", name: "Worker absent", q: { services: ["worker-queue"] }, cond: "no logs for 5m", status: "firing", value: "0 / 5m", since: "12:24", owner: "sre", chan: "slack:#sre" },
  { id: "a3", name: "Slow API p95", q: { services: ["api-core"], text: "dur" }, cond: "> 250ms for 2m", status: "pending", value: "262ms", owner: "ada.m", chan: "webhook:opsgenie" },
  { id: "a4", name: "5xx spike — api-core", q: { services: ["api-core"], text: "500" }, cond: "> 5 / 5m", status: "ok", value: "1 / 5m", owner: "sre", chan: "email:sre@runestack.io" },
  { id: "a5", name: "Auth failures", q: { services: ["auth-service"], levels: ["error"] }, cond: "> 20 / 15m", status: "ok", value: "3 / 15m", owner: "ada.m", chan: "slack:#deploys" },
  { id: "a6", name: "Ingest volume drop", q: {}, cond: "< 50% of 24h baseline", status: "ok", value: "−4%", owner: "ore", chan: "slack:#sre" },
];

/* ---- query history seed for the Saved Queries panel ---- */
export const HISTORY_SEED = [
  { q: { namespaces: ["production"], services: ["api-core"] } as Partial<LogQuery>, ts: "12:41:08", hits: "318", dur: "42ms" },
  { q: { levels: ["error"] } as Partial<LogQuery>, ts: "12:38:54", hits: "1.1k", dur: "61ms" },
];

/* ---- retention / ingestion summary for the home Sight card (mocked) ---- */
export const INGESTION = {
  retentionDays: 14,
  storageUsedGi: 22.1,
  storageCapGi: 50,
  storagePct: 44,
  gbToday: 3.8,
};

/** Streaming log sources (services emitting logs). Mocked — matches the
 *  design's running-container set; the live count comes from facets when a
 *  query runs, but the nav badge needs a stable value up front. */
export const SOURCES_COUNT = 13;

export const firingCount = (): number => ALERTS.filter((a) => a.status === "firing").length;

export const savedViewLogQL = (q: Partial<LogQuery>): string => toLogQL(normQuery(q));

/* derive a meaningful icon + tone from a saved view's query (design: classifyView) */
export type Tone = "red" | "tan" | "cyan" | "green" | "accent";
export function classifyView(q0: Partial<LogQuery>): { icon: IconName; tone: Tone } {
  const q = normQuery(q0);
  const levels = q.levels, services = q.services, text = (q.text || "").toLowerCase();
  if (levels.includes("error") || /error|exception|panic|fail/.test(text)) return { icon: "alert", tone: "red" };
  if (/50\d|5xx|timeout|unavailable/.test(text)) return { icon: "bolt", tone: "tan" };
  if (services.some((s) => /auth|login|token/.test(s)) || /auth|denied|reject|forbidden/.test(text)) return { icon: "shield", tone: "cyan" };
  if (/rollback|deploy|cast|image|release/.test(text) || services.some((s) => /deploy|indexer|ci/.test(s))) return { icon: "refresh", tone: "green" };
  if (/latency|slow|dur|p9\d|timeout/.test(text)) return { icon: "clock", tone: "tan" };
  if (levels.includes("warn")) return { icon: "alert", tone: "tan" };
  if (services.length) return { icon: "services", tone: "accent" };
  if (q.namespaces.length) return { icon: "namespaces", tone: "accent" };
  return { icon: "search", tone: "accent" };
}
