import {
  Badge, Button, Card, CardHead, Dot, Feed, Icon, Kpi, KpiRow, PageHead,
  Replicas, Spark, Table, Tag, UsageBar,
} from "../components";
import { RUNE } from "../mock/data";
import type { Service } from "../mock/data";

export function Overview({ go, openSvc }: { go: (r: string, arg?: Service) => void; openSvc: (s: Service) => void }) {
  const T = RUNE.totals;
  const topSvc = RUNE.services.filter((s) => s.ns === "production").slice(0, 7);

  return (
    <div className="wrap">
      <PageHead
        eyebrow={`${RUNE.cluster.name} · ${RUNE.cluster.context} · up ${RUNE.cluster.uptime}`}
        title="Good afternoon, <em>Ore</em>"
        sub={`Your cluster is healthy. ${T.healthy} of ${T.services} services running, ${T.runningInstances} instances live across ${T.namespaces} namespaces.`}
        actions={
          <>
            <Button size="sm"><Icon name="refresh" size={14} />Sync</Button>
            <Button size="sm" variant="primary"><Icon name="bolt" size={14} />rune cast</Button>
          </>
        }
      />

      {/* cluster summary — Legend-style KPI row */}
      <Card style={{ marginBottom: 16 }}>
        <CardHead actions={<Tag>last synced 30s ago</Tag>}>Cluster summary</CardHead>
        <KpiRow>
          <Kpi
            hero
            label={<><span className="kpi-ico"><Icon name="health" size={15} /></span>Health<Badge s="run">Operational</Badge></>}
            value={<>{T.healthy}<small>/ {T.services}</small></>}
            sub={`${T.services - T.healthy} services need attention`}
          />
          <Kpi
            label={<><span className="kpi-ico"><Icon name="instances" size={15} /></span>Instances</>}
            value={<>{T.runningInstances}<small>/ {T.instances}</small></>}
            sub={`live across ${RUNE.nodes.length} nodes`}
          />
          <Kpi
            label={<><span className="kpi-ico"><Icon name="cpu" size={15} /></span>CPU</>}
            value={<>{T.cpu}<small>%</small></>}
            sub={T.cpuCores}
          />
          <Kpi
            label={<><span className="kpi-ico"><Icon name="mem" size={15} /></span>Memory</>}
            value={<>{T.mem}<small>%</small></>}
            sub={T.memGi}
          />
        </KpiRow>
      </Card>

      {/* services + nodes */}
      <div className="grid g-2-1" style={{ marginBottom: 16 }}>
        <Card>
          <CardHead actions={<Button size="sm" variant="ghost" onClick={() => go("services")}>All services <Icon name="chevron" size={13} /></Button>}>
            Services at a glance
          </CardHead>
          <Table>
            <thead><tr><th>Service</th><th>Status</th><th>Replicas</th><th>CPU</th></tr></thead>
            <tbody>
              {topSvc.map((s) => (
                <tr key={s.name} onClick={() => openSvc(s)}>
                  <td><div className="cell-name"><Dot s={s.status} pulse={s.status === "deploy"} />{s.name}</div></td>
                  <td><Badge s={s.status} /></td>
                  <td><Replicas ready={s.ready} want={s.want} /></td>
                  <td><UsageBar v={s.cpu} w={56} /></td>
                </tr>
              ))}
            </tbody>
          </Table>
        </Card>

        <Card>
          <CardHead actions={<Tag>{RUNE.nodes.length} ready</Tag>}>Nodes</CardHead>
          <div style={{ padding: "6px 20px 16px" }}>
            {RUNE.nodes.map((n) => (
              <div key={n.name} style={{ padding: "13px 0", borderBottom: "1px solid var(--border-faint)" }}>
                <div style={{ display: "flex", alignItems: "center", gap: 9, marginBottom: 9 }}>
                  <Dot s="run" />
                  <span style={{ fontWeight: 600, fontSize: 13, whiteSpace: "nowrap" }}>{n.name}</span>
                  <Tag>{n.role}</Tag>
                </div>
                <div style={{ display: "flex", gap: 18 }}>
                  <div style={{ flex: 1 }}><div className="eyebrow" style={{ marginBottom: 5, fontSize: 9.5 }}>CPU</div><UsageBar v={n.cpu} w="100%" /></div>
                  <div style={{ flex: 1 }}><div className="eyebrow" style={{ marginBottom: 5, fontSize: 9.5 }}>MEM</div><UsageBar v={n.mem} w="100%" /></div>
                </div>
              </div>
            ))}
          </div>
        </Card>
      </div>

      {/* activity + usage */}
      <div className="grid g-2-1">
        <Card>
          <CardHead actions={<Icon name="bolt" size={15} style={{ color: "var(--text-3)" }} />}>Activity</CardHead>
          <Feed events={RUNE.events.slice(0, 6)} />
        </Card>

        <Card pad>
          <div className="eyebrow" style={{ marginBottom: 4 }}>Cluster CPU · 24h</div>
          <div style={{ display: "flex", alignItems: "baseline", gap: 8, marginBottom: 12 }}>
            <span style={{ fontFamily: "var(--serif)", fontSize: 30, letterSpacing: "-0.02em" }}>{RUNE.totals.cpu}%</span>
            <span style={{ color: "var(--run)", fontSize: 12, fontWeight: 600 }}><Icon name="arrowup" size={12} /> stable</span>
          </div>
          <Spark data={RUNE.cpuHistory} />
          <div className="divider" />
          <div className="eyebrow" style={{ marginBottom: 4 }}>Cluster memory · 24h</div>
          <div style={{ display: "flex", alignItems: "baseline", gap: 8, marginBottom: 12 }}>
            <span style={{ fontFamily: "var(--serif)", fontSize: 30, letterSpacing: "-0.02em" }}>{RUNE.totals.mem}%</span>
          </div>
          <Spark data={RUNE.memHistory} />
        </Card>
      </div>
    </div>
  );
}
