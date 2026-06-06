import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { Drawer, Icon } from "../../components";
import {
  emptyQuery, facetsFromLines, runQuery, toggleFacet, LABEL_KEYS,
} from "../../api/observe";
import type { Bucket, Facet, LogLine, LogQuery, Range, RunResult } from "../../api/observe";
import { LogQLBar } from "./LogQLBar";

const LVL_LABEL: Record<string, string> = { error: "ERROR", warn: "WARN", info: "INFO", debug: "DEBUG" };
const REFRESH_MS = 5000;

function Highlight({ text, term }: { text: string; term: string }) {
  if (!term) return <>{text}</>;
  const i = text.toLowerCase().indexOf(term.toLowerCase());
  if (i < 0) return <>{text}</>;
  return <>{text.slice(0, i)}<mark className="rs-mark">{text.slice(i, i + term.length)}</mark>{text.slice(i + term.length)}</>;
}

/* ---------- histogram ---------- */
function Histogram({ buckets, onBucket, activeBucket }: { buckets: Bucket[]; onBucket: (i: number | null) => void; activeBucket: number | null }) {
  const [hover, setHover] = useState<number | null>(null);
  const sorted = buckets.map((b) => b.total).filter((x) => x > 0).sort((a, b) => b - a);
  const max = Math.max(1, sorted[Math.floor(sorted.length * 0.08)] || sorted[0] || 1);
  const fmt = (t: number) => new Date(t).toISOString().slice(11, 16);
  return (
    <div className="rs-hist-wrap">
      <div className="rs-hist">
        {buckets.map((b, i) => (
          <div key={i} className={"rs-bar" + (activeBucket === i ? " active" : "")}
            onMouseEnter={() => setHover(i)} onMouseLeave={() => setHover(null)}
            onClick={() => onBucket(activeBucket === i ? null : i)}>
            {b.total === 0 ? <span className="rs-bar-empty" /> : (
              <>
                {b.debug > 0 && <span className="rs-seg debug" style={{ height: `${Math.min(100, (b.debug / max) * 100)}%` }} />}
                {b.info > 0 && <span className="rs-seg info" style={{ height: `${Math.min(100, (b.info / max) * 100)}%` }} />}
                {b.warn > 0 && <span className="rs-seg warn" style={{ height: `${Math.min(100, (b.warn / max) * 100)}%` }} />}
                {b.error > 0 && <span className="rs-seg error" style={{ height: `${Math.min(100, (b.error / max) * 100)}%` }} />}
              </>
            )}
          </div>
        ))}
        {hover != null && buckets[hover] && (
          <div className="rs-hist-tip" style={{ left: `${((hover + 0.5) / buckets.length) * 100}%` }}>
            <div className="rs-tip-time">{fmt(buckets[hover].t0)}</div>
            <div className="rs-tip-row"><b>{buckets[hover].total}</b> lines</div>
            {buckets[hover].error > 0 && <div className="rs-tip-row"><span className="rs-tip-dot error" />{buckets[hover].error} error</div>}
            {buckets[hover].warn > 0 && <div className="rs-tip-row"><span className="rs-tip-dot warn" />{buckets[hover].warn} warn</div>}
            {buckets[hover].info > 0 && <div className="rs-tip-row"><span className="rs-tip-dot info" />{buckets[hover].info} info</div>}
          </div>
        )}
      </div>
      <div className="rs-hist-axis">
        <span>{buckets[0] ? fmt(buckets[0].t0) : ""}</span>
        <span className="rs-hist-legend">
          <span><i className="rs-tip-dot info" />info</span><span><i className="rs-tip-dot warn" />warn</span><span><i className="rs-tip-dot error" />error</span>
        </span>
        <span>now</span>
      </div>
    </div>
  );
}

/* ---------- labels facet panel ---------- */
function LabelsPanel({ facets, query, toggle }: { facets: Facet[]; query: LogQuery; toggle: (qf: keyof LogQuery, v: string) => void }) {
  const [expanded, setExpanded] = useState<Set<string>>(new Set());
  const toggleExpand = (k: string) => setExpanded((s) => { const n = new Set(s); n.has(k) ? n.delete(k) : n.add(k); return n; });
  return (
    <div className="rs-labels">
      <div className="rs-panel-head"><span className="eyebrow">Labels</span><span className="rs-panel-count">{facets.length}</span></div>
      <div className="rs-labels-scroll">
        {facets.map((f) => {
          const isExp = expanded.has(f.key);
          const lim = isExp ? f.values.length : 10;
          return (
            <div className="rs-facet" key={f.key}>
              <div className={"rs-facet-key k-" + f.key}>
                <span className="rs-facet-keyname">{f.key}</span>
                <span className="rs-facet-keyc tnum">{f.values.length}</span>
              </div>
              {f.values.slice(0, lim).map(({ v, c }) => {
                const on = (query[f.qf] as string[]).includes(v);
                return (
                  <div key={v} className={"rs-facet-val" + (on ? " on" : "")} onClick={() => toggle(f.qf, v)}>
                    <span className={"rs-facet-dot" + (f.key === "level" ? " lvl-" + v : "")} />
                    <span className="rs-facet-name mono">{v}</span>
                    <span className="rs-facet-c tnum">{c >= 1000 ? (c / 1000).toFixed(1) + "k" : c}</span>
                  </div>
                );
              })}
              {f.values.length > 10 && (
                <button className="rs-facet-more" onClick={() => toggleExpand(f.key)}>
                  <Icon name="chevrond" size={11} stroke={2.4} style={{ transition: "transform .15s", transform: isExp ? "rotate(180deg)" : "none" }} />
                  {isExp ? "Show less" : `+${f.values.length - 10} more`}
                </button>
              )}
            </div>
          );
        })}
      </div>
    </div>
  );
}

/* ---------- detail drawer ---------- */
function LogDetail({ line, onClose, onFilter }: { line: LogLine; onClose: () => void; onFilter: (qf: keyof LogQuery | "text", v: string) => void }) {
  const Row = ({ k, v, qf, val }: { k: string; v: string; qf?: keyof LogQuery; val?: string }) => (
    <div className="rs-d-row">
      <span className="rs-d-k">{k}</span>
      <span className="rs-d-v mono">{v}</span>
      {qf && val && <button className="rs-d-add" title={`Filter ${k}="${val}"`} onClick={() => onFilter(qf, val)}><Icon name="filter" size={12} /></button>}
    </div>
  );
  // user labels = everything not in the fixed identity set
  const extra = Object.entries(line.labels).filter(([k]) => !LABEL_KEYS.includes(k as never) && k !== "pod_ip");
  return (
    <Drawer onClose={onClose}>
      <div className="drawer-head">
        <div className="eyebrow" style={{ marginBottom: 8 }}>Log line · {line.ts}</div>
        <div style={{ display: "flex", alignItems: "center", gap: 10, marginBottom: 14, flexWrap: "wrap" }}>
          <span className={"rs-lvl-badge " + line.level}>{LVL_LABEL[line.level] ?? line.level.toUpperCase()}</span>
          {line.svc && <span style={{ fontSize: 14, color: "var(--accent-text)" }}>{line.svc}</span>}
        </div>
        <div className="rs-d-msg mono">{line.msg}</div>
      </div>
      <div className="drawer-body">
        <div className="rs-d-sec eyebrow">Labels</div>
        <div className="rs-d-grid">
          {line.ns && <Row k="namespace" v={line.ns} qf="namespaces" val={line.ns} />}
          {line.svc && <Row k="service" v={line.svc} qf="services" val={line.svc} />}
          <Row k="level" v={line.level} qf="levels" val={line.level} />
          {line.node && <Row k="node" v={line.node} qf="nodes" val={line.node} />}
          {line.inst && <Row k="instance" v={line.inst} qf="instances" val={line.inst} />}
          {line.stream && <Row k="stream" v={line.stream} />}
        </div>
        {extra.length > 0 && (
          <>
            <div className="rs-d-sec eyebrow">Stream labels</div>
            <div className="rs-d-grid">{extra.map(([k, v]) => <Row key={k} k={k} v={v} />)}</div>
          </>
        )}
      </div>
    </Drawer>
  );
}

/* ---------- explorer ---------- */
export interface ExplorerProps {
  query: LogQuery;
  setQuery: (q: LogQuery | ((q: LogQuery) => LogQuery)) => void;
  range: Range;
  setRange: (r: Range) => void;
  live: boolean;
  setLive: (fn: (l: boolean) => boolean) => void;
}

export function Explorer({ query, setQuery, range, setRange, live, setLive }: ExplorerProps) {
  const [sel, setSel] = useState<LogLine | null>(null);
  const [limit, setLimit] = useState(120);
  const [activeBucket, setActiveBucket] = useState<number | null>(null);
  const [showLabels, setShowLabels] = useState(() => { try { return localStorage.getItem("rs-labels") !== "0"; } catch { return true; } });
  const [result, setResult] = useState<RunResult | null>(null);
  const [running, setRunning] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const ctrlRef = useRef<AbortController | null>(null);

  useEffect(() => { try { localStorage.setItem("rs-labels", showLabels ? "1" : "0"); } catch { /* ignore */ } }, [showLabels]);

  const execute = useCallback((q: LogQuery, r: Range) => {
    ctrlRef.current?.abort();
    const ctrl = new AbortController();
    ctrlRef.current = ctrl;
    setRunning(true);
    setError(null);
    runQuery(q, r, { signal: ctrl.signal })
      .then((res) => { if (!ctrl.signal.aborted) { setResult(res); setRunning(false); } })
      .catch((e: unknown) => {
        if (ctrl.signal.aborted) return;
        setError(e instanceof Error ? e.message : String(e));
        setRunning(false);
      });
  }, []);

  // run on query/range change
  useEffect(() => {
    setLimit(120);
    setActiveBucket(null);
    execute(query, range);
    return () => ctrlRef.current?.abort();
  }, [query, range, execute]);

  // auto-refresh (live tail) — re-run on an interval without resetting the view
  useEffect(() => {
    if (!live) return;
    const id = setInterval(() => execute(query, range), REFRESH_MS);
    return () => clearInterval(id);
  }, [live, query, range, execute]);

  const all = result?.lines ?? [];
  const buckets = result?.buckets ?? [];
  const bucketW = result?.bucketW ?? 0;
  const facets = useMemo(() => facetsFromLines(all), [all]);

  let shown = all;
  if (activeBucket != null && buckets[activeBucket]) {
    const b = buckets[activeBucket];
    shown = all.filter((l) => l.t >= b.t0 && l.t < b.t0 + bucketW);
  }
  const total = shown.length;
  const view = shown.slice(0, limit);
  const counts = useMemo(() => {
    const c = { error: 0, warn: 0, info: 0, debug: 0 } as Record<string, number>;
    for (const l of all) c[l.level] = (c[l.level] ?? 0) + 1;
    return c;
  }, [all]);

  const scanned = ((result?.scannedRows ?? 0) * 0.41 / 1000).toFixed(1);
  const dur = (result?.durationMs ?? 0) + "ms";

  const toggle = useCallback((qf: keyof LogQuery, v: string) => setQuery((q) => toggleFacet(q, qf, v)), [setQuery]);
  const applyFilter = useCallback((key: keyof LogQuery | "text", val: string) => {
    setSel(null);
    setQuery((q) => key === "text"
      ? { ...q, text: val }
      : { ...q, [key]: (q[key] as string[]).includes(val) ? q[key] : [...(q[key] as string[]), val] });
  }, [setQuery]);

  return (
    <div className="rs-explorer">
      <LogQLBar
        query={query}
        range={range}
        live={live}
        running={running}
        labelValues={useMemo(() => {
          const m: Record<string, string[]> = {};
          for (const f of facets) m[f.key] = f.values.map((x) => x.v);
          return m;
        }, [facets])}
        setRange={setRange}
        setLive={setLive}
        run={(q) => setQuery(q)}
      />

      <div className="rs-hitbar">
        <span className="rs-hits"><b>{total.toLocaleString()}</b> hits</span>
        <span className="rs-meta-sep">·</span><span className="rs-dim">scanned {scanned} GiB</span>
        <span className="rs-meta-sep">·</span><span className="rs-dim">{dur}</span>
        <span className="rs-meta-sep">·</span><span className="rs-dim">7d retention</span>
        {counts.error > 0 && <span className="rs-cnt error" style={{ marginLeft: 10 }}>{counts.error} error</span>}
        {counts.warn > 0 && <span className="rs-cnt warn">{counts.warn} warn</span>}
        {activeBucket != null && buckets[activeBucket] && (
          <span className="rs-bucket-tag" onClick={() => setActiveBucket(null)}>
            bucket {new Date(buckets[activeBucket].t0).toISOString().slice(11, 16)} <Icon name="close" size={11} />
          </span>
        )}
        <span className="rs-spacer" />
        {live && <span className="rs-livetag"><span className="rs-live-dot" />streaming</span>}
        <button className={"rs-paneltgl" + (showLabels ? " on" : "")} onClick={() => setShowLabels((v) => !v)} title="Toggle labels panel">
          <Icon name="filter" size={14} />Labels
        </button>
      </div>

      <Histogram buckets={buckets} onBucket={setActiveBucket} activeBucket={activeBucket} />

      <div className="rs-cols">
        {showLabels && <LabelsPanel facets={facets} query={query} toggle={toggle} />}

        <div className="rs-stream-col">
          <div className="rs-stream-head"><span>TIMESTAMP</span><span>LEVEL</span><span>LABELS</span><span>MESSAGE</span></div>
          <div className="rs-stream">
            {error && (
              <div className="rs-empty">
                <Icon name="alert" size={26} /><div className="rs-empty-t">Query failed</div><div className="rs-empty-s">{error}</div>
              </div>
            )}
            {!error && view.length === 0 && !running && (
              <div className="rs-empty">
                <Icon name="search" size={26} /><div className="rs-empty-t">No lines match this query</div>
                <div className="rs-empty-s">Widen the range or remove a label matcher.</div>
              </div>
            )}
            {!error && view.map((l, i) => (
              <div key={l.id} className={"rs-row " + l.level + (sel && sel.id === l.id ? " sel" : "") + (live && i === 0 ? " fresh" : "")} onClick={() => setSel(l)}>
                <span className="t">{l.ts}</span>
                <span className={"rs-lvl " + l.level}>{LVL_LABEL[l.level] ?? l.level.toUpperCase()}</span>
                <span className="rs-row-labels">
                  {l.svc && <span className="rs-chip" onClick={(e) => { e.stopPropagation(); toggle("services", l.svc); }}><b>service</b>={l.svc}</span>}
                  {l.inst && <span className="rs-chip dim" onClick={(e) => { e.stopPropagation(); toggle("instances", l.inst); }}><b>instance</b>={l.inst}</span>}
                </span>
                <span className="rs-msg"><Highlight text={l.msg} term={query.text} /></span>
              </div>
            ))}
            {!error && total > view.length && (
              <button className="rs-loadmore" onClick={() => setLimit((l) => l + 200)}>
                Showing {view.length.toLocaleString()} of {total.toLocaleString()} — load 200 more
              </button>
            )}
            {!error && total > 0 && total <= view.length && <div className="rs-streamend">— end of results in range —</div>}
          </div>
        </div>
      </div>

      {sel && <LogDetail line={sel} onClose={() => setSel(null)} onFilter={applyFilter} />}
    </div>
  );
}

export { emptyQuery };
