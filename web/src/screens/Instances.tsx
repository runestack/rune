import { useState } from "react";
import { Badge, Card, Dot, Dropdown, Icon, PageHead, Table } from "../components";
import { RUNE } from "../mock/data";
import type { Service } from "../mock/data";

export function Instances({ openSvc }: { openSvc: (s: Service) => void }) {
  const [node, setNode] = useState("all");
  const list = RUNE.instances.filter((i) => node === "all" || i.node === node);
  return (
    <div className="wrap">
      <PageHead
        eyebrow={`${RUNE.totals.runningInstances} running · ${RUNE.totals.instances} total`}
        title="Instances"
        sub="Individual running copies of each service, scheduled across the cluster's nodes."
      />
      <div style={{ display: "flex", gap: 12, marginBottom: 16, alignItems: "center" }}>
        <Dropdown width={260} label={<span className="dd-lab"><span className="eyebrow">node</span><b>{node === "all" ? "All nodes" : node}</b></span>}>
          {(close) => (
            <div className="dd-list">
              <div className={`dd-item${node === "all" ? " sel" : ""}`} onClick={() => { setNode("all"); close(); }}>
                <Icon name="instances" size={14} /><span>All nodes</span><span className="tag ddi-sub">{RUNE.instances.length}</span>
              </div>
              <div className="dd-sep" />
              {RUNE.nodes.map((n) => (
                <div key={n.name} className={`dd-item${node === n.name ? " sel" : ""}`} onClick={() => { setNode(n.name); close(); }}>
                  <Dot s="run" /><span className="mono" style={{ fontSize: 12 }}>{n.name}</span><span className="tag ddi-sub">{n.role}</span>
                </div>
              ))}
            </div>
          )}
        </Dropdown>
        <span style={{ marginLeft: "auto", color: "var(--text-3)", fontSize: 12.5 }} className="mono">{list.length} instances</span>
      </div>
      <Card style={{ overflow: "hidden" }}>
        <Table>
          <thead><tr><th>Instance</th><th>Service</th><th>Node</th><th>IP</th><th>CPU</th><th>Mem</th><th>Restarts</th><th>Uptime</th><th>Status</th></tr></thead>
          <tbody>
            {list.map((i) => {
              const svc = RUNE.services.find((s) => s.name === i.svc);
              return (
                <tr key={i.id} onClick={() => svc && openSvc(svc)}>
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
    </div>
  );
}
