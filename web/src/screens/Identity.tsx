import { useState } from "react";
import { Avatar, Badge, Button, Card, EmptyState, Icon, PageHead, Spinner, Table, Tabs, Tag } from "../components";
import { usePrincipals, useRoles } from "../api/hooks";

export function Identity() {
  const [tab, setTab] = useState("principals");
  const { data: principals, loading: pLoading, error: pError, reload: pReload } = usePrincipals();
  const { data: roles, loading: rLoading, error: rError, reload: rReload } = useRoles();

  return (
    <div className="wrap">
      <PageHead
        eyebrow={`${principals.length} principals · ${roles.length} roles`}
        title="Identity <em>&</em> RBAC"
        sub="Users and service accounts, the roles bound to them, and what each can do across namespaces."
        actions={<Button size="sm" variant="primary"><Icon name="plus" size={14} />Invite / token</Button>}
      />
      <Tabs
        tabs={[
          { id: "principals", label: `Principals (${principals.length})` },
          { id: "roles", label: `Roles (${roles.length})` },
        ]}
        active={tab}
        onChange={setTab}
      />
      {tab === "principals" && (
        pLoading ? (
          <Spinner label="Loading principals…" />
        ) : pError ? (
          <EmptyState icon="identity" tone="error" title="Couldn't load principals" hint={pError} action={{ label: "Retry", onClick: pReload }} />
        ) : principals.length === 0 ? (
          <EmptyState icon="identity" title="No principals" hint="Create a user or token to grant cluster access." />
        ) : (
          <Card className="fadein" style={{ overflow: "hidden" }}>
            <Table>
              <thead><tr><th>Principal</th><th>Type</th><th>Role</th><th>MFA</th><th>Last active</th></tr></thead>
              <tbody>
                {principals.map((p) => (
                  <tr key={`${p.type}/${p.name}`}>
                    <td>
                      <div className="cell-name">
                        <Avatar name={p.name} type={p.type === "machine" ? "machine" : "user"} />
                        {p.name}
                      </div>
                      <div className="cell-sub" style={{ marginLeft: 36 }}>{p.email}</div>
                    </td>
                    <td><Tag>{p.kind}</Tag></td>
                    <td><Badge s="accent">{p.role}</Badge></td>
                    <td>{p.mfa ? <Badge s="run">on</Badge> : <span className="tag" style={{ color: "var(--deploy)" }}>off</span>}</td>
                    <td className="cell-sub" style={{ color: p.last === "active now" ? "var(--run)" : "var(--text-3)" }}>{p.last}</td>
                  </tr>
                ))}
              </tbody>
            </Table>
          </Card>
        )
      )}
      {tab === "roles" && (
        rLoading ? (
          <Spinner label="Loading roles…" />
        ) : rError ? (
          <EmptyState icon="shield" tone="error" title="Couldn't load roles" hint={rError} action={{ label: "Retry", onClick: rReload }} />
        ) : roles.length === 0 ? (
          <EmptyState icon="shield" title="No roles" />
        ) : (
          <Card className="fadein" style={{ overflow: "hidden" }}>
            <Table>
              <thead><tr><th>Role</th><th>Scope</th><th>Verbs</th><th>Subjects</th><th>Source</th></tr></thead>
              <tbody>
                {roles.map((r) => (
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
        )
      )}
    </div>
  );
}
