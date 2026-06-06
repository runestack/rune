import {
  Badge, Button, Card, CardHead, Dot, EmptyState, Feed, Icon, Kpi, KpiRow, PageHead,
  Replicas, Spark, Spinner, Table, Tag, UsageBar,
} from "../components";
import { useAuditFeed, useOverview } from "../api/hooks";
import { useObserveOverview } from "../api/observe";
import type { Bucket } from "../api/observe";
import { useScope } from "../lib/scope";
import type { Service } from "../api/types";
import "./runesight/RuneSight.css";

export function Overview({ go, openSvc }: { go: (r: string, arg?: Service) => void; openSvc: (s: Service) => void }) {
  const { data, loading, error, reload } = useOverview();
  const { data: feed } = useAuditFeed(6);
  const { data: observe } = useObserveOverview("1h");
  const { ns: scopeNs } = useScope();
  const T = data.totals;

  // The namespace scope filters the logical inventory (services & their
  // replicas). Physical capacity — nodes, CPU, memory — is cluster-wide and
  // stays unscoped (a namespace doesn't own hardware), so those are labelled.
  const scoped = scopeNs === "all" ? data.services : data.services.filter((s) => s.ns === scopeNs);
  const svcCount = scoped.length;
  const healthyCount = scoped.filter((s) => s.status === "run").length;
  const runInst = scoped.reduce((a, s) => a + s.ready, 0);
  const wantInst = scoped.reduce((a, s) => a + s.want, 0);

  // Pick a few representative services to preview (prefer those needing attention).
  const topSvc = [...scoped]
    .sort((a, b) => (a.status === "run" ? 1 : 0) - (b.status === "run" ? 1 : 0))
    .slice(0, 7);

  if (loading) {
    return (
      <div className="wrap">
        <PageHead eyebrow="Loading…" title="Overview" sub="Fetching live cluster health and inventory." />
        <Spinner label="Loading cluster overview…" height={320} />
      </div>
    );
  }
  if (error) {
    return (
      <div className="wrap">
        <PageHead eyebrow="Overview" title="Overview" sub="Live cluster health and inventory." />
        <EmptyState icon="overview" tone="error" title="Couldn't load the cluster overview" hint={error} action={{ label: "Retry", onClick: reload }} />
      </div>
    );
  }

  return (
    <div className="wrap">
      <PageHead
        eyebrow={`${data.cluster.name} · ${data.cluster.context} · ${data.cluster.version}`}
        title="Cluster <em>overview</em>"
        sub={scopeNs === "all"
          ? `${T.healthy} of ${T.services} services running, ${T.runningInstances} instances live across ${T.namespaces} namespace${T.namespaces === 1 ? "" : "s"}.`
          : `${scopeNs} · ${healthyCount} of ${svcCount} services running, ${runInst} of ${wantInst} replicas live.`}
        actions={
          <>
            <Button size="sm" onClick={reload}><Icon name="refresh" size={14} />Sync</Button>
            <Button size="sm" variant="primary"><Icon name="bolt" size={14} />rune cast</Button>
          </>
        }
      />

      {/* cluster summary — Legend-style KPI row */}
      <Card style={{ marginBottom: 16 }}>
        <CardHead actions={<>{scopeNs !== "all" && <Tag>scope · {scopeNs}</Tag>}<Tag>{data.cluster.uptime === "—" ? "live" : `up ${data.cluster.uptime}`}</Tag></>}>Cluster summary</CardHead>
        <KpiRow>
          <Kpi
            hero
            label={<><span className="kpi-ico"><Icon name="health" size={15} /></span>Health<Badge s={healthyCount === svcCount ? "run" : "warn"}>{healthyCount === svcCount ? "Operational" : "Attention"}</Badge></>}
            value={<>{healthyCount}<small>/ {svcCount}</small></>}
            sub={`${Math.max(0, svcCount - healthyCount)} services need attention`}
          />
          <Kpi
            label={<><span className="kpi-ico"><Icon name="instances" size={15} /></span>{scopeNs === "all" ? "Instances" : "Replicas"}</>}
            value={scopeNs === "all" ? <>{T.runningInstances}<small>/ {T.instances}</small></> : <>{runInst}<small>/ {wantInst}</small></>}
            sub={scopeNs === "all" ? `live across ${data.nodes.length} node${data.nodes.length === 1 ? "" : "s"}` : `ready in ${scopeNs}`}
          />
          <Kpi
            label={<><span className="kpi-ico"><Icon name="cpu" size={15} /></span>CPU</>}
            value={<>{T.cpu}<small>%</small></>}
            sub={scopeNs === "all" ? T.cpuCores : `${T.cpuCores} · cluster`}
          />
          <Kpi
            label={<><span className="kpi-ico"><Icon name="mem" size={15} /></span>Memory</>}
            value={<>{T.mem}<small>%</small></>}
            sub={scopeNs === "all" ? T.memGi : `${T.memGi} · cluster`}
          />
        </KpiRow>
      </Card>

      {/* services + nodes */}
      <div className="grid g-2-1" style={{ marginBottom: 16 }}>
        <Card>
          <CardHead actions={<Button size="sm" variant="ghost" onClick={() => go("services")}>All services <Icon name="chevron" size={13} /></Button>}>
            Services at a glance
          </CardHead>
          {topSvc.length === 0 ? (
            <EmptyState icon="services" title="No services" hint={scopeNs === "all" ? "Run rune cast to deploy a workload." : `No services in the ${scopeNs} namespace.`} />
          ) : (
            <Table>
              <thead><tr><th>Service</th><th>Status</th><th>Replicas</th><th>CPU</th></tr></thead>
              <tbody>
                {topSvc.map((s) => (
                  <tr key={`${s.ns}/${s.name}`} onClick={() => openSvc(s)}>
                    <td><div className="cell-name"><Dot s={s.status} pulse={s.status === "deploy"} />{s.name}</div></td>
                    <td><Badge s={s.status} /></td>
                    <td><Replicas ready={s.ready} want={s.want} /></td>
                    <td><UsageBar v={s.cpu} w={56} /></td>
                  </tr>
                ))}
              </tbody>
            </Table>
          )}
        </Card>

        <Card>
          <CardHead actions={<Tag>{data.nodes.length} ready</Tag>}>Nodes</CardHead>
          <div style={{ padding: "6px 20px 16px" }}>
            {data.nodes.length === 0 ? (
              <div className="empty" style={{ padding: "30px 10px" }}>No node health reported.</div>
            ) : data.nodes.map((n) => (
              <div key={n.name} style={{ padding: "13px 0", borderBottom: "1px solid var(--border-faint)" }}>
                <div style={{ display: "flex", alignItems: "center", gap: 9, marginBottom: 9 }}>
                  <Dot s={n.status === "ready" ? "run" : "warn"} />
                  <span style={{ fontWeight: 600, fontSize: 13, whiteSpace: "nowrap" }}>{n.name}</span>
                  <Tag>{n.role}</Tag>
                  <span style={{ marginLeft: "auto", color: "var(--text-3)", fontSize: 11.5 }} className="mono">{n.instances} inst</span>
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

      {/* observability — RuneSight error volume (core, not a plugin) */}
      {observe.enabled && (
        <Card style={{ marginBottom: 16 }}>
          <CardHead actions={<Button size="sm" variant="ghost" onClick={() => go("runesight")}>Open RuneSight <Icon name="chevron" size={13} /></Button>}>
            Log activity · last 1h
          </CardHead>
          <div style={{ padding: "6px 20px 18px" }}>
            <RuneSightSpark buckets={observe.buckets} />
            <div className="rs-ov-axis"><span>1h ago</span><span>now</span></div>
            <div className="rs-ov-stats" style={{ marginTop: 14 }}>
              <div className="rs-ov-stat">
                <div className={"rs-ov-num" + (observe.errors > 0 ? " error" : "")}>{observe.errors.toLocaleString()}</div>
                <div className="rs-ov-lbl">errors</div>
              </div>
              <div className="rs-ov-stat">
                <div className={"rs-ov-num" + (observe.warns > 0 ? " warn" : "")}>{observe.warns.toLocaleString()}</div>
                <div className="rs-ov-lbl">warnings</div>
              </div>
              <div className="rs-ov-stat">
                <div className="rs-ov-num">{observe.total.toLocaleString()}</div>
                <div className="rs-ov-lbl">lines</div>
              </div>
            </div>
          </div>
        </Card>
      )}

      {/* activity + usage */}
      <div className="grid g-2-1">
        <Card>
          <CardHead actions={<Icon name="bolt" size={15} style={{ color: "var(--text-3)" }} />}>Activity</CardHead>
          {feed.length === 0 ? (
            <div className="empty" style={{ padding: "40px 20px" }}>No recent audit activity.</div>
          ) : (
            <Feed events={feed.slice(0, 6)} />
          )}
        </Card>

        <Card pad>
          <div className="eyebrow" style={{ marginBottom: 4 }}>Cluster CPU</div>
          <div style={{ display: "flex", alignItems: "baseline", gap: 8, marginBottom: 12 }}>
            <span style={{ fontFamily: "var(--serif)", fontSize: 30, letterSpacing: "-0.02em" }}>{T.cpu}%</span>
            <span style={{ color: "var(--run)", fontSize: 12, fontWeight: 600 }}><Icon name="arrowup" size={12} /> live</span>
          </div>
          <Spark data={data.cpuHistory} />
          <div className="divider" />
          <div className="eyebrow" style={{ marginBottom: 4 }}>Cluster memory</div>
          <div style={{ display: "flex", alignItems: "baseline", gap: 8, marginBottom: 12 }}>
            <span style={{ fontFamily: "var(--serif)", fontSize: 30, letterSpacing: "-0.02em" }}>{T.mem}%</span>
          </div>
          <Spark data={data.memHistory} />
        </Card>
      </div>
    </div>
  );
}

/** Stacked level histogram for the Overview RuneSight card (info/warn/error). */
function RuneSightSpark({ buckets }: { buckets: Bucket[] }) {
  const sorted = buckets.map((b) => b.total).filter((x) => x > 0).sort((a, b) => b - a);
  const max = Math.max(1, sorted[Math.floor(sorted.length * 0.08)] || sorted[0] || 1);
  return (
    <div className="rs-ov-spark">
      {buckets.map((b, i) => (
        <div key={i} className="rs-ov-bar">
          {b.total === 0 ? <span className="rs-ov-empty" /> : (
            <>
              {b.info + b.debug > 0 && <span className="rs-ov-seg info" style={{ height: `${Math.min(100, ((b.info + b.debug) / max) * 100)}%` }} />}
              {b.warn > 0 && <span className="rs-ov-seg warn" style={{ height: `${Math.min(100, (b.warn / max) * 100)}%` }} />}
              {b.error > 0 && <span className="rs-ov-seg error" style={{ height: `${Math.min(100, (b.error / max) * 100)}%` }} />}
            </>
          )}
        </div>
      ))}
    </div>
  );
}
