import { useEffect, useState } from "react";
import { Badge, Button, Card, CopyButton, Dot, Drawer, EmptyState, Field, Icon, KeyValue, PageHead, Segmented, Select, Spinner, Table, Tabs, Tag, TextInput, useToast } from "../components";
import type { ConfigMap, Secret, SecretMount } from "../api/types";
import { useConfigmaps, useNamespaces, useSecretVersions, useSecrets } from "../api/hooks";
import { useScope } from "../lib/scope";
import "./Secrets.css";

// How runed encrypts secrets at rest (envelope encryption). Static cluster
// crypto config — there is no RPC for it, so it's surfaced as a constant.
const ENCRYPTION = {
  algo: "AES-256-GCM",
  scheme: "envelope encryption",
  kek: { source: "/etc/runed/kek.key", mode: "0600", bytes: 32, loaded: "on server start" },
  dek: "fresh 256-bit DEK generated per secret version",
  aad: "namespace · name · version",
  versionsKept: 5,
};

const DNS1123 = /^[a-z0-9]([-a-z0-9]*[a-z0-9])?$/;
const clone = <T,>(x: T): T => JSON.parse(JSON.stringify(x));
const fmtBytes = (b: number) => (b < 1024 ? `${b} B` : `${(b / 1024).toFixed(1)} KiB`);
const pendingOf = (r: { mounts: SecretMount[]; version: number }) => (r.mounts || []).filter((m) => m.v < r.version).length;

function CopyField({ text, label }: { text: string; label?: string }) {
  return (
    <div className="ref-field">
      {label && <span style={{ color: "var(--text-4)", fontSize: 11 }}>{label}</span>}
      <code>{text}</code>
      <CopyButton value={text} />
    </div>
  );
}

function MountTargets({ res, keys }: { res: SecretMount; keys: string[] }) {
  const list = res.mode === "pull" ? ["imagePullSecret · .dockerconfigjson"] : res.mode === "file" ? keys.map((k) => `${res.at}/${k}`) : keys.map((k) => k);
  const shown = list.slice(0, 4);
  return (
    <div className="mount-targets">
      {shown.map((t) => (
        <div className="mtarget" key={t}>
          <Icon name={res.mode === "file" ? "doc" : res.mode === "pull" ? "cube" : "terminal"} size={12} style={{ color: "var(--text-4)" }} />
          {res.mode === "env" ? <span><b>${t}</b> <span style={{ color: "var(--text-4)" }}>(env)</span></span> : <span>{t}</span>}
        </div>
      ))}
      {list.length > shown.length && <div className="mtarget" style={{ color: "var(--text-4)" }}>+{list.length - shown.length} more</div>}
    </div>
  );
}

function MountCard({ res, version, keyNames, onRestart }: { res: SecretMount; version: number; keyNames: string[]; onRestart: () => void }) {
  const pending = res.v < version;
  return (
    <div className="mount">
      <div className="mount-top">
        <Icon name="services" size={15} style={{ color: "var(--text-3)" }} />
        <span className="mount-svc mono">{res.svc === "*" ? "all services in namespace" : res.svc}</span>
        <span className={`mode-chip ${res.mode}`}>
          <Icon name={res.mode === "file" ? "doc" : res.mode === "pull" ? "cube" : "terminal"} size={12} />
          {res.mode === "file" ? `file → ${res.at}` : res.mode === "pull" ? "imagePullSecret" : "envFrom"}
        </span>
        <div className="mount-state">
          {pending ? (
            <>
              <span className="vstate pending"><Dot s="deploy" /> v{res.v} mounted · v{version} ready</span>
              <Button size="sm" onClick={onRestart}><Icon name="refresh" size={13} />Restart</Button>
            </>
          ) : (
            <span className="vstate ok"><Dot s="run" /> v{res.v} · current</span>
          )}
        </div>
      </div>
      <MountTargets res={res} keys={keyNames} />
    </div>
  );
}

interface VRow { v: number; when: string; keys: number; current: boolean }

// Presentational version list. Author isn't tracked server-side, so rows show
// the version, key count and timestamp — no fabricated "cast by".
function VersionRows({ rows, loading, error, note }: { rows: VRow[]; loading?: boolean; error?: string | null; note?: string }) {
  if (loading) return <Spinner label="Loading versions…" height={120} />;
  if (error) return <EmptyState icon="secrets" tone="error" title="Couldn't load versions" hint={error} />;
  if (rows.length === 0) return <EmptyState icon="secrets" title="No version history" />;
  return (
    <div className="fadein">
      {note && <p style={{ fontSize: 11.5, color: "var(--text-3)", margin: "0 0 12px", fontFamily: "var(--mono)" }}>{note}</p>}
      {rows.map((h) => (
        <div key={h.v} style={{ display: "flex", alignItems: "center", gap: 13, padding: "13px 4px", borderBottom: "1px solid var(--border-faint)" }}>
          <span className="mono" style={{ fontSize: 13, fontWeight: 600, color: h.current ? "var(--accent-text)" : "var(--text-2)", width: 36 }}>v{h.v}</span>
          <Dot s={h.current ? "net" : "idle"} />
          <span style={{ fontSize: 12.5, color: "var(--text-2)" }}>{h.keys} {h.keys === 1 ? "key" : "keys"}</span>
          {h.current && <Badge s="accent">current</Badge>}
          <span className="mono" style={{ marginLeft: "auto", fontSize: 11.5, color: "var(--text-3)" }}>{h.when}</span>
        </div>
      ))}
    </div>
  );
}

// Secrets have a real per-version history RPC; configmaps don't, so we show
// only the current version for those.
function SecretVersions({ sec }: { sec: Secret }) {
  const { data, loading, error } = useSecretVersions(sec.name, sec.ns);
  return <VersionRows rows={data.map((v) => ({ v: v.version, when: v.when, keys: v.keys, current: v.current }))} loading={loading} error={error} />;
}

function PendingBanner({ pend, version, color = "deploy" }: { pend: number; version: number; color?: string }) {
  if (pend <= 0) return null;
  return (
    <div style={{ marginTop: 11, padding: "8px 12px", background: `var(--${color}-dim)`, border: "1px solid rgba(247,104,9,.28)", borderRadius: 8, fontSize: 12.5, color: "#e8a06a", display: "flex", gap: 8, alignItems: "flex-start", lineHeight: 1.5 }}>
      <Icon name="alert" size={14} style={{ color: "var(--deploy)", flex: "none", marginTop: 2 }} />
      <span><b style={{ color: "#f3b483" }}>{pend} {pend === 1 ? "service" : "services"}</b> still running an older version — restart to apply v{version}.</span>
    </div>
  );
}

/* ---------------- SECRET DRAWER ---------------- */
function SecretDrawer({ sec, onClose, onRestart, fqdn, setFqdn }: { sec: Secret; onClose: () => void; onRestart: (n: string, svc: string) => void; fqdn: boolean; setFqdn: (v: boolean) => void }) {
  const [tab, setTab] = useState("overview");
  const keyNames = sec.keys.map((k) => k.k);
  const pend = pendingOf(sec);
  const E = ENCRYPTION;
  const ref = (k: string) => (fqdn ? `secret:${sec.name}.${sec.ns}.rune/${k}` : `secret:${sec.name}/${k}`);
  return (
    <Drawer onClose={onClose}>
      <div className="drawer-head">
        <div className="eyebrow" style={{ marginBottom: 8 }}>{sec.ns} / secret</div>
        <div style={{ display: "flex", alignItems: "center", gap: 12, marginBottom: 14, flexWrap: "wrap" }}>
          <h2 className="mono" style={{ fontFamily: "var(--serif)", fontSize: 25, fontWeight: 500, margin: 0, letterSpacing: "-0.01em" }}>{sec.name}</h2>
          <Badge s="accent"><Icon name="secrets" size={12} />encrypted</Badge>
          <Tag>{sec.type}</Tag>
          <Tag>v{sec.version}</Tag>
        </div>
        <div style={{ display: "flex", gap: 8, flexWrap: "wrap" }}>
          <Button size="sm" variant="primary"><Icon name="refresh" size={14} />Rotate</Button>
          <Button size="sm"><Icon name="bolt" size={14} />Cast new version</Button>
          <Button size="sm"><Icon name="link" size={14} />Mount into service</Button>
        </div>
        {sec.auto && (
          <div style={{ marginTop: 13, padding: "8px 12px", background: "var(--net-dim)", border: "1px solid rgba(103,221,253,.25)", borderRadius: 8, fontSize: 12.5, color: "var(--net)", display: "flex", gap: 8, alignItems: "center" }}>
            <Icon name="refresh" size={14} />{sec.auto}
          </div>
        )}
        <PendingBanner pend={pend} version={sec.version} />
      </div>
      <div className="drawer-body">
        <Tabs
          tabs={[
            { id: "overview", label: "Overview" },
            { id: "keys", label: "Keys" },
            { id: "mounts", label: `Mounts (${sec.mounts.length})` },
            { id: "versions", label: "Versions" },
            { id: "encryption", label: "Encryption" },
          ]}
          active={tab}
          onChange={setTab}
        />
        {tab === "overview" && (
          <div className="fadein">
            <KeyValue>
              <dt>Namespace</dt><dd>{sec.ns}</dd>
              <dt>Type</dt><dd>{sec.type}</dd>
              <dt>Version</dt><dd>v{sec.version}</dd>
              <dt>Keys</dt><dd>{sec.keys.length}</dd>
              <dt>Consumers</dt><dd>{sec.mounts.length} mount{sec.mounts.length === 1 ? "" : "s"}</dd>
              <dt>Last cast</dt><dd>{sec.updated} · by {sec.castBy}</dd>
              <dt>Created</dt><dd>{sec.age} ago</dd>
              <dt>Encryption</dt><dd>{E.algo}</dd>
            </KeyValue>
            <div className="divider" />
            <div className="eyebrow" style={{ marginBottom: 10, display: "flex", alignItems: "center", gap: 10 }}>
              Secret reference
              <span style={{ marginLeft: "auto" }}>
                <Segmented options={[{ value: "short", label: "shorthand" }, { value: "fqdn", label: "FQDN" }]} value={fqdn ? "fqdn" : "short"} onChange={(v) => setFqdn(v === "fqdn")} />
              </span>
            </div>
            <CopyField text={ref(keyNames[0])} />
            <p style={{ fontSize: 12, color: "var(--text-3)", marginTop: 9, lineHeight: 1.6 }}>
              Use this string where a literal value is expected (e.g. a StorageClass parameter). {fqdn ? "FQDN form names the namespace explicitly — for cluster-scoped consumers." : "Shorthand resolves in the consumer's own namespace."}
            </p>
          </div>
        )}
        {tab === "keys" && (
          <div className="fadein">
            <div style={{ display: "flex", alignItems: "center", gap: 8, marginBottom: 14, color: "var(--text-3)", fontSize: 12 }}>
              <Icon name="secrets" size={14} style={{ color: "var(--accent-text)" }} />
              Values are sealed — there is no API to read plaintext. Mount the secret into a service to use it.
            </div>
            {sec.keys.map((k) => (
              <div className="kvrow" key={k.k}>
                <Icon name="secrets" size={13} className="seal-ico" style={{ color: "var(--accent-text)" }} />
                <span className="kvr-name">{k.k}</span>
                <span className="seal" style={{ marginLeft: "auto" }}><span className="seal-dots">••••••••••••••</span> sealed</span>
                <span className="kvr-meta">{fmtBytes(k.bytes)}</span>
              </div>
            ))}
          </div>
        )}
        {tab === "mounts" && (
          <div className="fadein">
            {sec.mounts.length === 0 ? <div className="empty">Not mounted by any service yet.</div> : (
              <>
                <p style={{ fontSize: 12.5, color: "var(--text-3)", margin: "0 0 14px", lineHeight: 1.6 }}>
                  Each consumer pins a version at mount time. Rune does not hot-reload — a service picks up a new version only on its next instance restart.
                </p>
                {sec.mounts.map((m) => <MountCard key={m.svc} res={m} version={sec.version} keyNames={keyNames} onRestart={() => onRestart(sec.name, m.svc)} />)}
              </>
            )}
          </div>
        )}
        {tab === "versions" && <SecretVersions sec={sec} />}
        {tab === "encryption" && (
          <div className="fadein">
            <div className="envelope">
              <div className="env-flow">
                <span className="env-box">plaintext</span>
                <span className="env-arrow">→</span>
                <div style={{ textAlign: "center" }}><span className="env-box key">AES-256-GCM</span><div className="env-op">DEK</div></div>
                <span className="env-arrow">→</span>
                <span className="env-box cipher">ciphertext</span>
              </div>
              <div className="env-flow">
                <span className="env-box key">DEK</span>
                <span className="env-arrow">→</span>
                <div style={{ textAlign: "center" }}><span className="env-box key">AES-256-GCM</span><div className="env-op">KEK</div></div>
                <span className="env-arrow">→</span>
                <span className="env-box">wrapped DEK</span>
                <span style={{ fontFamily: "var(--mono)", fontSize: 11, color: "var(--text-4)" }}>stored together</span>
              </div>
            </div>
            <div className="divider" />
            <KeyValue>
              <dt>Scheme</dt><dd>{E.scheme}</dd>
              <dt>Algorithm</dt><dd>{E.algo}</dd>
              <dt>DEK</dt><dd>{E.dek}</dd>
              <dt>KEK source</dt><dd>{E.kek.source} ({E.kek.mode})</dd>
              <dt>Associated data</dt><dd>{E.aad}</dd>
            </KeyValue>
            <div style={{ marginTop: 14, padding: "10px 13px", background: "var(--warn-dim)", border: "1px solid rgba(217,154,62,.22)", borderRadius: 8, fontSize: 12.5, color: "#d9a94e", display: "flex", gap: 9, alignItems: "center" }}>
              <Icon name="alert" size={15} />Associated data binds ciphertext to (namespace, name, version). Lose the KEK, lose every secret — back it up.
            </div>
          </div>
        )}
      </div>
    </Drawer>
  );
}

/* ---------------- CONFIGMAP DRAWER ---------------- */
function ConfigDrawer({ cm, onClose, onRestart }: { cm: ConfigMap; onClose: () => void; onRestart: (n: string, svc: string) => void }) {
  const [tab, setTab] = useState("data");
  const keyNames = cm.data.map((d) => d.k);
  const pend = pendingOf(cm);
  return (
    <Drawer onClose={onClose}>
      <div className="drawer-head">
        <div className="eyebrow" style={{ marginBottom: 8 }}>{cm.ns} / configmap</div>
        <div style={{ display: "flex", alignItems: "center", gap: 12, marginBottom: 14, flexWrap: "wrap" }}>
          <h2 className="mono" style={{ fontFamily: "var(--serif)", fontSize: 25, fontWeight: 500, margin: 0, letterSpacing: "-0.01em" }}>{cm.name}</h2>
          <Tag><Icon name="doc" size={11} style={{ marginRight: 4, verticalAlign: "-1px" }} />plaintext</Tag>
          <Tag>v{cm.version}</Tag>
        </div>
        <div style={{ display: "flex", gap: 8, flexWrap: "wrap" }}>
          <Button size="sm" variant="primary"><Icon name="bolt" size={14} />Edit &amp; cast</Button>
          <Button size="sm"><Icon name="link" size={14} />Mount into service</Button>
        </div>
        <PendingBanner pend={pend} version={cm.version} />
      </div>
      <div className="drawer-body">
        <Tabs tabs={[{ id: "data", label: "Data" }, { id: "mounts", label: `Mounts (${cm.mounts.length})` }, { id: "versions", label: "Versions" }]} active={tab} onChange={setTab} />
        {tab === "data" && (
          <div className="fadein">
            <div style={{ display: "flex", alignItems: "center", gap: 8, marginBottom: 14, color: "var(--text-3)", fontSize: 12 }}>
              <Icon name="doc" size={14} />Plaintext — not encrypted. Use only for non-sensitive config.
            </div>
            {cm.data.map((d) => (
              <div key={d.k} style={{ marginBottom: 13 }}>
                <div style={{ display: "flex", alignItems: "center", gap: 9, marginBottom: 6 }}>
                  <Icon name={d.file ? "doc" : "instances"} size={13} style={{ color: "var(--text-3)" }} />
                  <span className="mono" style={{ fontSize: 12.5, fontWeight: 600, color: "var(--text)" }}>{d.k}</span>
                  {d.file && <Tag>file</Tag>}
                  <span className="mono" style={{ marginLeft: "auto", fontSize: 11, color: "var(--text-4)" }}>{fmtBytes(d.value.length)}</span>
                </div>
                {d.value.includes("\n") ? (
                  <pre style={{ margin: 0, background: "var(--inset)", border: "1px solid var(--border)", borderRadius: 7, padding: "11px 13px", fontFamily: "var(--mono)", fontSize: 12, color: "var(--text-2)", lineHeight: 1.65, overflowX: "auto" }}>{d.value}</pre>
                ) : (
                  <div className="ref-field"><code style={{ color: "var(--text)" }}>{d.value}</code></div>
                )}
              </div>
            ))}
          </div>
        )}
        {tab === "mounts" && (
          <div className="fadein">
            {cm.mounts.length === 0 ? <div className="empty">Not mounted by any service yet.</div> : (
              <>
                <p style={{ fontSize: 12.5, color: "var(--text-3)", margin: "0 0 14px", lineHeight: 1.6 }}>Mounted files are not hot-reloaded. Restart a consumer to roll it onto the current version.</p>
                {cm.mounts.map((m) => <MountCard key={m.svc} res={m} version={cm.version} keyNames={keyNames} onRestart={() => onRestart(cm.name, m.svc)} />)}
              </>
            )}
          </div>
        )}
        {tab === "versions" && (
          <VersionRows
            rows={[{ v: cm.version, when: cm.updated, keys: cm.data.length, current: true }]}
            note="ConfigMap version history isn't retained — showing the current version."
          />
        )}
      </div>
    </Drawer>
  );
}

/* ---------------- CREATE DRAWER ---------------- */
interface Row { k: string; v: string; file: string; bytes: number }
function CreateDrawer({ onClose, onCreate }: { onClose: () => void; onCreate: (kind: string, res: Secret | ConfigMap) => void }) {
  const { data: namespaces } = useNamespaces();
  const [kind, setKind] = useState("secret");
  const [name, setName] = useState("");
  const [ns, setNs] = useState("default");
  const [stype, setStype] = useState("Opaque");
  const [source, setSource] = useState("literal");
  const [rows, setRows] = useState<Row[]>([{ k: "", v: "", file: "", bytes: 0 }]);

  const nameTouched = name.length > 0;
  const nameValid = DNS1123.test(name) && name.length <= 63;
  const validRows = rows.filter((r) => r.k && (source === "file" ? r.file : r.v));
  const canCreate = nameValid && validRows.length > 0;

  const setRow = (i: number, patch: Partial<Row>) => setRows((rs) => rs.map((r, j) => (j === i ? { ...r, ...patch } : r)));
  const addRow = () => setRows((rs) => [...rs, { k: "", v: "", file: "", bytes: 0 }]);
  const delRow = (i: number) => setRows((rs) => (rs.length === 1 ? rs : rs.filter((_, j) => j !== i)));

  const cmd = (() => {
    const verb = kind === "secret" ? "create secret" : "create config";
    const parts = [`rune ${verb} ${name || "<name>"}`, `-n ${ns}`];
    if (kind === "secret" && stype !== "Opaque") parts.push(`--type ${stype}`);
    validRows.forEach((r) => parts.push(source === "file" ? `--from-file=${r.k}=${r.file}` : `--from-literal=${r.k}=${r.v}`));
    return parts;
  })();

  function submit() {
    if (!canCreate) return;
    const base = { name, ns, version: 1, age: "just now", updated: "just now", castBy: "ore", mounts: [], usedBy: [] };
    const res: Secret | ConfigMap =
      kind === "secret"
        ? { ...base, type: stype, keys: validRows.map((r) => ({ k: r.k, bytes: source === "file" ? r.bytes || 0 : r.v.length })) }
        : { ...base, data: validRows.map((r) => ({ k: r.k, file: source === "file", value: source === "file" ? `(from ${r.file})` : r.v })) };
    onCreate(kind, res);
  }

  return (
    <Drawer onClose={onClose}>
      <div className="drawer-head">
        <div className="eyebrow" style={{ marginBottom: 8 }}>create resource</div>
        <h2 style={{ fontFamily: "var(--serif)", fontSize: 25, fontWeight: 500, margin: "0 0 14px", letterSpacing: "-0.01em" }}>New {kind === "secret" ? "secret" : "configmap"}</h2>
        <Segmented options={[{ value: "secret", label: "Secret · encrypted" }, { value: "config", label: "ConfigMap · plaintext" }]} value={kind} onChange={setKind} />
      </div>
      <div className="drawer-body">
        <div style={{ display: "grid", gridTemplateColumns: kind === "secret" ? "1.3fr 1fr 1fr" : "1.3fr 1fr", gap: 12, marginBottom: 20 }}>
          <Field
            label="Name"
            hint={!nameTouched ? "DNS-1123: a–z, 0–9, hyphen" : undefined}
            success={nameTouched && nameValid ? "✓ valid name" : undefined}
            error={nameTouched && !nameValid ? "must match [a-z0-9]([-a-z0-9]*[a-z0-9])?" : undefined}
          >
            <TextInput value={name} placeholder="db-credentials" autoFocus invalid={nameTouched && !nameValid} onChange={(e) => setName(e.target.value)} />
          </Field>
          <Field label="Namespace">
            <Select value={ns} onChange={(e) => setNs(e.target.value)}>
              {namespaces.map((n) => <option key={n.name} value={n.name}>{n.name}</option>)}
            </Select>
          </Field>
          {kind === "secret" && (
            <Field label="Type">
              <Select value={stype} onChange={(e) => setStype(e.target.value)}>
                {["Opaque", "tls", "dockerconfigjson"].map((t) => <option key={t} value={t}>{t}</option>)}
              </Select>
            </Field>
          )}
        </div>

        <div style={{ display: "flex", alignItems: "center", marginBottom: 12 }}>
          <label className="field-label" style={{ margin: 0 }}>Data</label>
          <span style={{ marginLeft: "auto" }}>
            <Segmented options={[{ value: "literal", label: "from literal" }, { value: "file", label: "from file" }]} value={source} onChange={setSource} />
          </span>
        </div>

        {rows.map((r, i) => (
          <div className="kentry" key={i}>
            <TextInput placeholder={source === "file" ? "tls.crt" : "key"} value={r.k} onChange={(e) => setRow(i, { k: e.target.value })} />
            {source === "literal" ? (
              <TextInput type={kind === "secret" ? "password" : "text"} placeholder="value" value={r.v} onChange={(e) => setRow(i, { v: e.target.value })} />
            ) : (
              <label className="field-control" style={{ cursor: "pointer", display: "flex", alignItems: "center", gap: 8, color: r.file ? "var(--text)" : "var(--text-3)" }}>
                <Icon name="doc" size={13} />
                <span style={{ overflow: "hidden", textOverflow: "ellipsis", whiteSpace: "nowrap" }}>{r.file ? `${r.file} · ${fmtBytes(r.bytes)}` : "Choose file…"}</span>
                <input type="file" style={{ display: "none" }} onChange={(e) => { const f = e.target.files?.[0]; if (f) setRow(i, { file: f.name, bytes: f.size, k: r.k || f.name }); }} />
              </label>
            )}
            <Button icon variant="ghost" size="sm" onClick={() => delRow(i)} disabled={rows.length === 1} style={{ opacity: rows.length === 1 ? 0.3 : 1 }}><Icon name="close" size={14} /></Button>
          </div>
        ))}
        <Button size="sm" variant="ghost" onClick={addRow} style={{ marginBottom: 22 }}><Icon name="plus" size={13} />Add key</Button>

        <div className="eyebrow" style={{ marginBottom: 9 }}>Equivalent command</div>
        <div className="cmd-preview">
          <span className="c-cmd">{cmd[0]}</span>
          {cmd.slice(1).map((p, i) => <span key={i}> {p.startsWith("--") || p.startsWith("-n") ? <span className="c-flag">{p}</span> : p}</span>)}
        </div>
        <p style={{ fontSize: 11.5, color: "var(--text-4)", marginTop: 9, fontFamily: "var(--mono)" }}>
          {kind === "secret" ? "Encrypted with a fresh DEK and sealed at v1 on cast." : "Stored in plaintext and applied at v1 on cast."}
        </p>
      </div>
      <div style={{ borderTop: "1px solid var(--border)", padding: "14px 24px", display: "flex", gap: 10, justifyContent: "flex-end" }}>
        <Button onClick={onClose}>Cancel</Button>
        <Button variant="primary" disabled={!canCreate} onClick={submit} style={{ opacity: canCreate ? 1 : 0.45, cursor: canCreate ? "pointer" : "default" }}>
          <Icon name="bolt" size={14} />Cast {kind === "secret" ? "secret" : "config"}
        </Button>
      </div>
    </Drawer>
  );
}

/* ---------------- MAIN SCREEN ---------------- */
export function Secrets() {
  const { data: liveSecrets, loading: sLoading, error: sError, reload: sReload } = useSecrets();
  const { data: liveConfigs, loading: cLoading, error: cError } = useConfigmaps();
  const loading = sLoading || cLoading;
  const error = sError || cError;

  const [tab, setTab] = useState("secrets");
  const [secrets, setSecrets] = useState<Secret[]>(() => clone(liveSecrets));
  const [configs, setConfigs] = useState<ConfigMap[]>(() => clone(liveConfigs));
  // Re-seed local working state whenever the live lists resolve/change.
  useEffect(() => { setSecrets(clone(liveSecrets)); }, [liveSecrets]);
  useEffect(() => { setConfigs(clone(liveConfigs)); }, [liveConfigs]);
  const [openSec, setOpenSec] = useState<string | null>(null);
  const [openCm, setOpenCm] = useState<string | null>(null);
  const [creating, setCreating] = useState(false);
  const [flash, setFlash] = useState<string | null>(null);
  const [fqdn, setFqdn] = useState(false);
  const toast = useToast();

  const E = ENCRYPTION;
  const secDetail = secrets.find((s) => s.name === openSec);
  const cmDetail = configs.find((c) => c.name === openCm);

  const pendingTotal = secrets.reduce((a, s) => a + pendingOf(s), 0) + configs.reduce((a, c) => a + pendingOf(c), 0);
  const pendingResCount = secrets.filter((s) => pendingOf(s)).length + configs.filter((c) => pendingOf(c)).length;

  // Active namespace scope filters the list views (the roll-up banner above
  // stays cluster-wide — it's a global rollout signal).
  const { ns: scopeNs } = useScope();
  const inScope = <T extends { ns: string }>(arr: T[]) => (scopeNs === "all" ? arr : arr.filter((r) => r.ns === scopeNs));
  const shownSecrets = inScope(secrets);
  const shownConfigs = inScope(configs);
  const pendingSec = shownSecrets.reduce((a, s) => a + pendingOf(s), 0);
  const pendingCm = shownConfigs.reduce((a, c) => a + pendingOf(c), 0);

  const restartSec = (resName: string, svc: string) => setSecrets((list) => list.map((r) => (r.name !== resName ? r : { ...r, mounts: r.mounts.map((m) => (m.svc === svc ? { ...m, v: r.version } : m)) })));
  const restartCm = (resName: string, svc: string) => setConfigs((list) => list.map((r) => (r.name !== resName ? r : { ...r, mounts: r.mounts.map((m) => (m.svc === svc ? { ...m, v: r.version } : m)) })));

  function create(kind: string, res: Secret | ConfigMap) {
    if (kind === "secret") { setSecrets((l) => [res as Secret, ...l]); setTab("secrets"); }
    else { setConfigs((l) => [res as ConfigMap, ...l]); setTab("config"); }
    setCreating(false);
    setFlash(res.name);
    setTimeout(() => setFlash(null), 1300);
    toast({
      tone: "success",
      icon: "check",
      title: <><b>{kind === "secret" ? "Secret" : "ConfigMap"} cast</b> — {res.name}</>,
      message: `${kind}/${res.name} · ${res.ns} · ${kind === "secret" ? "sealed at v1" : "applied at v1"}`,
    });
  }

  function consumerSummary(r: { mounts: SecretMount[] }) {
    const n = r.mounts.length;
    if (n === 0) return <span style={{ color: "var(--text-4)" }}>—</span>;
    return (
      <span style={{ display: "inline-flex", alignItems: "center", gap: 8, whiteSpace: "nowrap" }}>
        <span className="mono" style={{ fontSize: 12, color: "var(--text-2)" }}>{r.mounts[0].svc === "*" ? "all svc" : r.mounts[0].svc}</span>
        {n > 1 && <Tag>+{n - 1}</Tag>}
      </span>
    );
  }
  function statusCell(r: { mounts: SecretMount[]; version: number }) {
    const pend = pendingOf(r);
    if (pend > 0) return <Badge s="deploy">{pend} pending</Badge>;
    return <Badge s="accent"><Icon name="check" size={12} />rolled out</Badge>;
  }

  return (
    <div className="wrap">
      <PageHead
        eyebrow={`${secrets.length} secrets · ${configs.length} configmaps${pendingTotal ? ` · ${pendingTotal} pending` : ""}`}
        title="Secrets <em>&</em> Config"
        sub="Namespaced key-value resources mounted into services. Secrets are encrypted at rest with envelope encryption; configmaps are plaintext. Updates roll out on the next instance restart."
        actions={<Button size="sm" variant="primary" onClick={() => setCreating(true)}><Icon name="plus" size={14} />Create</Button>}
      />

      <div className="enc-strip">
        <div className="enc-seg lead">
          <div className="enc-k"><Icon name="secrets" size={12} style={{ color: "var(--accent-text)" }} />encrypted at rest</div>
          <div className="enc-v">{E.algo} <small>· {E.scheme}</small></div>
        </div>
        <div className="enc-seg">
          <div className="enc-k"><Icon name="shield" size={12} />KEK</div>
          <div className="enc-v">loaded <small>{E.kek.source} · {E.kek.mode}</small></div>
        </div>
        <div className="enc-seg">
          <div className="enc-k"><Icon name="refresh" size={12} />DEK</div>
          <div className="enc-v">per version <small>{E.kek.bytes * 8}-bit</small></div>
        </div>
        <div className="enc-seg">
          <div className="enc-k"><Icon name="clock" size={12} />versions kept</div>
          <div className="enc-v">{E.versionsKept} <small>· older GC&apos;d</small></div>
        </div>
      </div>

      {pendingTotal > 0 && (
        <div className="roll-banner">
          <Icon name="alert" size={18} className="rb-ico" />
          <div className="rb-txt">
            <b>{pendingResCount} {pendingResCount === 1 ? "resource has" : "resources have"}</b> a newer version than what {pendingTotal === 1 ? "a service is" : "some services are"} running. Restart the affected services to roll out — Rune does not hot-reload mounted values.
          </div>
        </div>
      )}

      <Tabs
        tabs={[
          { id: "secrets", label: `Secrets (${shownSecrets.length})${pendingSec ? ` · ${pendingSec}` : ""}` },
          { id: "config", label: `ConfigMaps (${shownConfigs.length})${pendingCm ? ` · ${pendingCm}` : ""}` },
        ]}
        active={tab}
        onChange={setTab}
      />

      {tab === "secrets" && (
        loading ? (
          <Spinner label="Loading secrets…" />
        ) : error ? (
          <EmptyState icon="secrets" tone="error" title="Couldn't load secrets" hint={error} action={{ label: "Retry", onClick: sReload }} />
        ) : shownSecrets.length === 0 ? (
          <EmptyState icon="secrets" title="No secrets" hint={scopeNs === "all" ? "Create a sealed secret — values are encrypted at rest and never revealed in the UI." : `No secrets in the ${scopeNs} namespace.`} />
        ) : (
        <Card className="fadein" style={{ overflow: "hidden" }}>
          <Table>
            <thead><tr><th>Secret</th><th>Type</th><th>Keys</th><th>Ver</th><th>Mounted by</th><th>Status</th><th>Last cast</th></tr></thead>
            <tbody>
              {shownSecrets.map((s) => (
                <tr key={`${s.ns}/${s.name}`} className={flash === s.name ? "row-flash" : ""} onClick={() => setOpenSec(s.name)}>
                  <td>
                    <div className="cell-name mono" style={{ fontSize: 13 }}><Icon name="secrets" size={15} style={{ color: "var(--accent-text)" }} />{s.name}</div>
                    <div className="cell-sub" style={{ marginTop: 3, marginLeft: 25 }}>{s.ns}</div>
                  </td>
                  <td><Tag>{s.type}</Tag></td>
                  <td className="num" style={{ color: "var(--text-2)" }}>{s.keys.length}</td>
                  <td className="num" style={{ color: "var(--text-2)" }}>v{s.version}</td>
                  <td>{consumerSummary(s)}</td>
                  <td>{statusCell(s)}</td>
                  <td className="cell-sub">{s.updated} · {s.castBy}</td>
                </tr>
              ))}
            </tbody>
          </Table>
        </Card>
        )
      )}

      {tab === "config" && (
        loading ? (
          <Spinner label="Loading configmaps…" />
        ) : error ? (
          <EmptyState icon="doc" tone="error" title="Couldn't load configmaps" hint={error} action={{ label: "Retry", onClick: sReload }} />
        ) : shownConfigs.length === 0 ? (
          <EmptyState icon="doc" title="No configmaps" hint={scopeNs === "all" ? "Cast a configmap to share plaintext configuration with services." : `No configmaps in the ${scopeNs} namespace.`} />
        ) : (
        <Card className="fadein" style={{ overflow: "hidden" }}>
          <Table>
            <thead><tr><th>ConfigMap</th><th>Keys</th><th>Ver</th><th>Mounted by</th><th>Status</th><th>Last cast</th></tr></thead>
            <tbody>
              {shownConfigs.map((c) => (
                <tr key={`${c.ns}/${c.name}`} className={flash === c.name ? "row-flash" : ""} onClick={() => setOpenCm(c.name)}>
                  <td>
                    <div className="cell-name mono" style={{ fontSize: 13 }}><Icon name="doc" size={15} style={{ color: "var(--text-3)" }} />{c.name}</div>
                    <div className="cell-sub" style={{ marginTop: 3, marginLeft: 25 }}>{c.ns}</div>
                  </td>
                  <td><div style={{ display: "flex", gap: 4, flexWrap: "wrap" }}>{c.data.slice(0, 3).map((d) => <Tag key={d.k}>{d.k}</Tag>)}</div></td>
                  <td className="num" style={{ color: "var(--text-2)" }}>v{c.version}</td>
                  <td>{consumerSummary(c)}</td>
                  <td>{statusCell(c)}</td>
                  <td className="cell-sub">{c.updated} · {c.castBy}</td>
                </tr>
              ))}
            </tbody>
          </Table>
        </Card>
        )
      )}

      {secDetail && <SecretDrawer sec={secDetail} onClose={() => setOpenSec(null)} onRestart={restartSec} fqdn={fqdn} setFqdn={setFqdn} />}
      {cmDetail && <ConfigDrawer cm={cmDetail} onClose={() => setOpenCm(null)} onRestart={restartCm} />}
      {creating && <CreateDrawer onClose={() => setCreating(false)} onCreate={create} />}
    </div>
  );
}
