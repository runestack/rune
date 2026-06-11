import { useEffect, useState } from "react";
import { Card, CardHead, Dot, Icon, PageHead, Table, Tag } from "../../components";
import { bytes24h, fmtBytes, fmtBytesParts, getStats, nodeRates, topStreams } from "../../api/sources";
import type { Bytes24h, NodeRate, StatsData, StreamRow } from "../../api/sources";
import { timeAgo } from "../../api/savedViews";
import type { LogQuery } from "../../api/observe";

const POLL_MS = 60_000;

function errMsg(e: unknown): string {
  return e instanceof Error ? e.message : String(e);
}

function PanelErr({ msg }: { msg: string }) {
  return <div className="rs-empty-s" style={{ color: "var(--fail)", padding: "10px 0", fontSize: 12 }}>{msg}</div>;
}

export function Sources({ loadView }: { loadView: (q: Partial<LogQuery>) => void }) {
  const [stats, setStats] = useState<StatsData | null>(null);
  const [streams, setStreams] = useState<StreamRow[] | null>(null);
  const [nodes, setNodes] = useState<NodeRate[] | null>(null);
  const [bytes, setBytes] = useState<Bytes24h | null>(null);
  const [errs, setErrs] = useState<{ stats?: string; streams?: string; nodes?: string; bytes?: string }>({});
  const [loading, setLoading] = useState(true);

  useEffect(() => {
    let on = true;
    const load = () => {
      Promise.allSettled([getStats(), topStreams(), nodeRates(), bytes24h()]).then(([st, ts, nr, by]) => {
        if (!on) return;
        const e: typeof errs = {};
        if (st.status === "fulfilled") setStats(st.value); else e.stats = errMsg(st.reason);
        if (ts.status === "fulfilled") setStreams(ts.value); else e.streams = errMsg(ts.reason);
        if (nr.status === "fulfilled") setNodes(nr.value); else e.nodes = errMsg(nr.reason);
        if (by.status === "fulfilled") setBytes(by.value); else e.bytes = errMsg(by.reason);
        setErrs(e);
        setLoading(false);
      });
    };
    load();
    const id = setInterval(load, POLL_MS);
    return () => { on = false; clearInterval(id); };
  }, []);

  const totalLines = (streams ?? []).reduce((a, s) => a + s.lines, 0);
  const bytesByService = new Map((bytes?.byService ?? []).map((b) => [b.service, b.bytes]));
  const ingested = fmtBytesParts(bytes?.total ?? 0);
  const capPct = stats && stats.supported && stats.diskCapBytes > 0
    ? Math.min(100, Math.round((stats.diskUsedBytes / stats.diskCapBytes) * 100))
    : null;

  return (
    <div className="wrap">
      <PageHead
        eyebrow="rune sight · pipeline health"
        title="Log <em>Sources</em>"
        sub="rune-agent tails every instance and ships to the RuneSight store. History is retained and searchable."
      />

      {!loading && (
      <>
      <div className="grid" style={{ gridTemplateColumns: "repeat(4, 1fr)", gap: 16, marginBottom: 20 }}>
        <Card pad>
          <div className="eyebrow" style={{ marginBottom: 12 }}>Retention</div>
          {errs.stats ? <PanelErr msg={errs.stats} /> : stats && stats.retentionDays > 0 ? (
            <>
              <div className="sk-val">{stats.retentionDays}<small>days</small></div>
              <div className="sk-sub" style={{ marginTop: 9 }}>rolling window</div>
            </>
          ) : (
            <>
              <div className="sk-val">—</div>
              <div className="sk-sub" style={{ marginTop: 9 }}>store default</div>
            </>
          )}
        </Card>
        <Card pad>
          <div className="eyebrow" style={{ marginBottom: 12 }}>Ingested · 24h</div>
          {errs.bytes ? <PanelErr msg={errs.bytes} /> : (
            <>
              <div className="sk-val">{ingested.v}<small>{ingested.u}</small></div>
              <div className="sk-sub" style={{ marginTop: 9 }}>{totalLines.toLocaleString()} lines</div>
            </>
          )}
        </Card>
        <Card pad>
          <div className="eyebrow" style={{ marginBottom: 12 }}>Sources</div>
          {errs.streams ? <PanelErr msg={errs.streams} /> : (
            <>
              <div className="sk-val">{(streams ?? []).length}</div>
              <div className="sk-sub" style={{ marginTop: 9 }}>services streaming · 24h</div>
            </>
          )}
        </Card>
        <Card pad>
          <div className="eyebrow" style={{ marginBottom: 12 }}>Oldest record</div>
          {errs.stats ? <PanelErr msg={errs.stats} /> : (
            <>
              <div className="sk-val">{stats?.oldestRecord ? timeAgo(stats.oldestRecord) : "—"}</div>
              <div className="sk-sub" style={{ marginTop: 9 }}>history depth</div>
            </>
          )}
        </Card>
      </div>

      <div className="grid g-2-1" style={{ marginBottom: 20 }}>
        <Card pad>
          <CardHead>Store utilization</CardHead>
          {errs.stats ? <PanelErr msg={errs.stats} /> : !stats ? null : !stats.supported ? (
            <div style={{ marginTop: 14, fontSize: 12.5, color: "var(--text-2)" }}>
              Storage managed by the <span className="mono">{stats.backend || "external"}</span> backend.
            </div>
          ) : capPct !== null ? (
            <>
              <div style={{ height: 10, background: "var(--inset)", border: "1px solid var(--border)", borderRadius: 6, overflow: "hidden", margin: "14px 0 10px" }}>
                <div style={{ height: "100%", width: `${capPct}%`, background: "linear-gradient(90deg, var(--accent), var(--accent-text))", borderRadius: 5 }} />
              </div>
              <div style={{ display: "flex", justifyContent: "space-between", alignItems: "baseline" }}>
                <span className="mono" style={{ fontSize: 12.5, color: "var(--text)" }}>{fmtBytes(stats.diskUsedBytes)} of {fmtBytes(stats.diskCapBytes)} · backend: {stats.backend}</span>
                <span className="tnum" style={{ fontSize: 13, color: "var(--text-2)", fontWeight: 600 }}>{capPct}%</span>
              </div>
            </>
          ) : (
            <div style={{ display: "flex", justifyContent: "space-between", alignItems: "baseline", marginTop: 14 }}>
              <span className="mono" style={{ fontSize: 12.5, color: "var(--text)" }}>{fmtBytes(stats.diskUsedBytes)} used · backend: {stats.backend}</span>
              <span className="tnum" style={{ fontSize: 13, color: "var(--text-2)", fontWeight: 600 }}>unbounded</span>
            </div>
          )}
          {stats && stats.supported && (
            <div className="mono" style={{ marginTop: 11, fontSize: 11, color: "var(--text-4)" }}>
              {stats.records.toLocaleString()} records{stats.retentionDays > 0 ? ` · evicts oldest beyond ${stats.retentionDays}d` : ""}
            </div>
          )}
        </Card>

        <Card pad>
          <CardHead>Agents</CardHead>
          {errs.nodes ? <PanelErr msg={errs.nodes} /> : (
            <div style={{ marginTop: 8 }}>
              {(nodes ?? []).length === 0 ? (
                <div className="rs-empty-s" style={{ padding: "12px 0", fontSize: 12 }}>No ingest in the last 5 minutes.</div>
              ) : (nodes ?? []).map((n) => (
                <div key={n.node} style={{ display: "flex", alignItems: "center", gap: 10, padding: "9px 0", borderBottom: "1px solid var(--border-faint)" }}>
                  <Dot s="run" />
                  <span className="mono" style={{ fontSize: 12.5, color: "var(--text)" }}>{n.node}</span>
                  <span style={{ flex: 1 }} />
                  <span className="mono tnum" style={{ fontSize: 12, color: "var(--text-2)", minWidth: 64, textAlign: "right" }}>{n.lps.toFixed(1)} l/s</span>
                </div>
              ))}
            </div>
          )}
        </Card>
      </div>

      <Card style={{ overflow: "hidden" }}>
        <CardHead>Top streams · 24h</CardHead>
        {errs.streams ? <div style={{ padding: "0 20px" }}><PanelErr msg={errs.streams} /></div> : (streams ?? []).length === 0 ? (
          <div className="empty" style={{ padding: "30px 20px" }}>No streams ingested in the last 24 hours.</div>
        ) : (
        <Table>
          <thead><tr><th>Service</th><th>Namespace</th><th>Lines · 24h</th>{bytesByService.size > 0 && <th>Bytes · 24h</th>}<th style={{ width: "28%" }}>Share</th></tr></thead>
          <tbody>
            {(streams ?? []).map((s) => {
              const pct = totalLines > 0 ? Math.round((s.lines / totalLines) * 100) : 0;
              const b = bytesByService.get(s.service);
              return (
                <tr key={`${s.namespace}/${s.service}`} onClick={() => loadView({ namespaces: s.namespace ? [s.namespace] : [], services: s.service ? [s.service] : [] })}>
                  <td><div className="cell-name" style={{ fontWeight: 500 }}><Icon name="services" size={14} style={{ color: "var(--text-3)" }} />{s.service || "—"}</div></td>
                  <td>{s.namespace ? <Tag>{s.namespace}</Tag> : <span style={{ color: "var(--text-4)" }}>—</span>}</td>
                  <td className="num tnum">{s.lines.toLocaleString()}</td>
                  {bytesByService.size > 0 && <td className="num tnum">{b !== undefined ? fmtBytes(b) : "—"}</td>}
                  <td>
                    <div style={{ display: "flex", alignItems: "center", gap: 10 }}>
                      <div className="rs-share-bar"><span style={{ width: `${pct}%` }} /></div>
                      <span className="mono tnum" style={{ fontSize: 11.5, color: "var(--text-3)" }}>{pct}%</span>
                    </div>
                  </td>
                </tr>
              );
            })}
          </tbody>
        </Table>
        )}
      </Card>
      </>
      )}
    </div>
  );
}
