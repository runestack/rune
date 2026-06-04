import { useState } from "react";
import { Badge, Button, Card, Icon, PageHead, Table, Tabs, Tag } from "../components";
import { RUNE } from "../mock/data";

export function Identity() {
  const [tab, setTab] = useState("principals");
  return (
    <div className="wrap">
      <PageHead
        eyebrow={`${RUNE.principals.length} principals · ${RUNE.roles.length} roles`}
        title="Identity <em>&</em> RBAC"
        sub="Users and service accounts, the roles bound to them, and what each can do across namespaces."
        actions={<Button size="sm" variant="primary"><Icon name="plus" size={14} />Invite / token</Button>}
      />
      <Tabs
        tabs={[
          { id: "principals", label: `Principals (${RUNE.principals.length})` },
          { id: "roles", label: `Roles (${RUNE.roles.length})` },
        ]}
        active={tab}
        onChange={setTab}
      />
      {tab === "principals" && (
        <Card className="fadein" style={{ overflow: "hidden" }}>
          <Table>
            <thead><tr><th>Principal</th><th>Type</th><th>Role</th><th>MFA</th><th>Last active</th></tr></thead>
            <tbody>
              {RUNE.principals.map((p) => (
                <tr key={p.name}>
                  <td>
                    <div className="cell-name">
                      <span style={{
                        width: 26, height: 26, borderRadius: p.type === "machine" ? 7 : "50%", flex: "none",
                        background: p.type === "machine" ? "var(--surface-3)" : "linear-gradient(135deg, var(--accent), #6f5be0)",
                        display: "grid", placeItems: "center", fontSize: 11, fontWeight: 700, color: p.type === "machine" ? "var(--text-2)" : "#15121f",
                      }}>
                        {p.type === "machine" ? <Icon name="terminal" size={13} /> : p.name[0].toUpperCase()}
                      </span>
                      {p.name}
                    </div>
                    <div className="cell-sub" style={{ marginLeft: 36 }}>{p.email}</div>
                  </td>
                  <td><Tag>{p.kind}</Tag></td>
                  <td><span className="badge accent">{p.role}</span></td>
                  <td>{p.mfa ? <Badge s="run">on</Badge> : <span className="tag" style={{ color: "var(--deploy)" }}>off</span>}</td>
                  <td className="cell-sub" style={{ color: p.last === "active now" ? "var(--run)" : "var(--text-3)" }}>{p.last}</td>
                </tr>
              ))}
            </tbody>
          </Table>
        </Card>
      )}
      {tab === "roles" && (
        <Card className="fadein" style={{ overflow: "hidden" }}>
          <Table>
            <thead><tr><th>Role</th><th>Scope</th><th>Verbs</th><th>Subjects</th><th>Source</th></tr></thead>
            <tbody>
              {RUNE.roles.map((r) => (
                <tr key={r.name}>
                  <td><div className="cell-name mono" style={{ fontSize: 13 }}><Icon name="shield" size={15} style={{ color: "var(--accent-text)" }} />{r.name}</div></td>
                  <td className="cell-sub">{r.scope}</td>
                  <td className="mono" style={{ fontSize: 11.5, color: "var(--text-2)" }}>{r.verbs}</td>
                  <td className="num">{r.subjects}</td>
                  <td><Tag>{r.builtin ? "built-in" : "custom"}</Tag></td>
                </tr>
              ))}
            </tbody>
          </Table>
        </Card>
      )}
    </div>
  );
}
