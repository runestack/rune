import { useState } from "react";
import { Alert, Badge, Button, Card, Dot, Drawer, Icon, KeyValue, Replicas, Spinner, Table, Tabs, Tag, UsageBar, useConfirm } from "../components";
import type { Service } from "../api/types";
import { useServiceInstances } from "../api/hooks";
import { clients } from "../api/transport";
import { ScalingMode } from "../gen/pkg/api/proto/service_pb";

export function ServiceDrawer({ svc, onClose, go }: { svc: Service; onClose: () => void; go: (r: string, arg?: Service) => void }) {
  const [tab, setTab] = useState("overview");
  const { data: insts, loading: instLoading, reload: reloadInsts } = useServiceInstances(svc.name, svc.ns);
  const [busy, setBusy] = useState<string | null>(null);
  const [actionErr, setActionErr] = useState<string | null>(null);
  const confirm = useConfirm();

  // Run an action with busy/error bookkeeping. `busy` doubles as the label of
  // the in-flight action so the matching button shows its spinner.
  async function perform(label: string, fn: () => Promise<void>) {
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

  const onScale = async () => {
    const res = await confirm({
      title: `Scale ${svc.name}`,
      message: <>Currently <b>{svc.want}</b> desired · namespace <b>{svc.ns}</b>.</>,
      icon: "scale",
      confirmLabel: "Scale",
      input: {
        label: "Target instances",
        type: "number",
        defaultValue: String(svc.want),
        validate: (v) => {
          const n = Number(v);
          if (v.trim() === "" || Number.isNaN(n)) return "Enter a number";
          if (!Number.isInteger(n) || n < 0) return "Whole number ≥ 0";
          return null;
        },
      },
    });
    if (res === false) return;
    const scale = parseInt(res as string, 10);
    await perform("Scale", async () => {
      await clients.services.scaleService({ name: svc.name, namespace: svc.ns, scale, mode: ScalingMode.IMMEDIATE });
    });
  };

  const onRestart = async () => {
    const ok = await confirm({
      title: `Restart ${svc.name}?`,
      message: <>All instances in <b>{svc.ns}</b> roll. Non-HA services may be briefly unavailable.</>,
      icon: "refresh",
      confirmLabel: "Restart",
    });
    if (!ok) return;
    // Restart = scale to 0 then back to desired count (mirrors `rune restart`).
    await perform("Restart", async () => {
      await clients.services.scaleService({ name: svc.name, namespace: svc.ns, scale: 0, mode: ScalingMode.IMMEDIATE });
      await clients.services.scaleService({ name: svc.name, namespace: svc.ns, scale: svc.want || 1, mode: ScalingMode.IMMEDIATE });
    });
  };

  const onDelete = async () => {
    const ok = await confirm({
      title: `Delete ${svc.name}?`,
      message: <>This permanently removes the service and its instances from <b>{svc.ns}</b>. This can't be undone.</>,
      tone: "danger",
      icon: "alert",
      confirmLabel: "Delete service",
    });
    if (!ok) return;
    await perform("Delete", async () => {
      await clients.services.deleteService({ name: svc.name, namespace: svc.ns });
      onClose();
    });
  };

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
          <Button size="sm" variant="primary" onClick={onScale} disabled={!!busy} loading={busy === "Scale"}><Icon name="scale" size={14} />Scale</Button>
          <Button size="sm" onClick={onRestart} disabled={!!busy} loading={busy === "Restart"}><Icon name="refresh" size={14} />Restart</Button>
          <Button size="sm" onClick={() => { onClose(); go("logs", svc); }}><Icon name="logs" size={14} />Logs</Button>
          <Button size="sm" onClick={() => { onClose(); go("logs", svc); }}><Icon name="terminal" size={14} />Exec</Button>
          <Button size="sm" variant="danger" onClick={onDelete} disabled={!!busy} loading={busy === "Delete"}><Icon name="close" size={14} />Delete</Button>
        </div>
        {busy && <div style={{ marginTop: 12, fontSize: 12.5, color: "var(--text-3)", display: "flex", gap: 8, alignItems: "center" }}><Dot s="deploy" pulse />{busy}…</div>}
        {actionErr && <Alert tone="error" style={{ marginTop: 12 }}>{actionErr}</Alert>}
        {svc.note && !actionErr && (
          <Alert tone={svc.status === "warn" ? "error" : "warn"} icon="health" style={{ marginTop: 14 }}>{svc.note}</Alert>
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
