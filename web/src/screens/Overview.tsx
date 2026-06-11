import {
  Badge, Button, Card, CardHead, Dot, EmptyState, Feed, Icon, Kpi, KpiRow, PageHead,
  Replicas, Spark, Spinner, Table, Tag, UsageBar,
} from "../components";
import { useAuditFeed, useOverview } from "../api/hooks";
import { useObserveOverview } from "../api/observe";
import type { Bucket } from "../api/observe";
import { useScope } from "../lib/scope";
import type { Service } from "../api/types";
import { useEffect, useState } from "react";
import { listAlertRules } from "../api/alerting";
import { fmtBytes, getStats, topStreams } from "../api/sources";
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

      {/* observability — RuneSight "Sight" card (core, not a plugin) */}
      {observe.enabled && <SightCard buckets={observe.buckets} total={observe.total} go={go} />}

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

/**
 * Home "Sight" card — log persistence & alerts summary. All live:
 * INGEST (ln/min + last hour) and the VOLUME histogram come from
 * ObserveService, FIRING from the alerting RPCs, RETENTION from the
 * store stats RPC and SOURCES from a 24h stream count query.
 */
function SightCard({ buckets, total, go }: { buckets: Bucket[]; total: number; go: (r: string) => void }) {
  const perMin = Math.round(total / 60);
  const [firing, setFiring] = useState<string[]>([]); // firing rule names
  const [retention, setRetention] = useState<{ days: number; storage: string } | null>(null);
  const [sources, setSources] = useState<number | null>(null);
  useEffect(() => {
    let on = true;
    listAlertRules()
      .then(({ statuses }) => { if (on) setFiring(statuses.filter((s) => s.state === "firing").map((s) => s.rule)); })
      .catch(() => { /* observe unreachable — the card just shows 0 firing */ });
    getStats()
      .then((s) => {
        if (!on) return;
        const storage = s.supported && s.diskCapBytes > 0
          ? `${Math.round((s.diskUsedBytes / s.diskCapBytes) * 100)}% of ${fmtBytes(s.diskCapBytes)}`
          : s.supported ? `${fmtBytes(s.diskUsedBytes)} used` : `${s.backend} backend`;
        setRetention({ days: s.retentionDays, storage });
      })
      .catch(() => { /* stats unreachable — keep the "—" placeholder */ });
    topStreams()
      .then((rows) => { if (on) setSources(rows.length); })
      .catch(() => { /* keep placeholder */ });
    return () => { on = false; };
  }, []);
  const sorted = buckets.map((b) => b.total).filter((x) => x > 0).sort((a, b) => b - a);
  const max = Math.max(1, sorted[Math.floor(sorted.length * 0.08)] || sorted[0] || 1);
  const fmt = (t: number) => new Date(t).toISOString().slice(11, 16);
  return (
    <Card className="sight-card" style={{ marginBottom: 16 }}>
      <CardHead
        actions={
          <div className="sight-act">
            {firing.length > 0 && <span className="sight-firing"><Icon name="alert" size={12} />{firing.length} firing</span>}
            <Button size="sm" variant="ghost" onClick={() => go("rs-explore")}>Open in Sight <Icon name="external" size={13} /></Button>
          </div>
        }
      >
        <span className="sight-badge"><Icon name="search" size={13} /></span>
        Sight
        <span className="sight-sub">Log persistence &amp; alerts</span>
      </CardHead>
      <div className="sight-grid">
        <div className="sight-kpis">
          <div className="sight-kpi">
            <div className="sk-label">Ingest</div>
            <div className="sk-val">{perMin.toLocaleString()}<small>ln/min</small></div>
            <div className="sk-sub">last hour {total.toLocaleString()}</div>
          </div>
          <div className="sight-kpi">
            <div className="sk-label">Retention</div>
            <div className="sk-val">{retention && retention.days > 0 ? <>{retention.days}<small>d</small></> : "—"}</div>
            <div className="sk-sub">{retention ? retention.storage : "store"}</div>
          </div>
          <div className="sight-kpi">
            <div className="sk-label">Sources</div>
            <div className="sk-val">{sources ?? "—"}</div>
            <div className="sk-sub">services streaming</div>
          </div>
          <div className="sight-kpi">
            <div className="sk-label">Firing</div>
            <div className={"sk-val" + (firing.length ? " hot" : "")}>{firing.length}</div>
            <div className="sk-sub">{firing.length ? firing[0] : "all clear"}</div>
          </div>
        </div>
        <div className="sight-vol">
          <div className="sight-vol-head">
            <span className="eyebrow">Volume · last hour</span>
            <span className="sight-vol-legend"><i className="sv-i" />info<i className="sv-w" />warn<i className="sv-e" />error</span>
          </div>
          <div className="sight-hist">
            {buckets.map((b, i) => (
              <div className="sv-bar" key={i} title={`${fmt(b.t0)} · ${b.total} lines`}>
                {b.error > 0 && <span className="sv-seg e" style={{ height: `${(b.error / max) * 100}%` }} />}
                {b.warn > 0 && <span className="sv-seg w" style={{ height: `${(b.warn / max) * 100}%` }} />}
                {(b.info + b.debug) > 0 && <span className="sv-seg i" style={{ height: `${((b.info + b.debug) / max) * 100}%` }} />}
              </div>
            ))}
          </div>
          <div className="sight-vol-axis"><span>{buckets[0] ? fmt(buckets[0].t0) : "1h ago"}</span><span>now</span></div>
        </div>
      </div>
    </Card>
  );
}
