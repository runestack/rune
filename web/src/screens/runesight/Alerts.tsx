import { useCallback, useEffect, useRef, useState } from "react";
import { Button, Card, CardHead, Icon, PageHead, Select, Table, Tag } from "../../components";
import { parseLogQL } from "../../api/observe";
import type { LogQuery } from "../../api/observe";
import {
  deleteAlertRule, deleteChannel, listAlertRules, listChannels, saveAlertRule, saveChannel,
} from "../../api/alerting";
import type { AlertRuleData, AlertStatusData, ChannelData } from "../../api/alerting";
import { timeAgo, viewSlug } from "../../api/savedViews";

const POLL_MS = 30_000;
const WINDOWS = ["1m", "5m", "15m", "1h"] as const;
const OPS = [">", ">=", "<", "<=", "=="] as const;
const FORS = ["0s", "1m", "2m", "5m"] as const;

/* "type:name" chip; plain name when the channel's type is unknown. */
function renderChan(chan: string) {
  const i = chan.indexOf(":");
  if (i < 0) return <span className="rs-chan"><span className="rs-chan-v">{chan}</span></span>;
  const kind = chan.slice(0, i), v = chan.slice(i + 1);
  return (
    <span className="rs-chan">
      <span className={"rs-chan-k " + kind}>{kind}</span>
      <span className="rs-chan-c">:</span>
      <span className="rs-chan-v">{v}</span>
    </span>
  );
}

function fmtValue(v: number): string {
  return Number.isInteger(v) ? String(v) : v.toFixed(1);
}

function urlHost(url: string): string {
  try { return new URL(url).host || url; } catch { return url; }
}

function errMsg(e: unknown): string {
  return e instanceof Error ? e.message : String(e);
}

/* ---------- new-rule modal ---------- */
function RuleModal({ channels, onClose, onSaved }: {
  channels: ChannelData[];
  onClose: () => void;
  onSaved: () => void;
}) {
  const [name, setName] = useState("");
  const [desc, setDesc] = useState("");
  const [logql, setLogql] = useState('{service=""}');
  const [window_, setWindow] = useState<string>("5m");
  const [op, setOp] = useState<string>(">");
  const [threshold, setThreshold] = useState("10");
  const [for_, setFor] = useState<string>("0s");
  const [chans, setChans] = useState<Set<string>>(new Set());
  const [err, setErr] = useState<string | null>(null);
  const [saving, setSaving] = useState(false);
  const inputRef = useRef<HTMLInputElement>(null);
  useEffect(() => { const id = setTimeout(() => inputRef.current?.focus(), 30); return () => clearTimeout(id); }, []);

  const toggleChan = (n: string) => setChans((s) => { const x = new Set(s); x.has(n) ? x.delete(n) : x.add(n); return x; });
  const valid = name.trim() !== "" && logql.trim() !== "" && threshold.trim() !== "" && !Number.isNaN(Number(threshold));

  function submit() {
    if (!valid || saving) return;
    setSaving(true);
    setErr(null);
    saveAlertRule({
      name: viewSlug(name),
      description: desc.trim(),
      logql: logql.trim(),
      window: window_,
      op,
      threshold: Number(threshold),
      for: for_ === "0s" ? "" : for_,
      channels: [...chans],
    })
      .then(() => { onSaved(); onClose(); })
      .catch((e: unknown) => { setErr(errMsg(e)); setSaving(false); });
  }

  return (
    <div className="rs-modal-scrim" onMouseDown={onClose}>
      <div className="rs-modal" onMouseDown={(e) => e.stopPropagation()}>
        <div className="rs-modal-head"><div className="eyebrow">New alert rule</div>
          <button className="rs-modal-x" onClick={onClose}><Icon name="close" size={16} /></button>
        </div>
        <div className="rs-modal-body">
          <label className="rs-field"><span className="rs-field-l">Name</span>
            <input ref={inputRef} className="rs-field-i" value={name} placeholder="e.g. payment-error-rate"
              onChange={(e) => setName(e.target.value)} onKeyDown={(e) => { if (e.key === "Escape") onClose(); }} /></label>
          <label className="rs-field"><span className="rs-field-l">Description <em>optional</em></span>
            <input className="rs-field-i" value={desc} placeholder="What this rule watches for"
              onChange={(e) => setDesc(e.target.value)} /></label>
          <label className="rs-field"><span className="rs-field-l">Query (LogQL)</span>
            <textarea className="rs-field-i mono" rows={2} value={logql} spellCheck={false}
              onChange={(e) => setLogql(e.target.value)} />
            <span className="rs-field-hint">Plain log selector — the alerter counts matching lines over the window.</span></label>
          <div style={{ display: "grid", gridTemplateColumns: "1fr 1fr", gap: 12 }}>
            <label className="rs-field"><span className="rs-field-l">Window</span>
              <Select value={window_} onChange={(e) => setWindow(e.target.value)}>
                {WINDOWS.map((w) => <option key={w} value={w}>{w}</option>)}
              </Select></label>
            <label className="rs-field"><span className="rs-field-l">Condition</span>
              <Select value={op} onChange={(e) => setOp(e.target.value)}>
                {OPS.map((o) => <option key={o} value={o}>count {o} threshold</option>)}
              </Select></label>
            <label className="rs-field"><span className="rs-field-l">Threshold</span>
              <input className="rs-field-i tnum" type="number" value={threshold}
                onChange={(e) => setThreshold(e.target.value)} /></label>
            <label className="rs-field"><span className="rs-field-l">For <em>hysteresis</em></span>
              <Select value={for_} onChange={(e) => setFor(e.target.value)}>
                {FORS.map((f) => <option key={f} value={f}>{f === "0s" ? "0s — fire immediately" : f}</option>)}
              </Select></label>
          </div>
          <div className="rs-field"><span className="rs-field-l">Channels <em>optional</em></span>
            {channels.length === 0
              ? <span className="rs-field-hint">No channels yet — the alert will still emit Rune events.</span>
              : channels.map((c) => (
                <label key={c.name} className="rs-pinrow" onClick={(e) => { e.preventDefault(); toggleChan(c.name); }}>
                  <span className={"rs-pinbox" + (chans.has(c.name) ? " on" : "")}>{chans.has(c.name) ? <Icon name="check" size={12} /> : null}</span>
                  {renderChan(`${c.type}:${c.name}`)}
                </label>
              ))}
          </div>
          {err && <div className="rs-field-err">{err}</div>}
        </div>
        <div className="rs-modal-foot">
          <button className="rs-paneltgl" style={{ height: 34, padding: "0 13px" }} onClick={onClose}>Cancel</button>
          <button className="rs-run primary" disabled={!valid || saving} onClick={submit}><Icon name="check" size={14} />Save rule</button>
        </div>
      </div>
    </div>
  );
}

/* ---------- new-channel modal ---------- */
interface HeaderRow { k: string; v: string }

function ChannelModal({ onClose, onSaved }: { onClose: () => void; onSaved: () => void }) {
  const [name, setName] = useState("");
  const [type, setType] = useState("webhook");
  const [url, setUrl] = useState("");
  const [headers, setHeaders] = useState<HeaderRow[]>([{ k: "", v: "" }, { k: "", v: "" }]);
  const [body, setBody] = useState("");
  const [err, setErr] = useState<string | null>(null);
  const [saving, setSaving] = useState(false);
  const inputRef = useRef<HTMLInputElement>(null);
  useEffect(() => { const id = setTimeout(() => inputRef.current?.focus(), 30); return () => clearTimeout(id); }, []);

  const setHeader = (i: number, key: "k" | "v", val: string) =>
    setHeaders((hs) => hs.map((h, j) => (j === i ? { ...h, [key]: val } : h)));
  const valid = name.trim() !== "" && url.trim() !== "";

  function submit() {
    if (!valid || saving) return;
    setSaving(true);
    setErr(null);
    const hdrs: Record<string, string> = {};
    for (const { k, v } of headers) if (k.trim()) hdrs[k.trim()] = v;
    saveChannel({ name: viewSlug(name), type, url: url.trim(), headers: hdrs, body })
      .then(() => { onSaved(); onClose(); })
      .catch((e: unknown) => { setErr(errMsg(e)); setSaving(false); });
  }

  return (
    <div className="rs-modal-scrim" onMouseDown={onClose}>
      <div className="rs-modal" onMouseDown={(e) => e.stopPropagation()}>
        <div className="rs-modal-head"><div className="eyebrow">New channel</div>
          <button className="rs-modal-x" onClick={onClose}><Icon name="close" size={16} /></button>
        </div>
        <div className="rs-modal-body">
          <label className="rs-field"><span className="rs-field-l">Name</span>
            <input ref={inputRef} className="rs-field-i" value={name} placeholder="e.g. pagerduty"
              onChange={(e) => setName(e.target.value)} onKeyDown={(e) => { if (e.key === "Escape") onClose(); }} /></label>
          <div style={{ display: "grid", gridTemplateColumns: "140px 1fr", gap: 12 }}>
            <label className="rs-field"><span className="rs-field-l">Type</span>
              <Select value={type} onChange={(e) => setType(e.target.value)}>
                <option value="webhook">webhook</option>
                <option value="slack">slack</option>
              </Select></label>
            <label className="rs-field"><span className="rs-field-l">URL</span>
              <input className="rs-field-i mono" value={url} placeholder="https://hooks.example.com/…"
                onChange={(e) => setUrl(e.target.value)} /></label>
          </div>
          <div className="rs-field"><span className="rs-field-l">Headers <em>optional</em></span>
            {headers.map((h, i) => (
              <div key={i} style={{ display: "grid", gridTemplateColumns: "1fr 1.4fr", gap: 8 }}>
                <input className="rs-field-i mono" value={h.k} placeholder="Authorization"
                  onChange={(e) => setHeader(i, "k", e.target.value)} />
                <input className="rs-field-i mono" value={h.v} placeholder="Bearer …"
                  onChange={(e) => setHeader(i, "v", e.target.value)} />
              </div>
            ))}
            <span className="rs-field-hint">Values may reference secrets: {"${secret:ns/name/key}"}.</span>
            <button className="rs-paneltgl" style={{ alignSelf: "flex-start" }}
              onClick={() => setHeaders((hs) => [...hs, { k: "", v: "" }])}><Icon name="plus" size={12} />Add header</button>
          </div>
          <label className="rs-field"><span className="rs-field-l">Body template <em>optional</em></span>
            <textarea className="rs-field-i mono" rows={3} value={body} spellCheck={false}
              placeholder={'Go template — e.g. {"text":"{{.Rule}} is {{.State}} ({{.Value}})"}. Empty = default payload.'}
              onChange={(e) => setBody(e.target.value)} /></label>
          {err && <div className="rs-field-err">{err}</div>}
        </div>
        <div className="rs-modal-foot">
          <button className="rs-paneltgl" style={{ height: 34, padding: "0 13px" }} onClick={onClose}>Cancel</button>
          <button className="rs-run primary" disabled={!valid || saving} onClick={submit}><Icon name="check" size={14} />Save channel</button>
        </div>
      </div>
    </div>
  );
}

/* ---------- alerts screen ---------- */
export function Alerts({ loadView }: { loadView: (q: Partial<LogQuery>) => void }) {
  const [rules, setRules] = useState<AlertRuleData[] | null>(null);
  const [statuses, setStatuses] = useState<Record<string, AlertStatusData>>({});
  const [channels, setChannels] = useState<ChannelData[] | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [ruleModal, setRuleModal] = useState(false);
  const [chanModal, setChanModal] = useState(false);
  const [confirmRule, setConfirmRule] = useState<string | null>(null);
  const [confirmChan, setConfirmChan] = useState<string | null>(null);

  const refresh = useCallback(() => {
    Promise.all([listAlertRules(), listChannels()])
      .then(([{ rules: rs, statuses: sts }, chs]) => {
        setRules(rs);
        setStatuses(Object.fromEntries(sts.map((s) => [s.rule, s])));
        setChannels(chs);
        setError(null);
      })
      .catch((e: unknown) => setError(errMsg(e)));
  }, []);
  useEffect(() => {
    refresh();
    const id = setInterval(refresh, POLL_MS); // statuses go stale fast — poll
    return () => clearInterval(id);
  }, [refresh]);

  const chanByName = new Map((channels ?? []).map((c) => [c.name, c]));
  const ruleState = (r: AlertRuleData): string =>
    r.disabled ? "paused" : (statuses[r.name]?.state ?? "ok");
  const firing = (rules ?? []).filter((r) => ruleState(r) === "firing");
  const pending = (rules ?? []).filter((r) => ruleState(r) === "pending").length;
  const ok = (rules ?? []).filter((r) => ruleState(r) === "ok").length;

  function open(r: AlertRuleData) {
    try { loadView(parseLogQL(r.logql)); } catch { /* unparseable logql — stay put */ }
  }
  function toggleRule(r: AlertRuleData) {
    saveAlertRule({ ...r, disabled: !r.disabled })
      .then(refresh)
      .catch((e: unknown) => setError(errMsg(e)));
  }
  function removeRule(name: string) {
    deleteAlertRule(name)
      .then(refresh)
      .catch((e: unknown) => setError(errMsg(e)));
    setConfirmRule(null);
  }
  function removeChannel(name: string) {
    deleteChannel(name)
      .then(refresh)
      .catch((e: unknown) => setError(errMsg(e)));
    setConfirmChan(null);
  }

  const loading = rules === null && !error;

  return (
    <div className="wrap">
      <PageHead
        eyebrow="rune sight · log-based alerting"
        title="Alert <em>Rules</em>"
        sub="Rules evaluate a LogQL query on a rolling window and fire when the condition is met."
        actions={<Button size="sm" variant="primary" onClick={() => setRuleModal(true)}><Icon name="plus" size={14} />New rule</Button>}
      />

      {error && <div className="rs-empty-s" style={{ color: "var(--fail)", marginBottom: 14 }}>{error}</div>}

      {firing.length > 0 && (
        <div className="rs-firing-banner">
          <Icon name="alert" size={18} />
          <span><b>{firing.length} alert{firing.length === 1 ? "" : "s"} firing</b> — {firing.map((r) => r.name).join(", ")}.</span>
        </div>
      )}

      {!loading && (
      <Card style={{ overflow: "hidden" }}>
        <CardHead
          actions={
            <div style={{ display: "flex", alignItems: "center", gap: 16, marginLeft: "auto" }}>
              <div className="rs-rule-sum">
                {firing.length > 0 && <span className="rs-rs firing"><i />{firing.length} firing</span>}
                {pending > 0 && <span className="rs-rs pending"><i />{pending} pending</span>}
                <span className="rs-rs ok"><i />{ok} healthy</span>
              </div>
              <span className="rs-eval-note">evaluated every 60s</span>
            </div>
          }
        >
          Rules
        </CardHead>
        {(rules ?? []).length === 0 ? (
          <div className="rs-empty">
            <Icon name="alert" size={26} />
            <div className="rs-empty-t">No alert rules yet</div>
            <div className="rs-empty-s">Define a LogQL condition and get paged when it trips.</div>
            <Button size="sm" variant="primary" onClick={() => setRuleModal(true)}><Icon name="plus" size={14} />New rule</Button>
          </div>
        ) : (
        <Table>
          <thead><tr><th>Rule</th><th>Query (LogQL)</th><th>Condition</th><th>Value</th><th>Channels</th><th /></tr></thead>
          <tbody>
            {(rules ?? []).map((r) => {
              const st = statuses[r.name];
              const state = ruleState(r);
              return (
                <tr key={r.id || r.name} onClick={() => open(r)}>
                  <td>
                    <div className="rs-rule-cell">
                      <span className={"rs-alert-dot " + state} />
                      <div className="rs-rule-id">
                        <span className="rs-rule-name">{r.name}</span>
                        {r.disabled && <span className="rs-since-chip">disabled</span>}
                        {!r.disabled && state === "firing" && st?.since && <span className="rs-since-chip">since {timeAgo(st.since)}</span>}
                        {st?.lastError && <span className="rs-rule-err" title={st.lastError}>{st.lastError}</span>}
                      </div>
                    </div>
                  </td>
                  <td className="rs-alert-q">{r.logql}</td>
                  <td className="cell-sub tnum">{r.op} {fmtValue(r.threshold)} / {r.window}{r.for ? ` · for ${r.for}` : ""}</td>
                  <td className="cell-sub tnum">{st ? `${fmtValue(st.value)} / ${r.window}` : "—"}</td>
                  <td>
                    <div className="rs-chans-cell">
                      {r.channels.length === 0
                        ? <span className="cell-sub">events only</span>
                        : r.channels.map((n) => {
                          const c = chanByName.get(n);
                          return <span key={n}>{renderChan(c ? `${c.type}:${n}` : n)}</span>;
                        })}
                    </div>
                  </td>
                  <td onClick={(e) => e.stopPropagation()}>
                    {confirmRule === r.name ? (
                      <div className="rs-saved-confirm" style={{ marginTop: 0, justifyContent: "flex-end" }}>
                        <span>Delete?</span>
                        <button className="rs-sc-yes" onClick={() => removeRule(r.name)}>Delete</button>
                        <button className="rs-sc-no" onClick={() => setConfirmRule(null)}>Cancel</button>
                      </div>
                    ) : (
                      <div className="rs-rule-act">
                        <button title={r.disabled ? "Enable rule" : "Disable rule"} onClick={() => toggleRule(r)}>
                          <Icon name={r.disabled ? "eye" : "eyeoff"} size={14} />
                        </button>
                        <button className="danger" title="Delete rule" onClick={() => setConfirmRule(r.name)}>
                          <Icon name="close" size={14} />
                        </button>
                      </div>
                    )}
                  </td>
                </tr>
              );
            })}
          </tbody>
        </Table>
        )}
      </Card>
      )}

      {!loading && (
      <Card style={{ overflow: "hidden", marginTop: 16 }}>
        <CardHead
          actions={<Button size="sm" variant="ghost" onClick={() => setChanModal(true)}><Icon name="plus" size={13} />New channel</Button>}
        >
          Notification channels
        </CardHead>
        {(channels ?? []).length === 0 ? (
          <div className="rs-empty" style={{ padding: "44px 20px" }}>
            <Icon name="bolt" size={26} />
            <div className="rs-empty-t">No channels yet</div>
            <div className="rs-empty-s">Alerts always emit Rune events; add a webhook or Slack channel to page someone.</div>
            <Button size="sm" variant="primary" onClick={() => setChanModal(true)}><Icon name="plus" size={14} />New channel</Button>
          </div>
        ) : (
        <Table>
          <thead><tr><th>Channel</th><th>Type</th><th>Target</th><th>Updated</th><th /></tr></thead>
          <tbody>
            {(channels ?? []).map((c) => (
              <tr key={c.id || c.name}>
                <td><div className="cell-name" style={{ fontWeight: 500 }}><span className={"rs-chan-ico " + c.type} />{c.name}</div></td>
                <td><Tag>{c.type}</Tag></td>
                <td className="cell-sub mono">{urlHost(c.url)}</td>
                <td className="cell-sub">{timeAgo(c.updatedAt)}</td>
                <td onClick={(e) => e.stopPropagation()}>
                  {confirmChan === c.name ? (
                    <div className="rs-saved-confirm" style={{ marginTop: 0, justifyContent: "flex-end" }}>
                      <span>Delete?</span>
                      <button className="rs-sc-yes" onClick={() => removeChannel(c.name)}>Delete</button>
                      <button className="rs-sc-no" onClick={() => setConfirmChan(null)}>Cancel</button>
                    </div>
                  ) : (
                    <div className="rs-rule-act">
                      <button className="danger" title="Delete channel" onClick={() => setConfirmChan(c.name)}>
                        <Icon name="close" size={14} />
                      </button>
                    </div>
                  )}
                </td>
              </tr>
            ))}
          </tbody>
        </Table>
        )}
      </Card>
      )}

      {ruleModal && <RuleModal channels={channels ?? []} onClose={() => setRuleModal(false)} onSaved={refresh} />}
      {chanModal && <ChannelModal onClose={() => setChanModal(false)} onSaved={refresh} />}
    </div>
  );
}
