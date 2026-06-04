import { useState } from "react";
import { Badge, Button, Card, Dot, Dropdown, Icon, PageHead, Replicas, Segmented, Table, Tag, UsageBar } from "../components";
import { RUNE } from "../mock/data";
import type { Service } from "../mock/data";

export function Services({ openSvc }: { openSvc: (s: Service) => void }) {
  const [filter, setFilter] = useState("all");
  const [ns, setNs] = useState("all");
  const list = RUNE.services.filter(
    (s) =>
      (filter === "all" || (filter === "attention" ? s.status === "warn" || s.status === "fail" : s.status === filter)) &&
      (ns === "all" || s.ns === ns),
  );

  return (
    <div className="wrap">
      <PageHead
        eyebrow={`${RUNE.totals.services} services · ${RUNE.totals.namespaces} namespaces`}
        title="Services"
        sub="Every deployed workload across the cluster — containers and process runners packaged from runesets."
        actions={<Button size="sm" variant="primary"><Icon name="plus" size={14} />Deploy service</Button>}
      />
      <div style={{ display: "flex", gap: 12, marginBottom: 16, alignItems: "center" }}>
        <Segmented
          options={[
            { value: "all", label: "All" },
            { value: "run", label: "Running" },
            { value: "attention", label: "Attention" },
            { value: "deploy", label: "Deploying" },
          ]}
          value={filter}
          onChange={setFilter}
        />
        <Dropdown width={250} label={<span className="dd-lab"><span className="eyebrow">namespace</span><b>{ns === "all" ? "All namespaces" : ns}</b></span>}>
          {(close) => (
            <div className="dd-list">
              <div className={`dd-item${ns === "all" ? " sel" : ""}`} onClick={() => { setNs("all"); close(); }}>
                <Icon name="namespaces" size={14} /><span>All namespaces</span><span className="tag ddi-sub">{RUNE.services.length}</span>
              </div>
              <div className="dd-sep" />
              {RUNE.namespaces.filter((n) => n.services > 0).map((n) => (
                <div key={n.name} className={`dd-item${ns === n.name ? " sel" : ""}`} onClick={() => { setNs(n.name); close(); }}>
                  <Dot s="run" /><span>{n.name}</span><span className="tag ddi-sub">{n.services}</span>
                </div>
              ))}
            </div>
          )}
        </Dropdown>
        <span style={{ marginLeft: "auto", color: "var(--text-3)", fontSize: 12.5 }} className="mono">{list.length} shown</span>
      </div>

      <Card style={{ overflow: "hidden" }}>
        <Table>
          <thead><tr><th>Service</th><th>Namespace</th><th>Status</th><th>Replicas</th><th>CPU</th><th>Memory</th><th>Restarts</th><th>Age</th></tr></thead>
          <tbody>
            {list.map((s) => (
              <tr key={s.name} onClick={() => openSvc(s)}>
                <td>
                  <div className="cell-name"><Dot s={s.status} pulse={s.status === "deploy"} />{s.name}</div>
                  <div className="cell-sub" style={{ marginLeft: 18 }}>{s.image.split("/").pop()}</div>
                </td>
                <td><Tag>{s.ns}</Tag></td>
                <td><Badge s={s.status} /></td>
                <td><Replicas ready={s.ready} want={s.want} /></td>
                <td><UsageBar v={s.cpu} w={54} /></td>
                <td><UsageBar v={s.mem} w={54} /></td>
                <td className="num" style={{ color: s.restarts > 3 ? "var(--deploy)" : "var(--text-2)" }}>{s.restarts}</td>
                <td className="num" style={{ color: "var(--text-3)" }}>{s.age}</td>
              </tr>
            ))}
          </tbody>
        </Table>
      </Card>
    </div>
  );
}
