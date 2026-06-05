import { useState } from "react";
import { Badge, Card, Dot, Dropdown, EmptyState, Icon, PageHead, Spinner, Table } from "../components";
import { useInstances } from "../api/hooks";
import { useScope } from "../lib/scope";
import type { Instance } from "../api/types";

export function Instances({ openInst }: { openInst: (i: Instance) => void }) {
  const { data: instances, loading, error, reload } = useInstances();
  const { ns: scopeNs } = useScope();
  const [node, setNode] = useState("all");

  // Namespace scope is the base filter; node is a sub-filter within it.
  const scoped = scopeNs === "all" ? instances : instances.filter((i) => i.ns === scopeNs);

  // node list derived from the scoped instances
  const nodeCounts = new Map<string, number>();
  for (const i of scoped) nodeCounts.set(i.node, (nodeCounts.get(i.node) ?? 0) + 1);
  const nodes = [...nodeCounts.keys()].filter((n) => n && n !== "—").sort();

  const list = scoped.filter((i) => node === "all" || i.node === node);
  const running = scoped.filter((i) => i.status === "run").length;

  return (
    <div className="wrap">
      <PageHead
        eyebrow={`${running} running · ${scoped.length} total`}
        title="Instances"
        sub="Individual running copies of each service, scheduled across the cluster's nodes."
      />
      <div style={{ display: "flex", gap: 12, marginBottom: 16, alignItems: "center" }}>
        <Dropdown width={260} label={<span className="dd-lab"><span className="eyebrow">node</span><b>{node === "all" ? "All nodes" : node}</b></span>}>
          {(close) => (
            <div className="dd-list">
              <div className={`dd-item${node === "all" ? " sel" : ""}`} onClick={() => { setNode("all"); close(); }}>
                <Icon name="instances" size={14} /><span>All nodes</span><span className="tag ddi-sub">{scoped.length}</span>
              </div>
              <div className="dd-sep" />
              {nodes.map((n) => (
                <div key={n} className={`dd-item${node === n ? " sel" : ""}`} onClick={() => { setNode(n); close(); }}>
                  <Dot s="run" /><span className="mono" style={{ fontSize: 12 }}>{n}</span><span className="tag ddi-sub">{nodeCounts.get(n)}</span>
                </div>
              ))}
            </div>
          )}
        </Dropdown>
        <span style={{ marginLeft: "auto", color: "var(--text-3)", fontSize: 12.5 }} className="mono">{list.length} instances</span>
      </div>
      {loading ? (
        <Spinner label="Loading instances…" />
      ) : error ? (
        <EmptyState icon="instances" tone="error" title="Couldn't load instances" hint={error} action={{ label: "Retry", onClick: reload }} />
      ) : scoped.length === 0 ? (
        <EmptyState icon="instances" title="No instances running" hint={scopeNs === "all" ? "Instances appear once a deployed service schedules its replicas." : `No instances in the ${scopeNs} namespace.`} />
      ) : (
        <Card style={{ overflow: "hidden" }}>
          <Table>
            <thead><tr><th>Instance</th><th>Service</th><th>Node</th><th>IP</th><th>CPU</th><th>Mem</th><th>Restarts</th><th>Uptime</th><th>Status</th></tr></thead>
            <tbody>
              {list.map((i) => {
                return (
                  <tr key={i.id} onClick={() => openInst(i)}>
                    <td><div className="cell-name" style={{ fontWeight: 500 }}><Dot s={i.status} pulse={i.status === "deploy"} /><span className="mono" style={{ fontSize: 12 }}>{i.id}</span></div></td>
                    <td>{i.svc}</td>
                    <td className="cell-sub">{i.node}</td>
                    <td className="num" style={{ color: "var(--text-3)" }}>{i.ip}</td>
                    <td className="num">{i.cpu}%</td>
                    <td className="num">{i.mem}%</td>
                    <td className="num" style={{ color: i.restarts > 3 ? "var(--deploy)" : "var(--text-2)" }}>{i.restarts}</td>
                    <td className="num" style={{ color: "var(--text-3)" }}>{i.uptime}</td>
                    <td><Badge s={i.status} /></td>
                  </tr>
                );
              })}
            </tbody>
          </Table>
        </Card>
      )}
    </div>
  );
}
