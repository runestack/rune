import { Badge, Button, Card, CardHead, EmptyState, Icon, PageHead, Spinner, Table, Tag } from "../components";
import { usePolicies } from "../api/hooks";
import { useScope } from "../lib/scope";

export function Networking() {
  const { data: policies, loading, error, reload } = usePolicies();
  const { ns: scopeNs } = useScope();
  const shown = scopeNs === "all" ? policies : policies.filter((p) => p.ns === scopeNs);

  return (
    <div className="wrap">
      <PageHead
        eyebrow={`${shown.length} policies`}
        title="Networking"
        sub="Service discovery and authorization policy. A baseline deny-all is layered with explicit allow rules across the cluster."
        actions={<Button size="sm" variant="primary"><Icon name="plus" size={14} />New policy</Button>}
      />
      <Card style={{ overflow: "hidden" }}>
        <CardHead>Network policies</CardHead>
        {loading ? (
          <Spinner label="Loading policies…" height={160} />
        ) : error ? (
          <EmptyState icon="network" tone="error" title="Couldn't load policies" hint={error} action={{ label: "Retry", onClick: reload }} />
        ) : shown.length === 0 ? (
          <EmptyState icon="network" title="No policies" hint={scopeNs === "all" ? "Authorization policies define what each principal can do across namespaces." : `No network policies in the ${scopeNs} namespace.`} />
        ) : (
          <Table>
            <thead><tr><th>Policy</th><th>Namespace</th><th>Direction</th><th>Applies to</th><th>Rule</th><th>Mode</th></tr></thead>
            <tbody>
              {shown.map((p) => (
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
