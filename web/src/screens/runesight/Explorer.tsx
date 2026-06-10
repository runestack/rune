import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { Drawer, Icon } from "../../components";
import {
  emptyQuery, facetsFromLines, runQuery, toggleFacet, toLogQL, normQuery, parseLogQL, LABEL_KEYS,
  parseLogfmt, extractTraceId,
} from "../../api/observe";
import type { Bucket, Facet, LogLine, LogQuery, Range, RunResult } from "../../api/observe";
import { LogQLBar } from "./LogQLBar";
import { Highlighted } from "./LogQLBar";
import { deleteSavedView, listSavedViews, saveView, timeAgo, viewSlug } from "../../api/savedViews";
import type { SavedViewData } from "../../api/savedViews";
import { HISTORY_SEED } from "./mockData";
import type { RsTab } from "./RuneSight";

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
          <span><i className="info" />info</span><span><i className="warn" />warn</span><span><i className="error" />error</span>
        </span>
        <span>now</span>
      </div>
    </div>
  );
}

/* ---------- labels facet panel — collapsible · expandable · drag-to-reorder ---------- */
function LabelsPanel({ facets, query, toggle }: { facets: Facet[]; query: LogQuery; toggle: (qf: keyof LogQuery, v: string) => void }) {
  const keys: string[] = facets.map((f) => f.key);
  const byKey: Record<string, Facet> = {};
  facets.forEach((f) => { byKey[f.key] = f; });

  const [order, setOrder] = useState<string[]>(() => {
    try { const s = JSON.parse(localStorage.getItem("rs-facet-order") || "null"); if (Array.isArray(s)) return s; } catch { /* ignore */ }
    return keys;
  });
  const [collapsed, setCollapsed] = useState<Set<string>>(() => {
    try { return new Set(JSON.parse(localStorage.getItem("rs-facet-collapsed") || "[]")); } catch { return new Set(); }
  });
  const [expanded, setExpanded] = useState<Set<string>>(new Set());
  const [drag, setDrag] = useState<string | null>(null);
  const [over, setOver] = useState<string | null>(null);

  // reconcile saved order with the facet keys actually present
  const ordered = useMemo(() => {
    const present = order.filter((k) => keys.includes(k));
    const missing = keys.filter((k) => !present.includes(k));
    return [...present, ...missing];
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [order, facets]);

  useEffect(() => { try { localStorage.setItem("rs-facet-order", JSON.stringify(ordered)); } catch { /* ignore */ } }, [ordered]);
  useEffect(() => { try { localStorage.setItem("rs-facet-collapsed", JSON.stringify([...collapsed])); } catch { /* ignore */ } }, [collapsed]);

  const toggleCollapse = (k: string) => setCollapsed((s) => { const n = new Set(s); n.has(k) ? n.delete(k) : n.add(k); return n; });
  const toggleExpand = (k: string) => setExpanded((s) => { const n = new Set(s); n.has(k) ? n.delete(k) : n.add(k); return n; });
  function drop(targetKey: string) {
    if (drag && drag !== targetKey) {
      const arr = ordered.filter((k) => k !== drag);
      arr.splice(arr.indexOf(targetKey), 0, drag);
      setOrder(arr);
    }
    setDrag(null); setOver(null);
  }

  return (
    <div className="rs-labels">
      <div className="rs-panel-head"><span className="eyebrow">Labels</span><span className="rs-panel-count">{facets.length}</span></div>
      <div className="rs-labels-scroll">
        {ordered.map((key) => {
          const f = byKey[key];
          if (!f) return null;
          const isCol = collapsed.has(key);
          const isExp = expanded.has(key);
          const lim = isExp ? f.values.length : 10;
          return (
            <div className={"rs-facet" + (drag === key ? " dragging" : "") + (over === key && drag && drag !== key ? " dragover" : "")} key={key}
              onDragOver={(e) => { if (drag) { e.preventDefault(); if (over !== key) setOver(key); } }}
              onDrop={(e) => { e.preventDefault(); drop(key); }}>
              <div className={"rs-facet-key k-" + f.key} draggable
                onDragStart={(e) => { setDrag(key); e.dataTransfer.effectAllowed = "move"; try { e.dataTransfer.setData("text/plain", key); } catch { /* ignore */ } }}
                onDragEnd={() => { setDrag(null); setOver(null); }}
                onClick={() => toggleCollapse(key)} title="Click to collapse · drag to reorder">
                <Icon name="chevrond" size={12} stroke={2.4} style={{ flex: "none", transition: "transform .15s", transform: isCol ? "rotate(-90deg)" : "none" }} />
                <span className="rs-facet-keyname">{f.key}</span>
                <span className="rs-facet-keyc tnum">{f.values.length}</span>
              </div>
              {!isCol && f.values.slice(0, lim).map(({ v, c }) => {
                const on = (query[f.qf] as string[]).includes(v);
                return (
                  <div key={v} className={"rs-facet-val" + (on ? " on" : "")} onClick={() => toggle(f.qf, v)}>
                    <span className={"rs-facet-dot" + (f.key === "level" ? " lvl-" + v : "")} />
                    <span className="rs-facet-name mono">{v}</span>
                    <span className="rs-facet-c tnum">{c >= 1000 ? (c / 1000).toFixed(1) + "k" : c}</span>
                  </div>
                );
              })}
              {!isCol && f.values.length > 10 && (
                <button className="rs-facet-more" onClick={() => toggleExpand(key)}>
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

/* ---------- save-query modal ---------- */
function SaveQueryModal({ query, onClose, onSave }: { query: LogQuery; onClose: () => void; onSave: (v: { name: string; desc: string; pinned: boolean }) => void }) {
  const [name, setName] = useState("");
  const [desc, setDesc] = useState("");
  const [pinned, setPinned] = useState(false);
  const inputRef = useRef<HTMLInputElement>(null);
  useEffect(() => { const id = setTimeout(() => inputRef.current?.focus(), 30); return () => clearTimeout(id); }, []);
  const qstr = toLogQL(query);
  function submit() { if (!name.trim()) return; onSave({ name: name.trim(), desc: desc.trim(), pinned }); onClose(); }
  return (
    <div className="rs-modal-scrim" onMouseDown={onClose}>
      <div className="rs-modal" onMouseDown={(e) => e.stopPropagation()}>
        <div className="rs-modal-head"><div className="eyebrow">Save query</div>
          <button className="rs-modal-x" onClick={onClose}><Icon name="close" size={16} /></button>
        </div>
        <div className="rs-modal-body">
          <label className="rs-field"><span className="rs-field-l">Name</span>
            <input ref={inputRef} className="rs-field-i" value={name} placeholder="e.g. Payment 5xx errors"
              onChange={(e) => setName(e.target.value)} onKeyDown={(e) => { if (e.key === "Enter") submit(); if (e.key === "Escape") onClose(); }} /></label>
          <label className="rs-field"><span className="rs-field-l">Description <em>optional</em></span>
            <input className="rs-field-i" value={desc} placeholder="What this query is for"
              onChange={(e) => setDesc(e.target.value)} onKeyDown={(e) => { if (e.key === "Enter") submit(); }} /></label>
          <div className="rs-field"><span className="rs-field-l">Query</span>
            <div className="rs-field-q mono"><Highlighted text={qstr} /></div>
          </div>
          <label className="rs-pinrow" onClick={() => setPinned((p) => !p)}>
            <span className={"rs-pinbox" + (pinned ? " on" : "")}>{pinned ? <Icon name="check" size={12} /> : null}</span>
            Pin to top of saved queries
          </label>
        </div>
        <div className="rs-modal-foot">
          <button className="rs-paneltgl" style={{ height: 34, padding: "0 13px" }} onClick={onClose}>Cancel</button>
          <button className="rs-run primary" disabled={!name.trim()} onClick={submit}><Icon name="check" size={14} />Save query</button>
        </div>
      </div>
    </div>
  );
}

/* ---------- saved queries + history rail ---------- */
interface HistEntry { q: Partial<LogQuery>; ts: string; hits: string; dur: string }
function QueriesRail({ loadView, history, currentQuery, views, onSave, onDelete, onViewAll }: {
  loadView: (q: Partial<LogQuery>) => void;
  history: HistEntry[];
  currentQuery: LogQuery;
  views: SavedViewData[];
  onSave: (v: { name: string; desc: string; pinned: boolean }) => void;
  onDelete: (name: string) => void;
  onViewAll: () => void;
}) {
  const [saving, setSaving] = useState(false);
  const [confirmId, setConfirmId] = useState<string | null>(null);
  const parse = (logql: string): Partial<LogQuery> => { try { return parseLogQL(logql); } catch { return {}; } };
  return (
    <div className="rs-saved">
      <div className="rs-panel-head"><span className="eyebrow">Saved queries</span>
        <div style={{ display: "flex", alignItems: "center", gap: 8 }}>
          <button className="rs-viewall" onClick={onViewAll}>View all <Icon name="chevron" size={12} /></button>
          <button className="rs-panel-add" title="Save current query" onClick={() => setSaving(true)}><Icon name="plus" size={14} /></button>
        </div>
      </div>
      <div className="rs-saved-scroll">
        {views.length === 0 && <div className="rs-saved-empty">No saved queries. Press <Icon name="plus" size={11} /> to save the current one.</div>}
        {views.map((v) => (
          <div key={v.id} className={"rs-saved-item" + (confirmId === v.id ? " confirming" : "")} onClick={() => loadView(parse(v.logql))}>
            <div className="rs-saved-name">{v.name}{v.pinned && <Icon name="pin" size={11} />}</div>
            {v.description && <div className="rs-saved-desc">{v.description}</div>}
            <div className="rs-saved-q mono">{v.logql}</div>
            <div className="rs-saved-owner">{v.createdBy || "—"} · {timeAgo(v.updatedAt)}</div>
            {confirmId === v.id ? (
              <div className="rs-saved-confirm" onClick={(e) => e.stopPropagation()}>
                <span>Delete?</span>
                <button className="rs-sc-yes" onClick={() => { onDelete(v.name); setConfirmId(null); }}>Delete</button>
                <button className="rs-sc-no" onClick={() => setConfirmId(null)}>Cancel</button>
              </div>
            ) : (
              <button className="rs-saved-del" title="Delete saved query"
                onClick={(e) => { e.stopPropagation(); setConfirmId(v.id); }}><Icon name="close" size={13} /></button>
            )}
          </div>
        ))}
        <div className="rs-panel-head" style={{ marginTop: 6 }}><span className="eyebrow">History</span><span className="rs-panel-count">recent</span></div>
        {history.map((h, i) => (
          <button key={i} className="rs-hist-item" onClick={() => loadView(h.q)}>
            <div className="rs-saved-q mono">{toLogQL(normQuery(h.q))}</div>
            <div className="rs-hist-meta mono">{h.ts} · {h.hits} hits · {h.dur}</div>
          </button>
        ))}
      </div>
      {saving && <SaveQueryModal query={currentQuery} onClose={() => setSaving(false)} onSave={onSave} />}
    </div>
  );
}

/* ---------- detail drawer ---------- */
function LogDetail({ line, onClose, onFilter, contextLines, openInstance, openService }: {
  line: LogLine; onClose: () => void; onFilter: (qf: keyof LogQuery | "text", v: string) => void; contextLines: LogLine[];
  openInstance: (name: string, ns?: string) => void; openService: (name: string, ns?: string) => void;
}) {
  // A label row: the value can be a plain string, a filter affordance, an
  // out-of-RuneSight back-link, or a free-text search action.
  const Row = ({ k, v, qf, val, searchVal, onLink }: {
    k: string; v: string; qf?: keyof LogQuery; val?: string; searchVal?: string; onLink?: () => void;
  }) => (
    <div className="rs-d-row">
      <span className="rs-d-k">{k}</span>
      {onLink
        ? <button className="rs-d-link mono" onClick={onLink} title={`Open ${k} "${v}" in Rune`}>{v}<Icon name="external" size={11} /></button>
        : <span className="rs-d-v mono">{v}</span>}
      {qf && val && <button className="rs-d-add" title={`Filter ${k}="${val}"`} onClick={() => onFilter(qf, val)}><Icon name="filter" size={12} /></button>}
      {searchVal && <button className="rs-d-add" title={`Search "${searchVal}"`} onClick={() => onFilter("text", searchVal)}><Icon name="search" size={12} /></button>}
    </div>
  );
  const extra = Object.entries(line.labels).filter(([k]) => !LABEL_KEYS.includes(k as never) && k !== "pod_ip");
  // Parse the raw message as logfmt and detect a trace id (parsed fields first,
  // then the raw line). Memoised against the message so we don't re-parse on
  // every render of the drawer.
  const fields = useMemo(() => parseLogfmt(line.msg), [line.msg]);
  const fieldEntries = useMemo(() => Object.entries(fields), [fields]);
  const traceId = useMemo(() => extractTraceId(fields, line.msg), [fields, line.msg]);
  return (
    <Drawer onClose={onClose}>
      <div className="drawer-head">
        <div className="eyebrow" style={{ marginBottom: 8 }}>Log line · {line.ts}</div>
        <div style={{ display: "flex", alignItems: "center", gap: 10, marginBottom: 14, flexWrap: "wrap" }}>
          <span className={"rs-lvl-badge " + line.level}>{LVL_LABEL[line.level] ?? line.level.toUpperCase()}</span>
          {line.svc && <span className="rs-d-src" style={{ fontSize: 14 }}>{line.svc}</span>}
        </div>
        <div className="rs-d-msg mono">{line.msg}</div>
      </div>
      <div className="drawer-body">
        <div className="rs-d-sec eyebrow">Labels</div>
        <div className="rs-d-grid">
          {line.ns && <Row k="namespace" v={line.ns} qf="namespaces" val={line.ns} />}
          {line.svc && <Row k="service" v={line.svc} qf="services" val={line.svc} onLink={() => openService(line.svc, line.ns || undefined)} />}
          <Row k="level" v={line.level} qf="levels" val={line.level} />
          {line.node && <Row k="node" v={line.node} qf="nodes" val={line.node} />}
          {line.inst && <Row k="instance" v={line.inst} qf="instances" val={line.inst} onLink={() => openInstance(line.inst, line.ns || undefined)} />}
          {line.stream && <Row k="stream" v={line.stream} />}
        </div>
        {fieldEntries.length > 0 && (
          <>
            <div className="rs-d-sec eyebrow">Parsed fields</div>
            <div className="rs-d-grid">
              {fieldEntries.map(([k, v]) => <Row key={k} k={k} v={v} searchVal={v} />)}
            </div>
          </>
        )}
        {traceId && (
          <>
            <div className="rs-d-sec eyebrow">Trace</div>
            <div className="rs-d-grid">
              <Row k="trace_id" v={traceId} searchVal={traceId} />
            </div>
          </>
        )}
        {extra.length > 0 && (
          <>
            <div className="rs-d-sec eyebrow">Stream labels</div>
            <div className="rs-d-grid">{extra.map(([k, v]) => <Row key={k} k={k} v={v} />)}</div>
          </>
        )}
        {line.inst && contextLines.length > 1 && (
          <>
            <div className="rs-d-sec eyebrow">Context · {line.inst}</div>
            <div className="rs-d-grid" style={{ background: "var(--inset)", padding: "8px 0" }}>
              {contextLines.map((c) => (
                <div key={c.id} className="rs-d-row" style={{ background: c.id === line.id ? "var(--accent-dim)" : "var(--inset)", opacity: c.id === line.id ? 1 : 0.6, gap: 10 }}>
                  <span className="rs-d-k" style={{ width: 78 }}>{c.ts.slice(0, 8)}</span>
                  <span className={"rs-lvl " + c.level} style={{ width: 42, flex: "none" }}>{LVL_LABEL[c.level] ?? c.level.toUpperCase()}</span>
                  <span className="rs-d-v mono" style={{ whiteSpace: "nowrap", overflow: "hidden", textOverflow: "ellipsis" }}>{c.msg}</span>
                </div>
              ))}
            </div>
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
  go: (tab: RsTab) => void;
  loadView: (q: Partial<LogQuery>) => void;
  openInstance: (name: string, ns?: string) => void;
  openService: (name: string, ns?: string) => void;
}

export function Explorer({ query, setQuery, range, setRange, live, setLive, go, loadView, openInstance, openService }: ExplorerProps) {
  const [sel, setSel] = useState<LogLine | null>(null);
  const [limit, setLimit] = useState(120);
  const [activeBucket, setActiveBucket] = useState<number | null>(null);
  const [showLabels, setShowLabels] = useState(() => { try { return localStorage.getItem("rs-labels") !== "0"; } catch { return true; } });
  const [showSaved, setShowSaved] = useState(() => { try { return localStorage.getItem("rs-saved") === "1"; } catch { return false; } });
  const [result, setResult] = useState<RunResult | null>(null);
  const [running, setRunning] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [history, setHistory] = useState<HistEntry[]>(HISTORY_SEED);
  const [savedViews, setSavedViews] = useState<SavedViewData[]>([]);
  const ctrlRef = useRef<AbortController | null>(null);

  useEffect(() => { try { localStorage.setItem("rs-labels", showLabels ? "1" : "0"); } catch { /* ignore */ } }, [showLabels]);
  useEffect(() => { try { localStorage.setItem("rs-saved", showSaved ? "1" : "0"); } catch { /* ignore */ } }, [showSaved]);

  const refreshViews = useCallback(() => {
    listSavedViews()
      .then(setSavedViews)
      .catch((e: unknown) => console.error("listSavedViews:", e));
  }, []);
  useEffect(() => { refreshViews(); }, [refreshViews]);

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

  useEffect(() => {
    setLimit(120);
    setActiveBucket(null);
    execute(query, range);
    return () => ctrlRef.current?.abort();
  }, [query, range, execute]);

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

  function run(parsed: LogQuery) {
    setQuery(parsed);
    setHistory((h) => [{
      q: parsed,
      ts: new Date().toISOString().slice(11, 19),
      hits: total >= 1000 ? (total / 1000).toFixed(1) + "k" : String(total),
      dur,
    }, ...h].slice(0, 6));
  }

  function saveQuery({ name, desc, pinned }: { name: string; desc: string; pinned: boolean }) {
    saveView({ name: viewSlug(name), description: desc, logql: toLogQL(normQuery(query)), range, pinned })
      .then(refreshViews)
      .catch((e: unknown) => console.error("saveView:", e));
  }
  function deleteView(name: string) {
    deleteSavedView(name)
      .then(refreshViews)
      .catch((e: unknown) => console.error("deleteSavedView:", e));
  }

  // Context: surrounding lines from the same instance in the current result.
  const contextLines = useMemo(() => {
    if (!sel || !sel.inst) return [];
    const same = all.filter((l) => l.inst === sel.inst).sort((a, b) => a.t - b.t);
    const idx = same.findIndex((l) => l.id === sel.id);
    if (idx < 0) return [];
    return same.slice(Math.max(0, idx - 5), idx + 6);
  }, [sel, all]);

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
        run={run}
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
        <button className={"rs-paneltgl" + (showSaved ? " on" : "")} onClick={() => setShowSaved((v) => !v)} title="Toggle saved queries">
          <Icon name="logs" size={14} />Saved
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

        {showSaved && (
          <QueriesRail
            loadView={loadView}
            history={history}
            currentQuery={query}
            views={savedViews}
            onSave={saveQuery}
            onDelete={deleteView}
            onViewAll={() => go("views")}
          />
        )}
      </div>

      {sel && (
        <LogDetail
          line={sel}
          onClose={() => setSel(null)}
          onFilter={applyFilter}
          contextLines={contextLines}
          openInstance={openInstance}
          openService={openService}
        />
      )}
    </div>
  );
}

export { emptyQuery };
