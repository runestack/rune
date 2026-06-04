import { Button, Card, Icon, PageHead, Tag, UsageBar } from "../components";
import { RUNE } from "../mock/data";

export function Namespaces({ go }: { go: (r: string) => void }) {
  return (
    <div className="wrap">
      <PageHead
        eyebrow={`${RUNE.namespaces.length} namespaces`}
        title="Namespaces"
        sub="Logical partitions that scope services, secrets, and network policy. Resource quotas roll up per namespace."
        actions={<Button size="sm" variant="primary"><Icon name="plus" size={14} />New namespace</Button>}
      />
      <div className="grid g-2">
        {RUNE.namespaces.map((n) => (
          <Card key={n.name} pad style={{ cursor: "pointer" }} className="ns-card">
            <div onClick={() => go("services")}>
              <div style={{ display: "flex", alignItems: "center", gap: 10, marginBottom: 14 }}>
                <span style={{ color: "var(--accent-text)" }}><Icon name="namespaces" size={18} /></span>
                <span style={{ fontFamily: "var(--serif)", fontSize: 19, fontWeight: 500 }}>{n.name}</span>
                {n.services === 0 ? <Tag>empty</Tag> : <span className="badge accent" style={{ marginLeft: "auto" }}>{n.services} services</span>}
              </div>
              <div style={{ display: "flex", gap: 28, marginBottom: 16 }}>
                <div><div className="eyebrow" style={{ marginBottom: 4 }}>Instances</div><span className="serif" style={{ fontSize: 22 }}>{n.instances}</span></div>
                <div style={{ flex: 1 }}><div className="eyebrow" style={{ marginBottom: 6 }}>CPU</div><UsageBar v={n.cpu} w="100%" /></div>
                <div style={{ flex: 1 }}><div className="eyebrow" style={{ marginBottom: 6 }}>Memory</div><UsageBar v={n.mem} w="100%" /></div>
              </div>
              <div style={{ display: "flex", gap: 6, flexWrap: "wrap", alignItems: "center" }}>
                {n.labels.length ? n.labels.map((l) => <Tag key={l}>{l}</Tag>) : <span style={{ color: "var(--text-4)", fontSize: 12 }}>no labels</span>}
                <span style={{ marginLeft: "auto", color: "var(--text-3)", fontSize: 11.5 }} className="mono">{n.age}</span>
              </div>
            </div>
          </Card>
        ))}
      </div>
    </div>
  );
}
