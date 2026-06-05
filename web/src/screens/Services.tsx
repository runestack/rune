import { useState } from "react";
import { Badge, Button, Card, Dot, Dropdown, EmptyState, Icon, PageHead, Replicas, Segmented, Spinner, Table, Tag, UsageBar } from "../components";
import { useServices } from "../api/hooks";
import { useScope } from "../lib/scope";
import type { Service } from "../api/types";

export function Services({ openSvc }: { openSvc: (s: Service) => void }) {
  const { data: services, loading, error, reload } = useServices();
  const [filter, setFilter] = useState("all");
  const { ns, setNs } = useScope();

  // namespaces with their service counts, derived from the live list
  const nsCounts = new Map<string, number>();
  for (const s of services) nsCounts.set(s.ns, (nsCounts.get(s.ns) ?? 0) + 1);
  const nsList = [...nsCounts.keys()].sort();

  const list = services.filter(
    (s) =>
      (filter === "all" || (filter === "attention" ? s.status === "warn" || s.status === "fail" : s.status === filter)) &&
      (ns === "all" || s.ns === ns),
  );

  return (
    <div className="wrap">
      <PageHead
        eyebrow={`${services.length} services · ${nsList.length} namespaces`}
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
                <Icon name="namespaces" size={14} /><span>All namespaces</span><span className="tag ddi-sub">{services.length}</span>
              </div>
              <div className="dd-sep" />
              {nsList.map((n) => (
                <div key={n} className={`dd-item${ns === n ? " sel" : ""}`} onClick={() => { setNs(n); close(); }}>
                  <Dot s="run" /><span>{n}</span><span className="tag ddi-sub">{nsCounts.get(n)}</span>
                </div>
              ))}
            </div>
          )}
        </Dropdown>
        <span style={{ marginLeft: "auto", color: "var(--text-3)", fontSize: 12.5 }} className="mono">{list.length} shown</span>
      </div>

      {loading ? (
        <Spinner label="Loading services…" />
      ) : error ? (
        <EmptyState icon="services" tone="error" title="Couldn't load services" hint={error} action={{ label: "Retry", onClick: reload }} />
      ) : services.length === 0 ? (
        <EmptyState icon="services" title="No services deployed" hint="Run rune cast to deploy a workload into the cluster." />
      ) : (
        <Card style={{ overflow: "hidden" }}>
          <Table>
            <thead><tr><th>Service</th><th>Namespace</th><th>Status</th><th>Replicas</th><th>CPU</th><th>Memory</th><th>Restarts</th><th>Age</th></tr></thead>
            <tbody>
              {list.map((s) => (
                <tr key={`${s.ns}/${s.name}`} onClick={() => openSvc(s)}>
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
      )}
    </div>
  );
}
