/* ============================================================
   RuneSight observability data layer.

   ObserveService.Execute is a server-stream of QueryResult (each is either a
   LogRow or a MetricSample). The Log Explorer issues two queries per run:
     1. the user's LogQL, collecting LogRow results into the stream.
     2. a `count_over_time(...[Nm])` wrapper, collecting MetricSample results
        into the level histogram (one series per level via group_labels).

   The query is modelled as a structured `LogQuery` of label matchers + a line
   filter. We serialize it to LogQL for the wire and the query bar, and parse
   LogQL back so the bar stays the source of truth (mirrors runesight-data.js).
   ============================================================ */
import { useCallback, useEffect, useState } from "react";
import { clients } from "./transport";
import { CapabilitiesRequest, ObserveQuery } from "../gen/pkg/api/proto/observe_pb";
import type { Query } from "./hooks";

/* ---------------- model ---------------- */

export const LEVELS = ["error", "warn", "info", "debug"] as const;
export type Level = (typeof LEVELS)[number];

/** Label keys we surface as facets / matchers, mapped to LogQL label names. */
export const LABEL_KEYS = ["namespace", "service", "level", "node", "instance"] as const;
export type LabelKey = (typeof LABEL_KEYS)[number];

/** Structured query: OR within a label, AND across labels, plus a line filter. */
export interface LogQuery {
  namespaces: string[];
  services: string[];
  levels: string[];
  nodes: string[];
  instances: string[];
  text: string;
}

const QF: Record<LabelKey, keyof LogQuery> = {
  namespace: "namespaces",
  service: "services",
  level: "levels",
  node: "nodes",
  instance: "instances",
};

export function emptyQuery(): LogQuery {
  return { namespaces: [], services: [], levels: [], nodes: [], instances: [], text: "" };
}

export function normQuery(q?: Partial<LogQuery>): LogQuery {
  return { ...emptyQuery(), ...q };
}

/* ---------------- LogQL <-> LogQuery ---------------- */

/** Serialize the structured query to LogQL (selector + line filter + level pipe). */
export function toLogQL(q0: Partial<LogQuery>): string {
  const q = normQuery(q0);
  const sel: string[] = [];
  const add = (key: string, vals: string[]) => {
    if (!vals.length) return;
    sel.push(vals.length > 1 ? `${key}=~"${vals.join("|")}"` : `${key}="${vals[0]}"`);
  };
  add("namespace", q.namespaces);
  add("service", q.services);
  add("node", q.nodes);
  add("instance", q.instances);
  if (!sel.length) sel.push('namespace=~".+"');
  let s = `{${sel.join(", ")}}`;
  if (q.text) s += ` |= "${q.text}"`;
  if (q.levels.length && q.levels.length < LEVELS.length) s += ` | level=~"${q.levels.join("|")}"`;
  return s;
}

/** Parse a LogQL string back into the structured query (best-effort subset). */
export function parseLogQL(str: string): LogQuery {
  const q = emptyQuery();
  const sel = str.match(/\{([^}]*)\}/);
  if (sel) {
    const re = /(\w+)\s*(=~|!=|!~|=)\s*"([^"]*)"/g;
    let m: RegExpExecArray | null;
    while ((m = re.exec(sel[1]))) {
      const key = m[1] as LabelKey;
      const val = m[3];
      if (!(key in QF)) continue;
      if (key === "namespace" && val === ".+") continue;
      (q[QF[key]] as string[]) = val.split("|").filter(Boolean);
    }
  }
  const tx = str.match(/\|=\s*"([^"]*)"/);
  if (tx) q.text = tx[1];
  const lv = str.match(/\|\s*level\s*=~?\s*"([^"]*)"/);
  if (lv) q.levels = lv[1].split("|").filter(Boolean);
  return q;
}

/* ---------------- ranges ---------------- */

export const RANGES = ["15m", "1h", "6h", "24h"] as const;
export type Range = (typeof RANGES)[number];
export const RANGE_MS: Record<Range, number> = {
  "15m": 15 * 60_000,
  "1h": 3_600_000,
  "6h": 6 * 3_600_000,
  "24h": 24 * 3_600_000,
};

/* ---------------- run shapes ---------------- */

export interface LogLine {
  id: string;
  t: number; // epoch ms
  ts: string; // HH:MM:SS.mmm
  level: string;
  ns: string;
  svc: string;
  inst: string;
  node: string;
  stream: string;
  msg: string;
  labels: Record<string, string>;
}

export interface Bucket {
  t0: number;
  error: number;
  warn: number;
  info: number;
  debug: number;
  total: number;
}

export interface RunResult {
  lines: LogLine[];
  buckets: Bucket[];
  bucketW: number;
  scannedRows: number;
  durationMs: number;
}

const BUCKETS = 60;

function fmtClockMs(t: number): string {
  return new Date(t).toISOString().slice(11, 23);
}

function emptyBuckets(start: number, span: number): { buckets: Bucket[]; w: number } {
  const w = span / BUCKETS;
  const buckets: Bucket[] = [];
  for (let i = 0; i < BUCKETS; i++) {
    buckets.push({ t0: start + i * w, error: 0, warn: 0, info: 0, debug: 0, total: 0 });
  }
  return { buckets, w };
}

/** Classify an unlabelled line by scanning its content (server omits level when unclassified). */
function levelFromLine(level: string, line: string): string {
  const lv = (level || "").toLowerCase();
  if (lv) return lv;
  const s = (line || "").toLowerCase();
  if (/\b(error|err|fatal|panic|exception)\b/.test(s)) return "error";
  if (/\b(warn|warning)\b/.test(s)) return "warn";
  if (/\bdebug\b/.test(s)) return "debug";
  return "info";
}

/** Wrap a LogQL selector+filter in count_over_time for the histogram query. */
function toHistogramLogQL(logql: string, range: Range): string {
  // Per-bucket window: span / BUCKETS, rounded to whole seconds (min 1s).
  const stepS = Math.max(1, Math.round(RANGE_MS[range] / BUCKETS / 1000));
  return `count_over_time(${logql} [${stepS}s])`;
}

/* ---------------- streaming runner ---------------- */

const DEFAULT_LIMIT = 2000;

/**
 * runQuery executes the LogQL query (rows) and a count_over_time variant
 * (histogram samples) against ObserveService.Execute, collecting both into a
 * RunResult. Aborts cleanly on the provided signal.
 */
export async function runQuery(
  query: LogQuery,
  range: Range,
  opts: { signal?: AbortSignal; limit?: number; forward?: boolean } = {},
): Promise<RunResult> {
  const end = Date.now();
  const span = RANGE_MS[range];
  const start = end - span;
  const startIso = new Date(start).toISOString();
  const endIso = new Date(end).toISOString();
  const logql = toLogQL(query);
  const ns = query.namespaces.length === 1 ? query.namespaces[0] : "";
  const t0 = performance.now();

  const { buckets, w } = emptyBuckets(start, span);

  // 1. log rows
  const lines: LogLine[] = [];
  const rowReq = new ObserveQuery({
    logql,
    start: startIso,
    end: endIso,
    limit: opts.limit ?? DEFAULT_LIMIT,
    forward: opts.forward ?? false,
    namespace: ns,
  });
  let n = 0;
  for await (const res of clients.observe.execute(rowReq, { signal: opts.signal })) {
    if (res.result.case !== "row") continue;
    const r = res.result.value;
    const t = Date.parse(r.timestamp) || end;
    const labels = r.labels ?? {};
    const level = levelFromLine(r.level, r.line);
    lines.push({
      id: `R${n++}`,
      t,
      ts: fmtClockMs(t),
      level,
      ns: labels.namespace ?? "",
      svc: labels.service ?? "",
      inst: labels.instance ?? "",
      node: labels.node ?? "",
      stream: r.stream,
      msg: r.line,
      labels,
    });
  }
  lines.sort((a, b) => b.t - a.t);

  // 2. histogram via count_over_time — samples carry per-bucket counts; the
  //    `level` group label splits them by severity. If the backend returns no
  //    samples (older builds), fall back to bucketing the rows we already have.
  let gotSamples = false;
  try {
    const histReq = new ObserveQuery({
      logql: toHistogramLogQL(logql, range),
      start: startIso,
      end: endIso,
      limit: 0,
      forward: true,
      namespace: ns,
    });
    for await (const res of clients.observe.execute(histReq, { signal: opts.signal })) {
      if (res.result.case !== "sample") continue;
      gotSamples = true;
      const s = res.result.value;
      const t = Date.parse(s.timestamp) || start;
      let idx = Math.floor((t - start) / w);
      if (idx < 0) idx = 0;
      if (idx >= BUCKETS) idx = BUCKETS - 1;
      const lvl = (s.groupLabels?.level ?? "").toLowerCase();
      const v = Math.round(s.value);
      const b = buckets[idx];
      if (lvl === "error" || lvl === "warn" || lvl === "info" || lvl === "debug") b[lvl] += v;
      else b.info += v;
      b.total += v;
    }
  } catch (e) {
    // a missing histogram seam must not fail the whole run.
    if (opts.signal?.aborted) throw e;
    gotSamples = false;
  }
  if (!gotSamples) {
    for (const l of lines) {
      let idx = Math.floor((l.t - start) / w);
      if (idx < 0) idx = 0;
      if (idx >= BUCKETS) idx = BUCKETS - 1;
      const b = buckets[idx];
      if (l.level === "error" || l.level === "warn" || l.level === "info" || l.level === "debug") b[l.level]++;
      else b.info++;
      b.total++;
    }
  }

  return {
    lines,
    buckets,
    bucketW: w,
    scannedRows: lines.length,
    durationMs: Math.round(performance.now() - t0),
  };
}

/* ---------------- capabilities handshake ---------------- */

export interface Capabilities {
  enabled: boolean;
  backend: string;
  maxTier: string;
  rawSql: boolean;
  percentiles: boolean;
  highCardinalityFilters: boolean;
}

/**
 * useCapabilities performs the dashboard handshake. When the call fails (no
 * ObserveService wired) we surface enabled=false so the UI shows the
 * "enable [observability]" empty state instead of a hard error.
 */
export function useCapabilities(): Query<Capabilities> {
  const [data, setData] = useState<Capabilities>({
    enabled: false, backend: "", maxTier: "", rawSql: false, percentiles: false, highCardinalityFilters: false,
  });
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [nonce, setNonce] = useState(0);
  const reload = useCallback(() => setNonce((x) => x + 1), []);

  useEffect(() => {
    const ctrl = new AbortController();
    let alive = true;
    setLoading(true);
    setError(null);
    clients.observe
      .getCapabilities(new CapabilitiesRequest(), { signal: ctrl.signal })
      .then((c) => {
        if (!alive) return;
        setData({
          enabled: c.enabled,
          backend: c.backend,
          maxTier: c.maxTier,
          rawSql: c.rawSql,
          percentiles: c.percentiles,
          highCardinalityFilters: c.highCardinalityFilters,
        });
        setLoading(false);
      })
      .catch((e: unknown) => {
        if (!alive || ctrl.signal.aborted) return;
        // Treat an unreachable seam as "disabled" — empty state, not an error.
        setData((d) => ({ ...d, enabled: false }));
        setError(e instanceof Error ? e.message : String(e));
        setLoading(false);
      });
    return () => { alive = false; ctrl.abort(); };
  }, [nonce]);

  return { data, loading, error, reload };
}

/* ---------------- derived: facets ---------------- */

export interface Facet {
  key: LabelKey;
  qf: keyof LogQuery;
  values: { v: string; c: number }[];
}

/** Build label facets from a result set — each label counted across the lines. */
export function facetsFromLines(lines: LogLine[]): Facet[] {
  const fieldOf: Record<LabelKey, (l: LogLine) => string> = {
    namespace: (l) => l.ns,
    service: (l) => l.svc,
    level: (l) => l.level,
    node: (l) => l.node,
    instance: (l) => l.inst,
  };
  return LABEL_KEYS.map((key) => {
    const counts = new Map<string, number>();
    for (const l of lines) {
      const v = fieldOf[key](l);
      if (!v) continue;
      counts.set(v, (counts.get(v) ?? 0) + 1);
    }
    const values = [...counts.entries()].map(([v, c]) => ({ v, c })).sort((a, b) => b.c - a.c);
    return { key, qf: QF[key], values };
  }).filter((f) => f.values.length > 0);
}

/** Toggle a value in a structured query's label list. */
export function toggleFacet(q: LogQuery, qf: keyof LogQuery, v: string): LogQuery {
  const cur = (q[qf] as string[]) ?? [];
  return { ...q, [qf]: cur.includes(v) ? cur.filter((x) => x !== v) : [...cur, v] };
}

export const FIELD_QF = QF;

/* ---------------- overview widget ---------------- */

export interface ObserveOverview {
  enabled: boolean;
  buckets: Bucket[];
  errors: number;
  warns: number;
  total: number;
}

/**
 * useObserveOverview powers the home-screen RuneSight card: a 1h level
 * histogram plus error/warn counts. Returns enabled=false (no card) when the
 * handshake reports observability off or the call is unreachable.
 */
export function useObserveOverview(range: Range = "1h"): Query<ObserveOverview> {
  const empty: ObserveOverview = { enabled: false, buckets: [], errors: 0, warns: 0, total: 0 };
  const [data, setData] = useState<ObserveOverview>(empty);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [nonce, setNonce] = useState(0);
  const reload = useCallback(() => setNonce((x) => x + 1), []);

  useEffect(() => {
    const ctrl = new AbortController();
    let alive = true;
    setLoading(true);
    setError(null);
    (async () => {
      try {
        const caps = await clients.observe.getCapabilities(new CapabilitiesRequest(), { signal: ctrl.signal });
        if (!alive) return;
        if (!caps.enabled) { setData({ ...empty, enabled: false }); setLoading(false); return; }
        const res = await runQuery(emptyQuery(), range, { signal: ctrl.signal });
        if (!alive) return;
        let errors = 0, warns = 0, total = 0;
        for (const b of res.buckets) { errors += b.error; warns += b.warn; total += b.total; }
        setData({ enabled: true, buckets: res.buckets, errors, warns, total });
        setLoading(false);
      } catch (e) {
        if (!alive || ctrl.signal.aborted) return;
        setData({ ...empty, enabled: false });
        setError(e instanceof Error ? e.message : String(e));
        setLoading(false);
      }
    })();
    return () => { alive = false; ctrl.abort(); };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [nonce, range]);

  return { data, loading, error, reload };
}
