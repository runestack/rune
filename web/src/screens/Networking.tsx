import { Fragment } from "react";
import { Badge, Button, Card, CardHead, EmptyState, Icon, PageHead, Spinner, Table, Tag } from "../components";
import type { IconName } from "../components";
import { usePolicies } from "../api/hooks";
import { useDemo } from "../api/demo";

const PATH: { n: string; i: IconName; c: string }[] = [
  { n: "internet", i: "external", c: "var(--text-3)" },
  { n: "ingress", i: "network", c: "var(--text-2)" },
  { n: "web-gateway", i: "services", c: "var(--accent-text)" },
  { n: "api-core", i: "services", c: "var(--accent-text)" },
  { n: "postgres-primary", i: "storage", c: "var(--text-2)" },
];

export function Networking() {
  const { data: policies, loading, error, reload } = usePolicies();
  const demo = useDemo();

  return (
    <div className="wrap">
      <PageHead
        eyebrow={`${policies.length} policies`}
        title="Networking"
        sub="Service discovery and authorization policy. A baseline deny-all is layered with explicit allow rules across the cluster."
        actions={<Button size="sm" variant="primary"><Icon name="plus" size={14} />New policy</Button>}
      />
      {demo && (
        <Card pad style={{ marginBottom: 18 }}>
          <div className="eyebrow" style={{ marginBottom: 16 }}>Traffic path · production</div>
          <div style={{ display: "flex", alignItems: "center", gap: 0, flexWrap: "wrap", justifyContent: "space-between" }}>
            {PATH.map((node, idx, arr) => (
              <Fragment key={node.n}>
                <div style={{ display: "flex", flexDirection: "column", alignItems: "center", gap: 8, minWidth: 90 }}>
                  <div style={{ width: 44, height: 44, borderRadius: 11, border: "1px solid var(--border-strong)", background: "var(--surface-2)", display: "grid", placeItems: "center", color: node.c }}>
                    <Icon name={node.i} size={20} />
                  </div>
                  <span className="mono" style={{ fontSize: 11, color: "var(--text-2)", textAlign: "center" }}>{node.n}</span>
                </div>
                {idx < arr.length - 1 && (
                  <div style={{ flex: 1, height: 1, background: "repeating-linear-gradient(90deg, var(--border-strong) 0 5px, transparent 5px 10px)", minWidth: 24, position: "relative", top: -10 }} />
                )}
              </Fragment>
            ))}
          </div>
        </Card>
      )}
      <Card style={{ overflow: "hidden" }}>
        <CardHead>Network policies</CardHead>
        {loading ? (
          <Spinner label="Loading policies…" height={160} />
        ) : error ? (
          <EmptyState icon="network" tone="error" title="Couldn't load policies" hint={error} action={{ label: "Retry", onClick: reload }} />
        ) : policies.length === 0 ? (
          <EmptyState icon="network" title="No policies" hint="Authorization policies define what each principal can do across namespaces." />
        ) : (
          <Table>
            <thead><tr><th>Policy</th><th>Namespace</th><th>Direction</th><th>Applies to</th><th>Rule</th><th>Mode</th></tr></thead>
            <tbody>
              {policies.map((p) => (
                <tr key={p.name}>
                  <td><div className="cell-name mono" style={{ fontSize: 13 }}><Icon name={p.mode === "deny" ? "shield" : "network"} size={15} style={{ color: p.mode === "deny" ? "var(--fail)" : "var(--net)" }} />{p.name}</div></td>
                  <td><Tag>{p.ns}</Tag></td>
                  <td className="cell-sub">{p.direction}</td>
                  <td style={{ color: "var(--text-2)" }}>{p.targets}</td>
                  <td className="cell-sub">{p.from ? `${p.from} · ${p.ports}` : p.desc || "—"}</td>
                  <td><Badge s={p.mode === "deny" ? "fail" : "net"}>{p.mode}</Badge></td>
                </tr>
              ))}
            </tbody>
          </Table>
        )}
      </Card>
    </div>
  );
}
