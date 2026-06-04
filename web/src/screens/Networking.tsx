import { Fragment } from "react";
import { Badge, Button, Card, CardHead, Icon, PageHead, Table, Tag } from "../components";
import type { IconName } from "../components";
import { RUNE } from "../mock/data";

const PATH: { n: string; i: IconName; c: string }[] = [
  { n: "internet", i: "external", c: "var(--text-3)" },
  { n: "ingress", i: "network", c: "var(--text-2)" },
  { n: "web-gateway", i: "services", c: "var(--accent-text)" },
  { n: "api-core", i: "services", c: "var(--accent-text)" },
  { n: "postgres-primary", i: "storage", c: "var(--text-2)" },
];

export function Networking() {
  return (
    <div className="wrap">
      <PageHead
        eyebrow={`${RUNE.policies.length} policies`}
        title="Networking"
        sub="Service discovery and network policy. A baseline deny-all is layered with explicit allow rules between services."
        actions={<Button size="sm" variant="primary"><Icon name="plus" size={14} />New policy</Button>}
      />
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
      <Card style={{ overflow: "hidden" }}>
        <CardHead>Network policies</CardHead>
        <Table>
          <thead><tr><th>Policy</th><th>Namespace</th><th>Direction</th><th>Applies to</th><th>Rule</th><th>Mode</th></tr></thead>
          <tbody>
            {RUNE.policies.map((p) => (
              <tr key={p.name}>
                <td><div className="cell-name mono" style={{ fontSize: 13 }}><Icon name={p.mode === "deny" ? "shield" : "network"} size={15} style={{ color: p.mode === "deny" ? "var(--fail)" : "var(--net)" }} />{p.name}</div></td>
                <td><Tag>{p.ns}</Tag></td>
                <td className="cell-sub">{p.direction}</td>
                <td style={{ color: "var(--text-2)" }}>{p.targets}</td>
                <td className="cell-sub">{p.from ? `${p.from} · ${p.ports}` : "—"}</td>
                <td><Badge s={p.mode === "deny" ? "fail" : "net"}>{p.mode}</Badge></td>
              </tr>
            ))}
          </tbody>
        </Table>
      </Card>
    </div>
  );
}
