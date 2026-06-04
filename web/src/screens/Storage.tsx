import { Badge, Button, Card, CardHead, Icon, PageHead, Table, Tag, UsageBar } from "../components";
import { RUNE } from "../mock/data";

export function Storage() {
  return (
    <div className="wrap">
      <PageHead
        eyebrow={`${RUNE.volumes.length} volumes · ${RUNE.storageClasses.length} storage classes`}
        title="Storage"
        sub="Persistent volumes backing stateful services, and the storage classes that provision them."
        actions={<Button size="sm" variant="primary"><Icon name="plus" size={14} />New volume</Button>}
      />
      <Card style={{ overflow: "hidden", marginBottom: 24 }}>
        <CardHead>Volumes</CardHead>
        <Table>
          <thead><tr><th>Volume</th><th>Namespace</th><th>Bound to</th><th>Capacity</th><th>Used</th><th>Class</th><th>Mode</th><th>Status</th></tr></thead>
          <tbody>
            {RUNE.volumes.map((v) => (
              <tr key={v.name}>
                <td><div className="cell-name"><Icon name="storage" size={15} style={{ color: "var(--text-3)" }} />{v.name}</div></td>
                <td><Tag>{v.ns}</Tag></td>
                <td className="cell-sub">{v.svc}</td>
                <td className="num">{v.size}</td>
                <td style={{ width: 150 }}><UsageBar v={v.used} w={90} /></td>
                <td className="num" style={{ color: "var(--text-3)" }}>{v.class}</td>
                <td><Tag>{v.mode}</Tag></td>
                <td><Badge s={v.status === "bound" ? "run" : "idle"}>{v.status}</Badge></td>
              </tr>
            ))}
          </tbody>
        </Table>
      </Card>
      <Card style={{ overflow: "hidden" }}>
        <CardHead>Storage classes</CardHead>
        <Table>
          <thead><tr><th>Class</th><th>Provisioner</th><th>Reclaim policy</th><th>Expandable</th><th>Volumes</th></tr></thead>
          <tbody>
            {RUNE.storageClasses.map((c) => (
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
      </Card>
    </div>
  );
}
