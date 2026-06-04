import { Fragment, useEffect, useLayoutEffect, useRef, useState } from "react";
import { Dot, Dropdown, Icon, PageHead, Segmented } from "../components";
import { RUNE } from "../mock/data";
import type { Instance, Service } from "../mock/data";
import { execBanner, execRun, type ExecLine } from "../lib/execEngine";
import { useDemo } from "../api/demo";
import { useInstances, useServices } from "../api/hooks";
import { openExecSession, streamLogs, type ExecSession, type LiveLogLine } from "../api/streams";
import "./Logs.css";

/* ---------- scalable pickers ---------- */
function ServicePicker({ svc, onPick, services }: { svc: string; onPick: (n: string) => void; services: Service[] }) {
  const [q, setQ] = useState("");
  const cur = services.find((s) => s.name === svc);
  const list = services.filter((s) => (s.ports.length || s.type === "container") && s.name.toLowerCase().includes(q.toLowerCase()));
  return (
    <Dropdown width={300} label={<span className="dd-lab"><span className="eyebrow">service</span><Dot s={cur ? cur.status : "run"} /><b>{svc}</b></span>}>
      {(close) => (
        <>
          <div className="dd-search"><Icon name="search" size={13} /><input autoFocus placeholder="Filter services…" value={q} onChange={(e) => setQ(e.target.value)} /></div>
          <div className="dd-list">
            {list.length === 0 && <div className="dd-empty">No services match “{q}”.</div>}
            {list.map((s) => (
              <div key={s.name} className={`dd-item${s.name === svc ? " sel" : ""}`} onClick={() => { onPick(s.name); close(); }}>
                <Dot s={s.status} /><span>{s.name}</span><span className="tag ddi-sub">{s.ns}</span>
              </div>
            ))}
          </div>
        </>
      )}
    </Dropdown>
  );
}

function ScopePicker({ mode, svc, inst, setInst, instances }: { mode: string; svc: string; inst: string | null; setInst: (i: string | null) => void; instances: Instance[] }) {
  const insts = instances.filter((i) => i.svc === svc);
  const autoLabel = mode === "logs" ? "All instances" : "Auto · first healthy";
  const curInst = insts.find((i) => i.id === inst);
  return (
    <Dropdown width={320} label={
      <span className="dd-lab">
        <span className="eyebrow">{mode === "logs" ? "scope" : "target"}</span>
        {curInst ? <><Dot s={curInst.status} /><b className="mono" style={{ fontSize: 12 }}>{curInst.id}</b></> : <b>{autoLabel}</b>}
      </span>
    }>
      {(close) => (
        <div className="dd-list">
          <div className={`dd-item${!inst ? " sel" : ""}`} onClick={() => { setInst(null); close(); }}>
            <Icon name={mode === "logs" ? "instances" : "bolt"} size={14} /><span>{autoLabel}</span><span className="tag ddi-sub">{insts.length} live</span>
          </div>
          <div className="dd-sep" />
          {insts.map((i) => (
            <div key={i.id} className={`dd-item${inst === i.id ? " sel" : ""}`} onClick={() => { setInst(i.id); close(); }}>
              <Dot s={i.status} /><span className="mono" style={{ fontSize: 12 }}>{i.id}</span><span className="tag ddi-sub">{i.node}</span>
            </div>
          ))}
        </div>
      )}
    </Dropdown>
  );
}

// Strip ANSI/VT control sequences (CSI, OSC, single ESC pairs) and other
// non-printing control bytes from PTY output so the line-based terminal renders
// clean text. Keeps tab; drops the rest of C0 controls.
const ESC = String.fromCharCode(27);
const BEL = String.fromCharCode(7);
const ANSI_CSI = new RegExp(ESC + "\\[[0-9;?]*[ -/]*[@-~]", "g");
const ANSI_OSC = new RegExp(ESC + "\\][^" + BEL + ESC + "]*(?:" + BEL + "|" + ESC + "\\\\)", "g");
const ANSI_ESC = new RegExp(ESC + "[@-_]", "g");
const C0 = new RegExp("[\\x00-\\x08\\x0b-\\x1f\\x7f]", "g");
function stripAnsi(s: string): string {
  return s.replace(ANSI_CSI, "").replace(ANSI_OSC, "").replace(ANSI_ESC, "").replace(C0, "");
}

/* ---------- log formats ---------- */
type Fmt = "logfmt" | "json" | "clf" | "plain";
interface RawLine { ts: string; level: string; raw: string; fmt: Fmt; origin: string }

const LOGFMT_T: [string, string][] = [
  ["info", "request completed method=GET path=/v1/orders status=200 dur=42ms"],
  ["info", "request completed method=POST path=/v1/checkout status=201 dur=118ms"],
  ["debug", "cache hit key=user:48211 ttl=290s"],
  ["info", "request completed method=GET path=/healthz status=200 dur=1ms"],
  ["warn", "upstream latency high service=payments p99=820ms"],
  ["info", "db pool acquired conn=11/25 wait=0ms"],
  ["error", "request failed method=POST path=/v1/pay status=503 upstream=payments retry=1/3"],
  ["info", "grpc method=auth.Verify ok=true subject=user:48211 dur=8ms"],
  ["warn", "rate limit applied client=10.244.1.18 bucket=api-anon remaining=0"],
];
const JSON_T: [string, string][] = [
  ["info", '{"level":"info","msg":"job processed","queue":"emails","id":"j_8821","dur_ms":210}'],
  ["info", '{"level":"info","msg":"job processed","queue":"webhooks","id":"j_8839","dur_ms":96}'],
  ["warn", '{"level":"warn","msg":"retry scheduled","queue":"emails","attempt":2,"backoff_ms":500}'],
  ["debug", '{"level":"debug","msg":"lease renewed","worker":"w-3","ttl_s":30}'],
  ["error", '{"level":"error","msg":"handler panic","queue":"emails","err":"nil pointer deref"}'],
];
const CLF_T: [string, string][] = [
  ["info", '10.244.1.18 - - [DATE] "GET /v1/orders HTTP/1.1" 200 1243 "-" "curl/8.4.0"'],
  ["info", '10.244.1.20 - - [DATE] "POST /v1/checkout HTTP/1.1" 201 318 "-" "acme-web/2.3"'],
  ["warn", '10.244.1.33 - - [DATE] "GET /v1/admin HTTP/1.1" 403 122 "-" "curl/8.4.0"'],
  ["error", '10.244.1.18 - - [DATE] "GET /v1/pay HTTP/1.1" 502 0 "-" "acme-web/2.3"'],
  ["info", '10.244.1.51 - - [DATE] "GET /healthz HTTP/1.1" 200 2 "-" "kube-probe/1.0"'],
];
const SVC_LOG_FMT: Record<string, Fmt> = { "web-gateway": "clf", ingress: "clf", "worker-queue": "json" };
const fmtFor = (svc: string): Fmt => SVC_LOG_FMT[svc] || "logfmt";
const FMT_LABEL: Record<Fmt, string> = { logfmt: "logfmt", json: "JSON lines", clf: "access log (CLF)", plain: "plain text" };
const MON = ["Jan", "Feb", "Mar", "Apr", "May", "Jun", "Jul", "Aug", "Sep", "Oct", "Nov", "Dec"];
const fmtTs = (epoch: number) => new Date(epoch).toISOString().slice(11, 23);
const clfDate = (epoch: number) => {
  const d = new Date(epoch);
  return `${String(d.getUTCDate()).padStart(2, "0")}/${MON[d.getUTCMonth()]}/${d.getUTCFullYear()}:${d.toISOString().slice(11, 19)} +0000`;
};
let seedCtr = 7;
function genLine(fmt: Fmt, epoch: number): { level: string; raw: string; fmt: Fmt } {
  const pool = fmt === "json" ? JSON_T : fmt === "clf" ? CLF_T : LOGFMT_T;
  const [level, tmpl] = pool[(seedCtr++ * 13) % pool.length];
  let raw = tmpl;
  if (fmt === "clf") raw = raw.replace("DATE", clfDate(epoch));
  return { level, raw, fmt };
}
function detectFormat(raw: string): Fmt {
  const s = (raw || "").trim();
  if (s.startsWith("{") && s.endsWith("}")) return "json";
  if (/^\d{1,3}(\.\d{1,3}){3} .*"[A-Z]+ .*" \d{3}/.test(s)) return "clf";
  if (/(^|\s)[a-z_][\w.]*=("[^"]*"|\S+)/.test(s)) return "logfmt";
  return "plain";
}
const statusCls = (code: string) => { const c = +code; return c >= 500 ? "tok-err" : c >= 400 ? "tok-warn" : "tok-ok"; };
function valEl(v: string, k: string, key: string | number) {
  if (k === "status" && /^\d{3}$/.test(v)) return <span key={key} className={statusCls(v)}>{v}</span>;
  if (/^".*"$/.test(v)) return <span key={key} className="tok-str">{v}</span>;
  if (/^[\d.]+(ms|s|m|h|%)?$/.test(v) || /^\d+\/\d+$/.test(v)) return <span key={key} className="tok-num">{v}</span>;
  if (v === "true" || v === "false") return <span key={key} className="tok-num">{v}</span>;
  return <span key={key} className="tok-val">{v}</span>;
}
function PrettyLine({ l }: { l: RawLine }) {
  if (l.fmt === "json") {
    let obj: Record<string, unknown>;
    try { obj = JSON.parse(l.raw); } catch { return <span className="tok-msg">{l.raw}</span>; }
    const ks = Object.keys(obj);
    return (
      <>
        <span className="tok-punc">{"{ "}</span>
        {ks.map((k, i) => {
          const v = obj[k];
          const vEl = typeof v === "number" || typeof v === "boolean" ? <span className="tok-num">{String(v)}</span> : <span className="tok-str">{`"${v}"`}</span>;
          return <Fragment key={i}><span className="tok-key">{`"${k}"`}</span><span className="tok-punc">: </span>{vEl}{i < ks.length - 1 ? <span className="tok-punc">, </span> : null}</Fragment>;
        })}
        <span className="tok-punc">{" }"}</span>
      </>
    );
  }
  if (l.fmt === "clf") {
    const m = l.raw.match(/^(\S+) \S+ \S+ \[([^\]]+)\] "([^"]*)" (\d{3}) (\S+) "([^"]*)" "([^"]*)"/);
    if (!m) return <span className="tok-msg">{l.raw}</span>;
    const rp = m[3].split(" ");
    return (
      <>
        <span className="tok-ip">{m[1]}</span>
        <span className="tok-punc"> [{m[2]}] </span>
        <span className="tok-punc">"</span><span className="tok-meth">{rp[0]}</span><span className="tok-val"> {rp.slice(1).join(" ")}</span><span className="tok-punc">" </span>
        <span className={statusCls(m[4])}>{m[4]}</span>
        <span className="tok-num"> {m[5]}</span>
        <span className="tok-punc"> "{m[7]}"</span>
      </>
    );
  }
  if (l.fmt === "logfmt") {
    return (
      <>
        {l.raw.split(" ").map((tok, i) => {
          const eq = tok.indexOf("=");
          if (eq > 0 && /^[a-zA-Z_][\w.]*$/.test(tok.slice(0, eq)))
            return <Fragment key={i}><span className="tok-key">{tok.slice(0, eq)}</span><span className="tok-punc">=</span>{valEl(tok.slice(eq + 1), tok.slice(0, eq), "v")}<span> </span></Fragment>;
          return <span key={i} className="tok-msg">{tok} </span>;
        })}
      </>
    );
  }
  return <span className="tok-msg">{l.raw}</span>;
}

/* ---------- logs view (demo / simulated) ---------- */
function LogsView({ svc, instId, services, instances }: { svc: string; instId: string | null; services: Service[]; instances: Instance[] }) {
  const fmt = fmtFor(svc);
  const [level, setLevel] = useState("all");
  const [tail, setTail] = useState(true);
  const [pretty, setPretty] = useState(true);
  const ns = services.find((s) => s.name === svc)?.ns;
  const insts = instances.filter((i) => i.svc === svc);
  const pickOrigin = (k?: number) => instId || (insts.length ? insts[(k ?? insts.length) % insts.length].id : svc);

  const wellRef = useRef<HTMLDivElement>(null);
  const oldestRef = useRef(Date.now() - 24 * 1400);
  const pendRef = useRef<number | null>(null);
  const loadingRef = useRef(false);
  const nearBottomRef = useRef(true);
  const [olderCount, setOlderCount] = useState(0);
  const MAX_OLDER = 240;
  const hasMore = olderCount < MAX_OLDER;

  const [lines, setLines] = useState<RawLine[]>(() => {
    const now = Date.now();
    const arr: RawLine[] = [];
    for (let k = 23; k >= 0; k--) { const ep = now - k * 1400; arr.push({ ts: fmtTs(ep), ...genLine(fmt, ep), origin: pickOrigin(23 - k) }); }
    return arr;
  });

  useEffect(() => {
    if (!tail) return;
    const id = setInterval(() => {
      const ep = Date.now();
      setLines((prev) => [...prev, { ts: fmtTs(ep), ...genLine(fmt, ep), origin: pickOrigin() }]);
    }, 1600);
    return () => clearInterval(id);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [tail, svc, instId]);

  useEffect(() => {
    if (tail && nearBottomRef.current && wellRef.current) wellRef.current.scrollTop = wellRef.current.scrollHeight;
  }, [lines, tail]);

  useLayoutEffect(() => {
    if (pendRef.current != null && wellRef.current) {
      wellRef.current.scrollTop += wellRef.current.scrollHeight - pendRef.current;
      pendRef.current = null;
      loadingRef.current = false;
    }
  }, [lines]);

  function loadEarlier() {
    if (loadingRef.current || !hasMore) return;
    loadingRef.current = true;
    pendRef.current = wellRef.current ? wellRef.current.scrollHeight : 0;
    const end = oldestRef.current;
    const batch: RawLine[] = [];
    for (let k = 40; k >= 1; k--) { const ep = end - k * 1400; batch.push({ ts: fmtTs(ep), ...genLine(fmt, ep), origin: pickOrigin(k + olderCount) }); }
    oldestRef.current = end - 40 * 1400;
    setOlderCount((c) => c + 40);
    setLines((prev) => [...batch, ...prev]);
  }

  function onScroll(e: React.UIEvent<HTMLDivElement>) {
    const el = e.currentTarget;
    nearBottomRef.current = el.scrollHeight - el.scrollTop - el.clientHeight < 60;
    if (el.scrollTop < 28) loadEarlier();
  }

  const shown = lines.filter((l) => level === "all" || l.level === level);
  const target = instId || svc;
  const detected = detectFormat(lines.length ? lines[lines.length - 1].raw : "");

  return (
    <div className="fadein">
      <div style={{ display: "flex", gap: 12, marginBottom: 12, alignItems: "center", flexWrap: "wrap" }}>
        <Segmented options={["all", "info", "warn", "error", "debug"]} value={level} onChange={setLevel} />
        <Segmented options={[{ value: "pretty", label: "Pretty" }, { value: "raw", label: "Raw" }]} value={pretty ? "pretty" : "raw"} onChange={(v) => setPretty(v === "pretty")} />
        <span className="fmtbadge" style={{ marginLeft: "auto" }} title="Detected automatically from the stream">
          <Icon name="health" size={12} style={{ color: "var(--text-3)" }} />detected <b>{FMT_LABEL[detected]}</b>
        </span>
        <button className="loadmore" onClick={() => setTail((t) => !t)} style={{ borderColor: tail ? "rgba(48,164,108,.45)" : "var(--border-strong)" }}>
          <span className={`dot ${tail ? "run pulse" : "idle"}`} style={{ boxShadow: "none" }} />
          <span style={{ color: tail ? "#6bd49b" : "var(--text-2)" }}>{tail ? "Tailing" : "Paused"}</span>
        </button>
      </div>
      <div className="cmdline">
        <span className="c-prompt">$</span> rune logs {target} --follow{level !== "all" ? ` --grep=${level}` : ""}{!pretty ? " -o raw" : ""}{olderCount ? ` --since=${Math.round(((olderCount + 24) * 1.4) / 60) + 1}m` : ""}{" "}
        <span style={{ color: "var(--text-4)" }}># {instId ? "single instance" : `aggregated across ${insts.length} instances`}</span>
      </div>
      <div className="logwell" ref={wellRef} onScroll={onScroll} style={{ height: 430, overflowY: "auto" }}>
        <div className="logtop">
          {hasMore ? (
            <button className="loadmore" onClick={loadEarlier} disabled={loadingRef.current}><Icon name="arrowup" size={12} />Load earlier lines</button>
          ) : (
            <span className="logtop-end">— beginning of retained logs —<br />runed streams from the runner; older history isn't retained. Ship to RuneSight for retention.</span>
          )}
        </div>
        {shown.map((l, i) => (
          <div className="logline" key={i}>
            <span className="lt">{l.ts}</span>
            <span className={`lv ${l.level}`}>{l.level.toUpperCase()}</span>
            <span className="lsvc">{l.origin}</span>
            <span className="lm">{pretty ? <PrettyLine l={l} /> : l.raw}</span>
          </div>
        ))}
      </div>
      <div className="logmeta">
        <span>{shown.length} lines{olderCount ? ` · +${olderCount} earlier` : ""}</span>
        <span style={{ color: "var(--text-4)" }}>·</span>
        <span>{instId ? `instance ${instId}` : `${svc}.${ns}.rune.local`}</span>
        <span style={{ marginLeft: "auto", color: tail ? "var(--run)" : "var(--text-3)" }}>{tail ? "● streaming" : "○ paused"}</span>
      </div>
    </div>
  );
}

/* ---------- logs view (live · GetLogs server-stream) ---------- */
function LiveLogsView({ svc, instId, ns }: { svc: string; instId: string | null; ns: string }) {
  const [level, setLevel] = useState("all");
  const [pretty, setPretty] = useState(true);
  const [tail, setTail] = useState(true);
  const [lines, setLines] = useState<RawLine[]>([]);
  const [error, setError] = useState<string | null>(null);
  const [connected, setConnected] = useState(false);
  const wellRef = useRef<HTMLDivElement>(null);
  const nearBottomRef = useRef(true);
  const TAIL_N = 200;

  const target = instId || svc;

  // (Re)open the stream whenever the target / namespace / tail toggle changes.
  // streamLogs returns a stop() that aborts the underlying server-stream, so
  // the connection is torn down cleanly on unmount and on every dependency
  // change (no leaks).
  useEffect(() => {
    setLines([]);
    setError(null);
    setConnected(false);
    const stop = streamLogs({
      target,
      namespace: ns || "default",
      follow: tail,
      tail: TAIL_N,
      onLine: (l: LiveLogLine) => {
        setConnected(true);
        const fmt = detectFormat(l.content);
        setLines((prev) => {
          const next = [...prev, { ts: l.ts, level: l.level, raw: l.content, fmt, origin: l.origin }];
          return next.length > 2000 ? next.slice(next.length - 2000) : next;
        });
      },
      onError: (e) => setError(e.message),
      onEnd: () => setConnected(false),
    });
    return stop;
  }, [target, ns, tail]);

  useEffect(() => {
    if (nearBottomRef.current && wellRef.current) wellRef.current.scrollTop = wellRef.current.scrollHeight;
  }, [lines]);

  function onScroll(e: React.UIEvent<HTMLDivElement>) {
    const el = e.currentTarget;
    nearBottomRef.current = el.scrollHeight - el.scrollTop - el.clientHeight < 60;
  }

  const shown = lines.filter((l) => level === "all" || l.level === level);
  const detected = lines.length ? lines[lines.length - 1].fmt : "plain";

  return (
    <div className="fadein">
      <div style={{ display: "flex", gap: 12, marginBottom: 12, alignItems: "center", flexWrap: "wrap" }}>
        <Segmented options={["all", "info", "warn", "error", "debug"]} value={level} onChange={setLevel} />
        <Segmented options={[{ value: "pretty", label: "Pretty" }, { value: "raw", label: "Raw" }]} value={pretty ? "pretty" : "raw"} onChange={(v) => setPretty(v === "pretty")} />
        <span className="fmtbadge" style={{ marginLeft: "auto" }} title="Detected automatically from the stream">
          <Icon name="health" size={12} style={{ color: "var(--text-3)" }} />detected <b>{FMT_LABEL[detected]}</b>
        </span>
        <button className="loadmore" onClick={() => setTail((t) => !t)} style={{ borderColor: tail ? "rgba(48,164,108,.45)" : "var(--border-strong)" }}>
          <span className={`dot ${tail ? "run pulse" : "idle"}`} style={{ boxShadow: "none" }} />
          <span style={{ color: tail ? "#6bd49b" : "var(--text-2)" }}>{tail ? "Tailing" : "Paused"}</span>
        </button>
      </div>
      <div className="cmdline">
        <span className="c-prompt">$</span> rune logs {target} {tail ? "--follow" : ""}{level !== "all" ? ` --grep=${level}` : ""}{!pretty ? " -o raw" : ""}{" "}
        <span style={{ color: "var(--text-4)" }}># {instId ? "single instance" : "aggregated across the service's instances"}</span>
      </div>
      <div className="logwell" ref={wellRef} onScroll={onScroll} style={{ height: 430, overflowY: "auto" }}>
        {error ? (
          <div className="logtop"><span className="logtop-end" style={{ color: "var(--fail)" }}>stream error: {error}</span></div>
        ) : shown.length === 0 ? (
          <div className="logtop"><span className="logtop-end">{connected ? "— no log lines yet —" : "connecting to log stream…"}</span></div>
        ) : null}
        {shown.map((l, i) => (
          <div className="logline" key={i}>
            <span className="lt">{l.ts}</span>
            <span className={`lv ${l.level}`}>{l.level.toUpperCase()}</span>
            <span className="lsvc">{l.origin}</span>
            <span className="lm">{pretty ? <PrettyLine l={l} /> : l.raw}</span>
          </div>
        ))}
      </div>
      <div className="logmeta">
        <span>{shown.length} lines</span>
        <span style={{ color: "var(--text-4)" }}>·</span>
        <span>{instId ? `instance ${instId}` : `service ${svc}`}</span>
        <span style={{ marginLeft: "auto", color: tail && connected ? "var(--run)" : "var(--text-3)" }}>{tail ? (connected ? "● streaming" : "○ connecting") : "○ paused"}</span>
      </div>
    </div>
  );
}

/* ---------- exec terminal ---------- */
function TermLine({ item }: { item: ExecLine }) {
  if (item.type === "cmd") return <div className="term-line"><span className="term-prompt">{item.prompt}</span> <span className="term-cmd">{item.text}</span></div>;
  const cls = item.type === "sys" ? "term-sys" : item.type === "err" ? "term-err" : "term-out";
  return <div className={`term-line ${cls}`}>{item.text}</div>;
}

function ExecTerminal({ svc, instId, services, instances }: { svc: string; instId: string | null; services: Service[]; instances: Instance[] }) {
  const inst: Instance | undefined = instId
    ? instances.find((i) => i.id === instId)
    : instances.find((i) => i.svc === svc && i.status === "run") || instances.find((i) => i.svc === svc);
  const host = inst ? inst.id : instId || svc + "-x7f2a";
  const ns = services.find((s) => s.name === svc)?.ns;
  const [cwd, setCwd] = useState("/app");
  const [closed, setClosed] = useState(false);
  const [hist, setHist] = useState<ExecLine[]>(() => execBanner(svc, host, inst, instId));
  const [input, setInput] = useState("");
  const [cmdHist, setCmdHist] = useState<string[]>([]);
  const [histIdx, setHistIdx] = useState(-1);
  const wellRef = useRef<HTMLDivElement>(null);
  const inputRef = useRef<HTMLInputElement>(null);

  useEffect(() => { if (wellRef.current) wellRef.current.scrollTop = wellRef.current.scrollHeight; }, [hist]);
  useEffect(() => { if (inputRef.current && !closed) inputRef.current.focus(); }, [closed]);

  function run(cmdRaw: string) {
    const cmd = cmdRaw.trim();
    const prompt = `root@${host}:${cwd}#`;
    setHist((h) => [...h, { type: "cmd", prompt, text: cmdRaw }]);
    if (cmd) { setCmdHist((c) => [...c, cmd]); setHistIdx(-1); }
    if (cmd === "") return;
    if (cmd === "clear") { setHist([]); return; }
    if (cmd === "exit" || cmd === "logout") {
      setHist((h) => [...h, { type: "sys", text: `logout\nConnection to ${host} closed.` }]);
      setClosed(true);
      return;
    }
    const res = execRun(cmd, { svc, host, ns, inst, cwd, setCwd });
    if (res && res.length) setHist((h) => [...h, ...res]);
  }
  function onKey(e: React.KeyboardEvent<HTMLInputElement>) {
    if (e.key === "Enter") { e.preventDefault(); const v = input; setInput(""); run(v); }
    else if (e.key === "ArrowUp") { e.preventDefault(); navHist(-1); }
    else if (e.key === "ArrowDown") { e.preventDefault(); navHist(1); }
  }
  function navHist(dir: number) {
    if (!cmdHist.length) return;
    let idx = histIdx === -1 ? cmdHist.length : histIdx;
    idx = Math.max(0, Math.min(cmdHist.length, idx + dir));
    setHistIdx(idx >= cmdHist.length ? -1 : idx);
    setInput(idx >= cmdHist.length ? "" : cmdHist[idx]);
  }
  const promptStr = `root@${host}:${cwd}#`;

  return (
    <div className="fadein">
      <div className="cmdline"><span className="c-prompt">$</span> rune exec {instId || svc} <span style={{ color: "var(--text-4)" }}># {instId ? "pinned instance" : "attaches to a healthy instance"}</span></div>
      <div className="term" ref={wellRef} onClick={() => inputRef.current?.focus()}>
        {hist.map((h, i) => <TermLine key={i} item={h} />)}
        {!closed ? (
          <div className="term-input-row">
            <span className="term-prompt">{promptStr}</span>
            <input ref={inputRef} className="term-input" value={input} spellCheck={false} autoComplete="off" onChange={(e) => setInput(e.target.value)} onKeyDown={onKey} aria-label="exec command" />
          </div>
        ) : (
          <div className="term-closed">session closed · <span style={{ color: "var(--accent-text)", cursor: "pointer" }} onClick={() => { setClosed(false); setHist(execBanner(svc, host, inst, instId)); setCwd("/app"); }}>reconnect</span></div>
        )}
      </div>
      <div className="logmeta">
        <span>exec · {svc}</span><span style={{ color: "var(--text-4)" }}>·</span>
        <span>instance {host}</span><span style={{ color: "var(--text-4)" }}>·</span>
        <span>{inst ? inst.node : "—"}</span>
        <span style={{ marginLeft: "auto", color: closed ? "var(--text-3)" : "var(--run)" }}>{closed ? "○ disconnected" : "● connected · root"}</span>
      </div>
      <div style={{ marginTop: 11, color: "var(--text-3)", fontSize: 12 }}>
        {["ls /etc/secrets/db", "cat /etc/config/log-level", "env | grep LOG", "nc -zv postgres 5432", "ps aux", "help"].map((c) => (
          <code className="hint" key={c} onClick={() => { setInput(""); run(c); inputRef.current?.focus(); }}>{c}</code>
        ))}
      </div>
    </div>
  );
}

/* ---------- exec terminal (live · WS bridge) ---------- */
interface LiveLine { type: "cmd" | "out" | "sys" | "err"; text: string; prompt?: string }

function LiveExecTerminal({ svc, instId, ns }: { svc: string; instId: string | null; ns: string }) {
  const host = instId || svc;
  const [hist, setHist] = useState<LiveLine[]>([{ type: "sys", text: `Connecting to ${instId ? `instance ${instId}` : `a healthy instance of ${svc}`} …` }]);
  const [input, setInput] = useState("");
  const [cmdHist, setCmdHist] = useState<string[]>([]);
  const [histIdx, setHistIdx] = useState(-1);
  const [connected, setConnected] = useState(false);
  const [closed, setClosed] = useState(false);
  const sessionRef = useRef<ExecSession | null>(null);
  const wellRef = useRef<HTMLDivElement>(null);
  const inputRef = useRef<HTMLInputElement>(null);
  // Accumulate raw stdout and flush it as output lines.
  const bufRef = useRef("");

  function pushBuf(text: string) {
    bufRef.current += text;
    // Normalize CRLF, then split into lines.
    const norm = bufRef.current.replace(/\r\n/g, "\n").replace(/\r/g, "\n");
    const parts = norm.split("\n");
    bufRef.current = parts.pop() ?? "";
    const clean = parts.map((t) => ({ type: "out" as const, text: stripAnsi(t) }));
    if (clean.length) setHist((h) => [...h, ...clean]);
  }

  useEffect(() => {
    let live = true;
    (async () => {
      try {
        const session = await openExecSession({
          service: instId ? undefined : svc,
          instanceId: instId || undefined,
          namespace: ns || "default",
          command: ["/bin/sh"],
          tty: true,
          cols: 100,
          rows: 30,
          onOpen: () => { if (live) { setConnected(true); setHist((h) => [...h, { type: "sys", text: `Connected · ${host}` }]); } },
          onData: (text) => { if (live) pushBuf(text); },
          onExit: (code) => {
            if (!live) return;
            if (bufRef.current) { setHist((h) => [...h, { type: "out", text: bufRef.current }]); bufRef.current = ""; }
            setHist((h) => [...h, { type: "sys", text: `Session closed${code ? ` (exit ${code})` : ""}.` }]);
            setClosed(true);
            setConnected(false);
          },
          onError: (e) => { if (live) setHist((h) => [...h, { type: "err", text: e.message }]); },
        });
        if (!live) { session.close(); return; }
        sessionRef.current = session;
      } catch (e) {
        if (live) setHist((h) => [...h, { type: "err", text: e instanceof Error ? e.message : String(e) }]);
      }
    })();
    return () => {
      live = false;
      sessionRef.current?.close();
      sessionRef.current = null;
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [svc, instId, ns]);

  useEffect(() => { if (wellRef.current) wellRef.current.scrollTop = wellRef.current.scrollHeight; }, [hist]);
  useEffect(() => { if (inputRef.current && !closed) inputRef.current.focus(); }, [closed]);

  function run(raw: string) {
    const prompt = `${host}:~#`;
    setHist((h) => [...h, { type: "cmd", prompt, text: raw }]);
    if (raw.trim()) { setCmdHist((c) => [...c, raw]); setHistIdx(-1); }
    sessionRef.current?.send(raw + "\n");
  }
  function onKey(e: React.KeyboardEvent<HTMLInputElement>) {
    if (e.key === "Enter") { e.preventDefault(); const v = input; setInput(""); run(v); }
    else if (e.key === "ArrowUp") { e.preventDefault(); navHist(-1); }
    else if (e.key === "ArrowDown") { e.preventDefault(); navHist(1); }
    else if (e.key === "c" && e.ctrlKey) { e.preventDefault(); sessionRef.current?.send("\x03"); }
  }
  function navHist(dir: number) {
    if (!cmdHist.length) return;
    let idx = histIdx === -1 ? cmdHist.length : histIdx;
    idx = Math.max(0, Math.min(cmdHist.length, idx + dir));
    setHistIdx(idx >= cmdHist.length ? -1 : idx);
    setInput(idx >= cmdHist.length ? "" : cmdHist[idx]);
  }
  const promptStr = `${host}:~#`;

  return (
    <div className="fadein">
      <div className="cmdline"><span className="c-prompt">$</span> rune exec {instId || svc} <span style={{ color: "var(--text-4)" }}># {instId ? "pinned instance" : "attaches to a healthy instance"} · WS bridge</span></div>
      <div className="term" ref={wellRef} onClick={() => inputRef.current?.focus()}>
        {hist.map((h, i) => <TermLine key={i} item={h as ExecLine} />)}
        {!closed ? (
          <div className="term-input-row">
            <span className="term-prompt">{promptStr}</span>
            <input ref={inputRef} className="term-input" value={input} spellCheck={false} autoComplete="off" disabled={!connected} onChange={(e) => setInput(e.target.value)} onKeyDown={onKey} aria-label="exec command" />
          </div>
        ) : (
          <div className="term-closed">session closed</div>
        )}
      </div>
      <div className="logmeta">
        <span>exec · {svc}</span><span style={{ color: "var(--text-4)" }}>·</span>
        <span>{instId ? `instance ${instId}` : "auto instance"}</span>
        <span style={{ marginLeft: "auto", color: closed ? "var(--text-3)" : connected ? "var(--run)" : "var(--text-3)" }}>{closed ? "○ disconnected" : connected ? "● connected · root" : "○ connecting"}</span>
      </div>
    </div>
  );
}

export function Logs({ initialSvc }: { initialSvc?: string | null }) {
  const demo = useDemo();
  const { data: liveServices } = useServices();
  const { data: liveInstances } = useInstances();
  const services = demo ? RUNE.services : liveServices;
  const instances = demo ? RUNE.instances : liveInstances;

  const svcNames = services.filter((s) => s.ports.length || s.type === "container").map((s) => s.name);
  const fallback = svcNames[0] || services[0]?.name || "";
  const [mode, setMode] = useState("logs");
  const [svc, setSvc] = useState(initialSvc && svcNames.includes(initialSvc) ? initialSvc : fallback);
  const [inst, setInst] = useState<string | null>(null);
  const pickSvc = (n: string) => { setSvc(n); setInst(null); };

  // Once live data arrives, snap the selection to a real service if the
  // current pick isn't present (e.g. the mock default "api-core").
  useEffect(() => {
    if (!demo && services.length && !services.some((s) => s.name === svc)) {
      setSvc(initialSvc && svcNames.includes(initialSvc) ? initialSvc : fallback);
      setInst(null);
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [demo, services.length]);

  const ns = services.find((s) => s.name === svc)?.ns || "default";

  return (
    <div className="wrap" style={{ maxWidth: 1100 }}>
      <PageHead
        eyebrow={mode === "logs" ? "rune logs · live stream" : "rune exec · interactive shell"}
        title="Logs <em>&</em> Exec"
        sub="Logs and exec operate on instances. Target a service to aggregate (logs) or pick a healthy instance (exec), or pin a specific instance."
      />
      <div style={{ display: "flex", gap: 12, marginBottom: 14, alignItems: "center", flexWrap: "wrap" }}>
        <Segmented options={[{ value: "logs", label: "Logs" }, { value: "exec", label: "Exec" }]} value={mode} onChange={setMode} />
        <ServicePicker svc={svc} onPick={pickSvc} services={services} />
        <ScopePicker mode={mode} svc={svc} inst={inst} setInst={setInst} instances={instances} />
      </div>
      {mode === "logs" ? (
        demo
          ? <LogsView svc={svc} instId={inst} services={services} instances={instances} key={"l-" + svc + (inst || "")} />
          : <LiveLogsView svc={svc} instId={inst} ns={ns} key={"ll-" + svc + (inst || "")} />
      ) : (
        demo
          ? <ExecTerminal svc={svc} instId={inst} services={services} instances={instances} key={"e-" + svc + (inst || "")} />
          : <LiveExecTerminal svc={svc} instId={inst} ns={ns} key={"le-" + svc + (inst || "")} />
      )}
    </div>
  );
}
