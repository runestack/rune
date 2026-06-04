import { useState } from "react";
import { Badge, Button, Card, Dot, Drawer, Icon, KeyValue, Replicas, Spinner, Table, Tabs, Tag, UsageBar } from "../components";
import type { Service } from "../mock/data";
import { useServiceInstances } from "../api/hooks";
import { useDemo } from "../api/demo";
import { clients } from "../api/transport";
import { ScalingMode } from "../gen/pkg/api/proto/service_pb";

export function ServiceDrawer({ svc, onClose, go }: { svc: Service; onClose: () => void; go: (r: string, arg?: Service) => void }) {
  const demo = useDemo();
  const [tab, setTab] = useState("overview");
  const { data: insts, loading: instLoading, reload: reloadInsts } = useServiceInstances(svc.name, svc.ns);
  const [busy, setBusy] = useState<string | null>(null);
  const [actionErr, setActionErr] = useState<string | null>(null);

  async function withConfirm(label: string, fn: () => Promise<void>) {
    if (demo) {
      // No backend in demo mode — actions are illustrative only.
      window.alert(`${label} is available in a live session against a real cluster.`);
      return;
    }
    if (!window.confirm(`${label} ${svc.name} in ${svc.ns}?`)) return;
    setBusy(label);
    setActionErr(null);
    try {
      await fn();
      reloadInsts();
    } catch (e) {
      setActionErr(e instanceof Error ? e.message : String(e));
    } finally {
      setBusy(null);
    }
  }

  const onScale = () => {
    const raw = window.prompt(`Scale ${svc.name} to how many instances?`, String(svc.want));
    if (raw == null) return;
    const scale = parseInt(raw, 10);
    if (Number.isNaN(scale) || scale < 0) { window.alert("Enter a non-negative integer."); return; }
    void withConfirm(`Scale to ${scale}`, async () => {
      await clients.services.scaleService({ name: svc.name, namespace: svc.ns, scale, mode: ScalingMode.IMMEDIATE });
    });
  };
  const onRestart = () => void withConfirm("Restart", async () => {
    // Restart = scale to 0 then back to desired count (mirrors `rune restart`).
    await clients.services.scaleService({ name: svc.name, namespace: svc.ns, scale: 0, mode: ScalingMode.IMMEDIATE });
    await clients.services.scaleService({ name: svc.name, namespace: svc.ns, scale: svc.want || 1, mode: ScalingMode.IMMEDIATE });
  });
  const onDelete = () => void withConfirm("Delete", async () => {
    await clients.services.deleteService({ name: svc.name, namespace: svc.ns });
    onClose();
  });

  return (
    <Drawer onClose={onClose}>
      <div className="drawer-head">
        <div className="eyebrow" style={{ marginBottom: 8 }}>{svc.ns} / service</div>
        <div style={{ display: "flex", alignItems: "center", gap: 12, marginBottom: 14 }}>
          <h2 style={{ fontFamily: "var(--serif)", fontSize: 26, fontWeight: 500, margin: 0, letterSpacing: "-0.01em" }}>{svc.name}</h2>
          <Badge s={svc.status} />
          {svc.stateful && <Tag>stateful</Tag>}
          {svc.type === "process" && <Tag>process</Tag>}
        </div>
        <div style={{ display: "flex", gap: 8, flexWrap: "wrap" }}>
          <Button size="sm" variant="primary" onClick={onScale} disabled={!!busy}><Icon name="scale" size={14} />Scale</Button>
          <Button size="sm" onClick={onRestart} disabled={!!busy}><Icon name="refresh" size={14} />Restart</Button>
          <Button size="sm" onClick={() => { onClose(); go("logs", svc); }}><Icon name="logs" size={14} />Logs</Button>
          <Button size="sm" onClick={() => { onClose(); go("logs", svc); }}><Icon name="terminal" size={14} />Exec</Button>
          <Button size="sm" onClick={onDelete} disabled={!!busy}><Icon name="close" size={14} />Delete</Button>
        </div>
        {busy && <div style={{ marginTop: 12, fontSize: 12.5, color: "var(--text-3)", display: "flex", gap: 8, alignItems: "center" }}><Dot s="deploy" pulse />{busy}…</div>}
        {actionErr && (
          <div style={{ marginTop: 12, padding: "9px 12px", background: "var(--fail-dim)", border: "1px solid rgba(229,72,77,.25)", borderRadius: 8, fontSize: 12.5, color: "#f2868a", display: "flex", gap: 8, alignItems: "center" }}>
            <Icon name="alert" size={15} />{actionErr}
          </div>
        )}
        {svc.note && !actionErr && (
          <div style={{
            marginTop: 14, padding: "9px 12px",
            background: svc.status === "warn" ? "var(--fail-dim)" : "var(--deploy-dim)",
            border: `1px solid ${svc.status === "warn" ? "rgba(229,72,77,.25)" : "rgba(247,104,9,.25)"}`,
            borderRadius: 8, fontSize: 12.5, color: svc.status === "warn" ? "#f2868a" : "#f79050",
            display: "flex", gap: 8, alignItems: "center",
          }}>
            <Icon name="health" size={15} />{svc.note}
          </div>
        )}
      </div>
      <div className="drawer-body">
        <Tabs
          tabs={[
            { id: "overview", label: "Overview" },
            { id: "instances", label: `Instances (${insts.length})` },
            { id: "logs", label: "Logs" },
            { id: "networking", label: "Networking" },
          ]}
          active={tab}
          onChange={setTab}
        />

        {tab === "overview" && (
          <div className="fadein">
            <div className="grid g-2" style={{ marginBottom: 20 }}>
              <Card pad><div className="eyebrow" style={{ marginBottom: 12 }}>CPU</div><UsageBar v={svc.cpu} w={130} /></Card>
              <Card pad><div className="eyebrow" style={{ marginBottom: 12 }}>Memory</div><UsageBar v={svc.mem} w={130} /></Card>
            </div>
            <KeyValue>
              <dt>Status</dt><dd><Badge s={svc.status} /></dd>
              <dt>Replicas</dt><dd><Replicas ready={svc.ready} want={svc.want} /> ready</dd>
              <dt>Image</dt><dd style={{ wordBreak: "break-all" }}>{svc.image}</dd>
              <dt>Runeset</dt><dd>{svc.runeset}</dd>
              <dt>Type</dt><dd>{svc.type}</dd>
              {svc.schedule && <><dt>Schedule</dt><dd>{svc.schedule}</dd></>}
              <dt>Ports</dt><dd>{svc.ports.length ? svc.ports.join("  ") : "—"}</dd>
              <dt>Network policy</dt><dd>{svc.policy}</dd>
              {svc.volume && <><dt>Volume</dt><dd>{svc.volume}</dd></>}
              <dt>Restarts</dt><dd>{svc.restarts}</dd>
              <dt>Age</dt><dd>{svc.age}</dd>
            </KeyValue>
          </div>
        )}

        {tab === "instances" && (
          <Card className="fadein" style={{ overflow: "hidden" }}>
            {instLoading ? (
              <Spinner label="Loading instances…" height={120} />
            ) : insts.length === 0 ? (
              <div className="empty">No instances scheduled.</div>
            ) : (
              <Table>
                <thead><tr><th>Instance</th><th>Node</th><th>CPU</th><th>Mem</th><th>Restarts</th><th>Status</th></tr></thead>
                <tbody>
                  {insts.map((i) => (
                    <tr key={i.id}>
                      <td><div className="cell-name" style={{ fontWeight: 500 }}><Dot s={i.status} /><span className="mono" style={{ fontSize: 12 }}>{i.id}</span></div></td>
                      <td className="cell-sub">{i.node}</td>
                      <td className="num">{i.cpu}%</td>
                      <td className="num">{i.mem}%</td>
                      <td className="num">{i.restarts}</td>
                      <td><Badge s={i.status} /></td>
                    </tr>
                  ))}
                </tbody>
              </Table>
            )}
          </Card>
        )}

        {tab === "logs" && (
          <div className="fadein">
            <p style={{ fontSize: 12.5, color: "var(--text-3)", margin: "0 0 14px", lineHeight: 1.6 }}>
              Open <b>Logs &amp; Exec</b> for the live stream.{" "}
              <span style={{ color: "var(--accent-text)", cursor: "pointer" }} onClick={() => { onClose(); go("logs", svc); }}>Stream {svc.name} logs →</span>
            </p>
          </div>
        )}

        {tab === "networking" && (
          <div className="fadein">
            <KeyValue>
              <dt>Policy</dt><dd>{svc.policy}</dd>
              <dt>Exposed ports</dt><dd>{svc.ports.length ? svc.ports.join("  ") : "none (internal)"}</dd>
              <dt>Cluster DNS</dt><dd>{svc.name}.{svc.ns}.rune.local</dd>
            </KeyValue>
            <div className="divider" />
            <div className="eyebrow" style={{ marginBottom: 12 }}>Connections</div>
            <div style={{ display: "flex", flexDirection: "column", gap: 8 }}>
              {insts.slice(0, 4).map((i) => (
                <div key={i.id} style={{ display: "flex", alignItems: "center", gap: 10, fontSize: 12.5 }}>
                  <Icon name="link" size={14} style={{ color: "var(--net)" }} />
                  <span className="mono" style={{ color: "var(--text-2)" }}>{i.ip}</span>
                  <span style={{ color: "var(--text-4)" }}>→</span>
                  <span className="mono" style={{ color: "var(--text-2)" }}>{svc.ports[0] || "internal"}</span>
                </div>
              ))}
            </div>
          </div>
        )}
      </div>
    </Drawer>
  );
}
