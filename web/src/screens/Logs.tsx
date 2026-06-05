import { Fragment, useEffect, useRef, useState } from "react";
import { Dot, Dropdown, Icon, Segmented, Tooltip } from "../components";
import type { Instance, Service } from "../api/types";
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
// Screen-clear sequences emitted by `clear` / Ctrl-L. Shells differ: ncurses
// sends ESC[2J / ESC[3J (erase screen / scrollback), while xterm/busybox `clear`
// sends a cursor-home (ESC[H, ESC[1;1H, …) immediately followed by an
// erase-display (ESC[J / ESC[0-2J). We deliberately do NOT match a bare ESC[J,
// which line editors emit on every prompt redraw — only the unambiguous
// full-screen forms.
const ANSI_CLEAR = new RegExp(ESC + "\\[[23]J|" + ESC + "\\[(?:\\d*;\\d*)?H" + ESC + "\\[[0-2]?J", "g");
// A shell PS1 prompt at the start of an echoed line, e.g. "/etc # ls", "/ # ",
// "~ $ cmd". Split the prompt prefix off the command so the prompt keeps its
// accent color in scrollback (rendered as a `cmd` line) instead of going grey
// like ordinary output. Anchored to an absolute path or "~" so real output
// lines (`root`, `alpine-release`, …) don't match.
const PROMPT_RE = new RegExp("^((?:/|~)\\S* [#$]) ?(.*)$");
function stripAnsi(s: string): string {
  return s.replace(ANSI_CSI, "").replace(ANSI_OSC, "").replace(ANSI_ESC, "").replace(C0, "");
}

/* ---------- log formats ---------- */
type Fmt = "logfmt" | "json" | "clf" | "plain";
interface RawLine { ts: string; level: string; raw: string; fmt: Fmt; origin: string }

const FMT_LABEL: Record<Fmt, string> = { logfmt: "logfmt", json: "JSON lines", clf: "access log (CLF)", plain: "plain text" };
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

/* ---------- logs view (live · GetLogs server-stream) ---------- */
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
    <div className="fadein logsview">
      <div style={{ display: "flex", gap: 12, marginBottom: 12, alignItems: "center", flexWrap: "wrap" }}>
        <Segmented options={["all", "info", "warn", "error", "debug"]} value={level} onChange={setLevel} />
        <Segmented options={[{ value: "pretty", label: "Pretty" }, { value: "raw", label: "Raw" }]} value={pretty ? "pretty" : "raw"} onChange={(v) => setPretty(v === "pretty")} />
        <span style={{ marginLeft: "auto" }}>
          <Tooltip label="Detected automatically from the stream">
            <span className="fmtbadge">
              <Icon name="health" size={12} style={{ color: "var(--text-3)" }} />detected <b>{FMT_LABEL[detected]}</b>
            </span>
          </Tooltip>
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
      <div className="logwell grow" ref={wellRef} onScroll={onScroll} style={{ overflowY: "auto" }}>
        {error ? (
          <div className="logtop"><span className="logtop-end" style={{ color: "var(--fail)" }}>stream error: {error}</span></div>
        ) : shown.length === 0 ? (
          <div className="logtop"><span className="logtop-end">{connected ? "— no log lines yet —" : "connecting to log stream…"}</span></div>
        ) : null}
        {shown.map((l, i) =>
          pretty ? (
            <div className="logline" key={i}>
              <span className="lt">{l.ts}</span>
              <span className={`lv ${l.level}`}>{l.level.toUpperCase()}</span>
              <span className="lsvc">{l.origin}</span>
              <span className="lm"><PrettyLine l={l} /></span>
            </div>
          ) : (
            // Raw drops the timestamp/level/origin prefix columns — just the
            // unaltered log line, the way `rune logs … -o raw` prints it.
            <div className="logline raw" key={i}>
              <span className="lm">{l.raw}</span>
            </div>
          ),
        )}
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

/* ---------- exec terminal (live · WS bridge) ---------- */
interface LiveLine { type: "cmd" | "out" | "sys" | "err"; text: string; prompt?: string }

function TermLine({ item }: { item: LiveLine }) {
  if (item.type === "cmd") return <div className="term-line"><span className="term-prompt">{item.prompt}</span> <span className="term-cmd">{item.text}</span></div>;
  const cls = item.type === "sys" ? "term-sys" : item.type === "err" ? "term-err" : "term-out";
  return <div className={`term-line ${cls}`}>{item.text}</div>;
}

function LiveExecTerminal({ svc, instId, ns }: { svc: string; instId: string | null; ns: string }) {
  const host = instId || svc;
  const [hist, setHist] = useState<LiveLine[]>([{ type: "sys", text: `Connecting to ${instId ? `instance ${instId}` : `a healthy instance of ${svc}`} …` }]);
  const [input, setInput] = useState("");
  const [cmdHist, setCmdHist] = useState<string[]>([]);
  const [histIdx, setHistIdx] = useState(-1);
  const [connected, setConnected] = useState(false);
  const [closed, setClosed] = useState(false);
  // The shell's live prompt: the trailing, not-yet-newlined bytes the PTY has
  // emitted (e.g. "/var # "). We render this on the input row instead of a fake
  // static prompt so it tracks the real cwd the way a terminal does.
  const [pending, setPending] = useState("");
  const sessionRef = useRef<ExecSession | null>(null);
  const wellRef = useRef<HTMLDivElement>(null);
  const inputRef = useRef<HTMLInputElement>(null);
  // Accumulate raw stdout, flush completed lines, and keep the partial tail as
  // the live prompt.
  const bufRef = useRef("");

  function pushBuf(text: string) {
    let buf = bufRef.current + text;
    // Honor screen-clear escapes (`clear`, Ctrl-L). Find the last clear in the
    // buffer, wipe the rendered scrollback, and keep only what the shell drew
    // afterwards (a fresh prompt). Without this the codes are stripped and
    // `clear` is a no-op. See ANSI_CLEAR for the sequences matched.
    let ci = -1;
    ANSI_CLEAR.lastIndex = 0;
    for (let m: RegExpExecArray | null; (m = ANSI_CLEAR.exec(buf)); ) ci = m.index;
    if (ci !== -1) { setHist([]); buf = buf.slice(ci); }
    // Normalize CRLF, then split into lines.
    const norm = buf.replace(/\r\n/g, "\n").replace(/\r/g, "\n");
    const parts = norm.split("\n");
    bufRef.current = parts.pop() ?? "";
    const clean: LiveLine[] = [];
    for (const t of parts) {
      const s = stripAnsi(t);
      const m = PROMPT_RE.exec(s);
      if (m) {
        // A prompt with no command is the shell redrawing its prompt (cursor
        // query timeout on connect, empty Enter, …). The live prompt already
        // lives on the input row, so a bare prompt in scrollback is just noise —
        // drop it. Keep prompts that carry an actual command.
        if (m[2] === "") continue;
        clean.push({ type: "cmd", prompt: m[1], text: m[2] }); // accent prompt + plain command
      } else {
        clean.push({ type: "out", text: s });
      }
    }
    if (clean.length) setHist((h) => [...h, ...clean]);
    // Surface the partial tail as the prompt. Only update when non-empty so the
    // prompt stays stable in the brief gap between echoing a command and the
    // shell redrawing the next prompt (avoids a flicker to blank).
    const tail = stripAnsi(bufRef.current);
    if (tail) setPending(tail);
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
            // The tail is the shell's dangling prompt, not output — drop it.
            bufRef.current = "";
            setPending("");
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
    // Don't echo locally: the PTY echoes input and renders its own prompt, so a
    // synthetic line here would duplicate every command. Just record it for
    // ↑/↓ history and hand it to the shell.
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
  return (
    <div className="fadein execview">
      <div className="cmdline"><span className="c-prompt">$</span> rune exec {instId || svc} <span style={{ color: "var(--text-4)" }}># {instId ? "pinned instance" : "attaches to a healthy instance"} · WS bridge</span></div>
      <div className="term grow" ref={wellRef} onClick={() => inputRef.current?.focus()}>
        {hist.map((h, i) => <TermLine key={i} item={h} />)}
        {!closed ? (
          <div className="term-input-row">
            <span className="term-prompt">{pending}</span>
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
  const { data: services } = useServices();
  const { data: instances } = useInstances();

  const svcNames = services.filter((s) => s.ports.length || s.type === "container").map((s) => s.name);
  const fallback = svcNames[0] || services[0]?.name || "";
  const [mode, setMode] = useState("logs");
  const [svc, setSvc] = useState(initialSvc && svcNames.includes(initialSvc) ? initialSvc : fallback);
  const [inst, setInst] = useState<string | null>(null);
  const pickSvc = (n: string) => { setSvc(n); setInst(null); };

  // Once live data arrives, snap the selection to a real service if the
  // current pick isn't present yet.
  useEffect(() => {
    if (services.length && !services.some((s) => s.name === svc)) {
      setSvc(initialSvc && svcNames.includes(initialSvc) ? initialSvc : fallback);
      setInst(null);
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [services.length]);

  const ns = services.find((s) => s.name === svc)?.ns || "default";

  return (
    <div className="logs-screen">
      <div className="logs-head">
        <div className="lh-id">
          <div className="eyebrow">{mode === "logs" ? "rune logs · live stream" : "rune exec · interactive shell"}</div>
          <h1 className="lh-title">Logs <em>&amp;</em> Exec</h1>
        </div>
        <Segmented options={[{ value: "logs", label: "Logs" }, { value: "exec", label: "Exec" }]} value={mode} onChange={setMode} />
        <ServicePicker svc={svc} onPick={pickSvc} services={services} />
        <ScopePicker mode={mode} svc={svc} inst={inst} setInst={setInst} instances={instances} />
      </div>
      {mode === "logs" ? (
        <LiveLogsView svc={svc} instId={inst} ns={ns} key={"ll-" + svc + (inst || "")} />
      ) : (
        <LiveExecTerminal svc={svc} instId={inst} ns={ns} key={"le-" + svc + (inst || "")} />
      )}
    </div>
  );
}
