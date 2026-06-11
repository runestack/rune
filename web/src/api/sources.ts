/* Sources API — store-level facts plus ingest metrics for the RuneSight
   Sources page. GetObserveStats reports what the store owns (records, disk,
   retention); everything else is ordinary LogQL metric queries through
   ObserveService.Execute, summed per group-label series. Stores that don't
   own their storage (loki/clickhouse) report supported=false — the UI shows
   "managed by the backend" instead of a utilization bar. */

import { ObserveQuery, ObserveStatsRequest } from "../gen/pkg/api/proto/observe_pb";
import { clients } from "./transport";

export interface StatsData {
  backend: string;
  supported: boolean; // false => storage managed by the external backend
  records: number;
  diskUsedBytes: number;
  diskCapBytes: number; // 0 = unbounded
  retentionDays: number; // 0 = store default, negative = disabled
  oldestRecord: string; // RFC3339; "" when the store is empty
}

/** Store-level facts (records, disk, retention) from the observe store. */
export async function getStats(): Promise<StatsData> {
  const s = await clients.observe.getObserveStats(new ObserveStatsRequest());
  return {
    backend: s.backend,
    supported: s.supported,
    records: Number(s.records),
    diskUsedBytes: Number(s.diskUsedBytes),
    diskCapBytes: Number(s.diskCapBytes),
    retentionDays: s.retentionDays,
    oldestRecord: s.oldestRecord,
  };
}

/* ---------------- metric query runner ---------------- */

const HOUR_MS = 3_600_000;

interface Series {
  labels: Record<string, string>;
  sum: number;
}

/**
 * Run one metric LogQL over [now-windowMs, now] and sum the streamed samples
 * per group-label series. Empty result sets resolve to an empty list.
 */
async function sumSeries(logql: string, windowMs: number, signal?: AbortSignal): Promise<Series[]> {
  const end = Date.now();
  const req = new ObserveQuery({
    logql,
    start: new Date(end - windowMs).toISOString(),
    end: new Date(end).toISOString(),
    limit: 0,
    forward: true,
    namespace: "",
  });
  const series = new Map<string, Series>();
  for await (const res of clients.observe.execute(req, { signal })) {
    if (res.result.case !== "sample") continue;
    const s = res.result.value;
    const labels = s.groupLabels ?? {};
    const key = Object.keys(labels).sort().map((k) => `${k}=${labels[k]}`).join(",");
    const cur = series.get(key);
    if (cur) cur.sum += s.value;
    else series.set(key, { labels: { ...labels }, sum: s.value });
  }
  return [...series.values()];
}

/* ---------------- per-surface helpers ---------------- */

export interface StreamRow {
  namespace: string;
  service: string;
  lines: number;
}

/** Busiest streams over the last 24h (lines per namespace/service, top 10). */
export async function topStreams(signal?: AbortSignal): Promise<StreamRow[]> {
  const series = await sumSeries(
    'sum by (namespace, service) (count_over_time({namespace=~".+"} [24h]))',
    24 * HOUR_MS,
    signal,
  );
  return series
    .map((s) => ({
      namespace: s.labels.namespace ?? "",
      service: s.labels.service ?? "",
      lines: Math.round(s.sum),
    }))
    .filter((s) => s.lines > 0)
    .sort((a, b) => b.lines - a.lines)
    .slice(0, 10);
}

export interface NodeRate {
  node: string;
  lps: number; // lines per second over the last 5m
}

/** Per-node ingest rate: 5m line count / 300s, sorted busiest first. */
export async function nodeRates(signal?: AbortSignal): Promise<NodeRate[]> {
  const series = await sumSeries(
    'sum by (node) (count_over_time({namespace=~".+"} [5m]))',
    5 * 60_000,
    signal,
  );
  return series
    .map((s) => ({ node: s.labels.node ?? "", lps: Math.round((s.sum / 300) * 10) / 10 }))
    .filter((s) => s.node !== "")
    .sort((a, b) => b.lps - a.lps);
}

export interface Bytes24h {
  total: number;
  byService: { service: string; bytes: number }[];
}

/** Bytes ingested over the last 24h, total plus per-service breakdown. */
export async function bytes24h(signal?: AbortSignal): Promise<Bytes24h> {
  const series = await sumSeries(
    'sum by (service) (bytes_over_time({namespace=~".+"} [24h]))',
    24 * HOUR_MS,
    signal,
  );
  const byService = series
    .map((s) => ({ service: s.labels.service ?? "", bytes: Math.round(s.sum) }))
    .filter((s) => s.bytes > 0)
    .sort((a, b) => b.bytes - a.bytes);
  return { total: byService.reduce((a, s) => a + s.bytes, 0), byService };
}

/* ---------------- formatting ---------------- */

/** Human bytes: "812 KB", "3.8 MB", "1.2 GB". Binary base, short labels. */
export function fmtBytes(n: number): string {
  const { v, u } = fmtBytesParts(n);
  return `${v} ${u}`;
}

/** fmtBytes split for KPI markup (value + unit rendered separately). */
export function fmtBytesParts(n: number): { v: string; u: string } {
  if (!Number.isFinite(n) || n <= 0) return { v: "0", u: "B" };
  const units = ["B", "KB", "MB", "GB", "TB"];
  let i = 0;
  let x = n;
  while (x >= 1024 && i < units.length - 1) { x /= 1024; i++; }
  return { v: i === 0 ? String(Math.round(x)) : x >= 100 ? x.toFixed(0) : x.toFixed(1), u: units[i] };
}
