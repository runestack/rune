import { useState } from "react";
import { Alert, Badge, Button, Card, Drawer, Icon, KeyValue, Tag, UsageBar, useConfirm } from "../components";
import type { Instance, Service } from "../api/types";
import { useServices } from "../api/hooks";
import { clients } from "../api/transport";

/**
 * Dedicated drawer for a single instance — its own CPU/memory, status, and a
 * link back to the parent service. Distinct from the service drawer (clicking
 * an instance no longer reuses the service view).
 */
export function InstanceDrawer({ inst, onClose, go, openSvc }: { inst: Instance; onClose: () => void; go: (r: string, arg?: Service) => void; openSvc: (s: Service) => void }) {
  const { data: services } = useServices();
  const svc = services.find((s) => s.name === inst.svc && s.ns === inst.ns);
  const live = inst.status === "run" || inst.status === "deploy";
  const failed = inst.status === "fail";

  const [busy, setBusy] = useState(false);
  const [actionErr, setActionErr] = useState<string | null>(null);
  const [done, setDone] = useState(false);
  const confirm = useConfirm();

  const openParent = () => { if (svc) { onClose(); openSvc(svc); } };
  const toLogs = () => { onClose(); go("logs", svc); };

  const onRestart = async () => {
    const ok = await confirm({
      title: `Restart instance?`,
      message: <>Instance <b>{inst.id}</b> on <b>{inst.node}</b> will be replaced.</>,
      icon: "refresh",
      confirmLabel: "Restart",
    });
    if (!ok) return;
    setBusy(true);
    setActionErr(null);
    try {
      await clients.instances.restartInstance({ id: inst.id });
      setDone(true);
    } catch (e) {
      setActionErr(e instanceof Error ? e.message : String(e));
    } finally {
      setBusy(false);
    }
  };

  return (
    <Drawer onClose={onClose}>
      <div className="drawer-head">
        <div className="eyebrow" style={{ marginBottom: 8 }}>
          {inst.ns} / {svc ? <span className="inst-link" onClick={openParent}>{inst.svc}</span> : inst.svc} / instance
        </div>
        <h2 className="inst-title" style={{ marginBottom: 10 }}>{inst.id}</h2>
        <div style={{ display: "flex", alignItems: "center", gap: 10, marginBottom: 14, flexWrap: "wrap" }}>
          <Badge s={inst.status} />
          <Tag>{inst.node}</Tag>
        </div>
        <div style={{ display: "flex", gap: 8, flexWrap: "wrap" }}>
          <Button size="sm" variant="primary" onClick={onRestart} disabled={busy} loading={busy}><Icon name="refresh" size={14} />Restart</Button>
          <Button size="sm" onClick={toLogs}><Icon name="logs" size={14} />Logs</Button>
          <Button size="sm" onClick={toLogs}><Icon name="terminal" size={14} />Exec</Button>
        </div>
        {actionErr && <Alert tone="error" style={{ marginTop: 12 }}>{actionErr}</Alert>}
        {done && !actionErr && <Alert tone="success" icon="check" style={{ marginTop: 12 }}>Restart requested — the scheduler will replace this instance.</Alert>}
        {failed && !actionErr && (
          <Alert tone="error" icon="health" style={{ marginTop: 14 }}>Failing readiness probe — restarted {inst.restarts}×</Alert>
        )}
      </div>
      <div className="drawer-body">
        <div className="fadein">
          <div className="grid g-2" style={{ marginBottom: 20 }}>
            <Card pad>
              <div className="eyebrow" style={{ marginBottom: 12 }}>CPU</div>
              {live ? <UsageBar v={inst.cpu} w={130} /> : <div className="metric-empty">not running</div>}
            </Card>
            <Card pad>
              <div className="eyebrow" style={{ marginBottom: 12 }}>Memory</div>
              {live ? <UsageBar v={inst.mem} w={130} /> : <div className="metric-empty">not running</div>}
            </Card>
          </div>
          <KeyValue>
            <dt>Status</dt><dd><Badge s={inst.status} /></dd>
            <dt>Service</dt><dd>{svc ? <span className="inst-link" onClick={openParent}>{inst.svc}</span> : inst.svc}</dd>
            <dt>Namespace</dt><dd>{inst.ns}</dd>
            <dt>Node</dt><dd className="mono">{inst.node}</dd>
            <dt>IP</dt><dd className="mono">{inst.ip}</dd>
            <dt>Image</dt><dd style={{ wordBreak: "break-all" }}>{svc?.image ?? "—"}</dd>
            <dt>Restarts</dt><dd style={{ color: inst.restarts > 3 ? "var(--deploy)" : undefined }}>{inst.restarts}</dd>
            <dt>Uptime</dt><dd>{inst.uptime}</dd>
          </KeyValue>
        </div>
      </div>
    </Drawer>
  );
}
