import { Badge, Button, Card, CardHead, EmptyState, Icon, PageHead, Spinner, Table, Tag, UsageBar } from "../components";
import { useStorageClasses, useVolumes } from "../api/hooks";
import { useScope } from "../lib/scope";

export function Storage() {
  const { data: volumes, loading: vLoading, error: vError, reload: vReload } = useVolumes();
  const { data: classes, loading: cLoading, error: cError, reload: cReload } = useStorageClasses();
  const { ns: scopeNs } = useScope();
  // Volumes are namespaced; storage classes are cluster-scoped (left unfiltered).
  const shownVolumes = scopeNs === "all" ? volumes : volumes.filter((v) => v.ns === scopeNs);

  return (
    <div className="wrap">
      <PageHead
        eyebrow={`${shownVolumes.length} volumes · ${classes.length} storage classes`}
        title="Storage"
        sub="Persistent volumes backing stateful services, and the storage classes that provision them."
        actions={<Button size="sm" variant="primary"><Icon name="plus" size={14} />New volume</Button>}
      />
      <Card style={{ overflow: "hidden", marginBottom: 24 }}>
        <CardHead>Volumes</CardHead>
        {vLoading ? (
          <Spinner label="Loading volumes…" height={160} />
        ) : vError ? (
          <EmptyState icon="storage" tone="error" title="Couldn't load volumes" hint={vError} action={{ label: "Retry", onClick: vReload }} />
        ) : shownVolumes.length === 0 ? (
          <EmptyState icon="storage" title="No volumes" hint={scopeNs === "all" ? "Cast a volume spec or a stateful service to provision persistent storage." : `No volumes in the ${scopeNs} namespace.`} />
        ) : (
          <Table>
            <thead><tr><th>Volume</th><th>Namespace</th><th>Bound to</th><th>Capacity</th><th>Used</th><th>Class</th><th>Mode</th><th>Status</th></tr></thead>
            <tbody>
              {shownVolumes.map((v) => (
                <tr key={`${v.ns}/${v.name}`}>
                  <td><div className="cell-name"><Icon name="storage" size={15} style={{ color: "var(--text-3)" }} />{v.name}</div></td>
                  <td><Tag>{v.ns}</Tag></td>
                  <td className="cell-sub">{v.svc}</td>
                  <td className="num">{v.size}</td>
                  <td style={{ width: 150 }}><UsageBar v={v.used} w={90} /></td>
                  <td className="num" style={{ color: "var(--text-3)" }}>{v.class}</td>
                  <td><Tag>{v.mode}</Tag></td>
                  <td><Badge s={v.status === "bound" ? "run" : v.status === "failed" ? "fail" : "idle"}>{v.status}</Badge></td>
                </tr>
              ))}
            </tbody>
          </Table>
        )}
      </Card>
      <Card style={{ overflow: "hidden" }}>
        <CardHead>Storage classes</CardHead>
        {cLoading ? (
          <Spinner label="Loading storage classes…" height={120} />
        ) : cError ? (
          <EmptyState icon="cube" tone="error" title="Couldn't load storage classes" hint={cError} action={{ label: "Retry", onClick: cReload }} />
        ) : classes.length === 0 ? (
          <EmptyState icon="cube" title="No storage classes" />
        ) : (
          <Table>
            <thead><tr><th>Class</th><th>Provisioner</th><th>Reclaim policy</th><th>Expandable</th><th>Volumes</th></tr></thead>
            <tbody>
              {classes.map((c) => (
                <tr key={c.name}>
                  <td><div className="cell-name"><Icon name="cube" size={15} style={{ color: "var(--text-3)" }} />{c.name}{c.isDefault && <span className="badge accent">default</span>}</div></td>
                  <td className="cell-sub">{c.provisioner}</td>
                  <td><Tag>{c.reclaim}</Tag></td>
                  <td className="num">{c.expand ? "yes" : "no"}</td>
                  <td className="num">{c.volumes}</td>
                </tr>
              ))}
            </tbody>
          </Table>
        )}
      </Card>
    </div>
  );
}
