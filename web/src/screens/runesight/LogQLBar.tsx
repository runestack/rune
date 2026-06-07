import { useEffect, useRef, useState } from "react";
import { Icon } from "../../components";
import { LABEL_KEYS, RANGES, toLogQL, parseLogQL } from "../../api/observe";
import type { LogQuery, Range } from "../../api/observe";

/* ---------- LogQL syntax highlight ---------- */
interface Token { c: string; v: string }

const KEY_RE = /^(level|json|line_format|label_format|regexp|namespace|service|node|instance)$/;

export function tokenizeLogQL(str: string): Token[] {
  const out: Token[] = [];
  const re = /("(?:[^"\\]|\\.)*"?)|(=~|!~|\|=|\|~|!=|=)|([{}(),])|(\|)|([A-Za-z_][\w-]*)|(\s+)|(.)/g;
  let m: RegExpExecArray | null;
  while ((m = re.exec(str))) {
    if (m[1]) out.push({ c: "str", v: m[1] });
    else if (m[2]) out.push({ c: "op", v: m[2] });
    else if (m[3]) out.push({ c: "punc", v: m[3] });
    else if (m[4]) out.push({ c: "pipe", v: m[4] });
    else if (m[5]) out.push({ c: KEY_RE.test(m[5]) ? "key" : "ident", v: m[5] });
    else out.push({ c: "ws", v: m[6] || m[7] });
  }
  return out;
}

export function Highlighted({ text }: { text: string }) {
  return <>{tokenizeLogQL(text).map((tk, i) => <span key={i} className={"hl-" + tk.c}>{tk.v}</span>)}</>;
}

/* ---------- autocomplete ---------- */
const PIPE_STAGES = [
  { ins: ' |= ""', label: '|= "…"', hint: "line contains" },
  { ins: ' |~ ""', label: '|~ "…"', hint: "line matches regex" },
  { ins: ' != ""', label: '!= "…"', hint: "line excludes" },
  { ins: ' | level=~"error"', label: "| level=~", hint: "level filter" },
  { ins: " | json", label: "| json", hint: "parse JSON fields" },
];

interface Suggest {
  kind: "key" | "value" | "stage";
  start: number;
  items: { ins: string; label: string; hint: string }[];
}

function suggestFor(value: string, caret: number, labelValues: Record<string, string[]>): Suggest | null {
  const before = value.slice(0, caret);
  const braceOpen = before.lastIndexOf("{");
  const braceClose = before.lastIndexOf("}");
  if (braceOpen > braceClose) {
    const seg = before.slice(Math.max(before.lastIndexOf("{"), before.lastIndexOf(",")) + 1);
    const eq = seg.match(/(\w+)\s*=~?\s*"([^"]*)$/);
    if (eq) {
      const key = eq[1];
      const partial = (eq[2].split("|").pop() ?? "");
      const vals = (labelValues[key] || []).filter((v) => v.toLowerCase().startsWith(partial.toLowerCase()));
      return { kind: "value", start: caret - partial.length, items: vals.slice(0, 8).map((v) => ({ ins: v, label: v, hint: key })) };
    }
    const keyPartial = (seg.match(/([a-z]*)$/i)?.[1]) ?? "";
    const keys = LABEL_KEYS.filter((k) => k.startsWith(keyPartial.toLowerCase()) && !before.includes(k + "="));
    if (!keys.length) return null;
    return { kind: "key", start: caret - keyPartial.length, items: keys.map((k) => ({ ins: k + '="', label: k, hint: "label" })) };
  }
  if (braceClose > braceOpen && braceClose >= 0) {
    return { kind: "stage", start: caret, items: PIPE_STAGES };
  }
  return null;
}

export interface LogQLBarProps {
  query: LogQuery;
  range: Range;
  live: boolean;
  running: boolean;
  labelValues: Record<string, string[]>;
  setRange: (r: Range) => void;
  setLive: (fn: (l: boolean) => boolean) => void;
  run: (q: LogQuery) => void;
}

export function LogQLBar({ query, range, live, running, labelValues, setRange, setLive, run }: LogQLBarProps) {
  const [text, setText] = useState(() => toLogQL(query));
  const [sug, setSug] = useState<Suggest | null>(null);
  const [si, setSi] = useState(0);
  const [rangeOpen, setRangeOpen] = useState(false);
  const inputRef = useRef<HTMLInputElement>(null);
  const hlRef = useRef<HTMLDivElement>(null);
  const rangeRef = useRef<HTMLDivElement>(null);

  useEffect(() => { setText(toLogQL(query)); }, [query]);
  useEffect(() => {
    const h = (e: MouseEvent) => { if (rangeRef.current && !rangeRef.current.contains(e.target as Node)) setRangeOpen(false); };
    document.addEventListener("mousedown", h);
    return () => document.removeEventListener("mousedown", h);
  }, []);

  function recompute(el: HTMLInputElement) {
    const s = suggestFor(el.value, el.selectionStart ?? el.value.length, labelValues);
    setSug(s && s.items.length ? s : null);
    setSi(0);
  }
  function accept(item: Suggest["items"][number], s: Suggest) {
    const el = inputRef.current;
    if (!el) return;
    const next = text.slice(0, s.start) + item.ins + text.slice(el.selectionStart ?? text.length);
    setText(next);
    setSug(null);
    requestAnimationFrame(() => {
      el.focus();
      const pos = s.start + item.ins.length;
      el.setSelectionRange(pos, pos);
      recompute(el);
    });
  }
  function onKey(e: React.KeyboardEvent<HTMLInputElement>) {
    if (sug) {
      if (e.key === "ArrowDown") { e.preventDefault(); setSi((i) => (i + 1) % sug.items.length); return; }
      if (e.key === "ArrowUp") { e.preventDefault(); setSi((i) => (i - 1 + sug.items.length) % sug.items.length); return; }
      if (e.key === "Tab") { e.preventDefault(); accept(sug.items[si], sug); return; }
      if (e.key === "Escape") { e.preventDefault(); setSug(null); return; }
    }
    if (e.key === "Enter") { e.preventDefault(); setSug(null); run(parseLogQL(text)); }
  }

  return (
    <div className="rs-logql-row">
      <div className="rs-logql-bar">
        <span className="rs-logql-tag">LOGQL</span>
        <div className="rs-logql-input-wrap">
          <div className="rs-logql-hl mono" ref={hlRef} aria-hidden="true"><Highlighted text={text} /></div>
          <input
            ref={inputRef}
            className="rs-logql-input mono"
            value={text}
            spellCheck={false}
            autoComplete="off"
            placeholder={'{ service="api-core" } |= "error"'}
            onChange={(e) => { setText(e.target.value); recompute(e.target); if (hlRef.current) hlRef.current.scrollLeft = e.target.scrollLeft; }}
            onKeyDown={onKey}
            onClick={(e) => recompute(e.currentTarget)}
            onScroll={(e) => { if (hlRef.current) hlRef.current.scrollLeft = e.currentTarget.scrollLeft; }}
            onBlur={() => setTimeout(() => setSug(null), 120)}
          />
          {sug && (
            <div className="rs-ac">
              <div className="rs-ac-head">{sug.kind === "key" ? "Label" : sug.kind === "value" ? "Value" : "Pipeline stage"}</div>
              {sug.items.map((it, i) => (
                <div key={i} className={"rs-ac-item" + (i === si ? " on" : "")}
                  onMouseDown={(e) => { e.preventDefault(); accept(it, sug); }} onMouseEnter={() => setSi(i)}>
                  <span className="rs-ac-label mono">{it.label}</span>
                  <span className="rs-ac-hint">{it.hint}</span>
                </div>
              ))}
              <div className="rs-ac-foot"><kbd>Tab</kbd> insert · <kbd>↵</kbd> run</div>
            </div>
          )}
        </div>
      </div>

      <div className="rs-range-dd" ref={rangeRef}>
        <button className="rs-range-btn" onClick={() => setRangeOpen((o) => !o)}>
          <Icon name="clock" size={14} /><span>Last {range}</span><Icon name="chevrond" size={13} />
        </button>
        {rangeOpen && (
          <div className="rs-range-menu">
            {RANGES.map((r) => (
              <div key={r} className={"rs-range-opt" + (range === r ? " on" : "")} onClick={() => { setRange(r); setRangeOpen(false); }}>
                <span className="rs-range-lbl">Last {r}</span>
                <span className="rs-range-ck">{range === r ? <Icon name="check" size={13} /> : null}</span>
              </div>
            ))}
          </div>
        )}
      </div>

      <button className={"rs-auto" + (live ? " on" : "")} onClick={() => setLive((l) => !l)}>
        <span className="rs-auto-box">{live ? <Icon name="check" size={12} /> : null}</span>Auto · 5s
      </button>
      <button className="rs-run primary" disabled={running} onClick={() => run(parseLogQL(text))}>
        <Icon name="bolt" size={13} />{running ? "Running…" : "Run query"}
      </button>
    </div>
  );
}
