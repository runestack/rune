import { Card, CardHead, Dot, Icon, PageHead, Table, Tag } from "../../components";
import { INGESTION, SOURCES_COUNT } from "./mockData";

/* mocked agents + top streams — no ingestion backend seam yet */
const AGENTS = [
  { name: "do-nyc3-01", role: "control-plane", lps: 42, kbps: 310 },
  { name: "do-nyc3-02", role: "worker", lps: 31, kbps: 224 },
  { name: "do-nyc3-03", role: "worker", lps: 27, kbps: 196 },
];

const TOP_STREAMS = [
  { svc: "api-core", ns: "production", lines: 41200 },
  { svc: "web-gateway", ns: "production", lines: 31800 },
  { svc: "auth-service", ns: "production", lines: 18900 },
  { svc: "payments", ns: "production", lines: 17400 },
  { svc: "worker-queue", ns: "production", lines: 15600 },
  { svc: "search-indexer", ns: "staging", lines: 8200 },
];

export function Sources() {
  const total = TOP_STREAMS.reduce((a, s) => a + s.lines, 0);
  const linesToday = total + 12400;

  return (
    <div className="wrap">
      <PageHead
        eyebrow="rune sight · pipeline health"
        title="Log <em>Sources</em>"
        sub="rune-agent tails every instance and ships to the RuneSight store. History is retained and searchable."
      />

      <div className="grid" style={{ gridTemplateColumns: "repeat(4, 1fr)", gap: 16, marginBottom: 20 }}>
        <Card pad>
          <div className="eyebrow" style={{ marginBottom: 12 }}>Retention</div>
          <div className="sk-val">{INGESTION.retentionDays}<small>days</small></div>
          <div className="sk-sub" style={{ marginTop: 9 }}>rolling window</div>
        </Card>
        <Card pad>
          <div className="eyebrow" style={{ marginBottom: 12 }}>Ingested · 24h</div>
          <div className="sk-val">{INGESTION.gbToday}<small>GB</small></div>
          <div className="sk-sub" style={{ marginTop: 9 }}>{linesToday.toLocaleString()} lines</div>
        </Card>
        <Card pad>
          <div className="eyebrow" style={{ marginBottom: 12 }}>Sources</div>
          <div className="sk-val">{SOURCES_COUNT}</div>
          <div className="sk-sub" style={{ marginTop: 9 }}>services streaming</div>
        </Card>
        <Card pad>
          <div className="eyebrow" style={{ marginBottom: 12 }}>Dropped · 24h</div>
          <div className="sk-val">0<small>lines</small></div>
          <div className="sk-sub" style={{ marginTop: 9 }}>at-least-once delivery</div>
        </Card>
      </div>

      <div className="grid g-2-1" style={{ marginBottom: 20 }}>
        <Card pad>
          <CardHead>Store utilization</CardHead>
          <div style={{ height: 10, background: "var(--inset)", border: "1px solid var(--border)", borderRadius: 6, overflow: "hidden", margin: "14px 0 10px" }}>
            <div style={{ height: "100%", width: `${INGESTION.storagePct}%`, background: "linear-gradient(90deg, var(--accent), var(--accent-text))", borderRadius: 5 }} />
          </div>
          <div style={{ display: "flex", justifyContent: "space-between", alignItems: "baseline" }}>
            <span className="mono" style={{ fontSize: 12.5, color: "var(--text)" }}>{INGESTION.storageUsedGi} / {INGESTION.storageCapGi} GiB</span>
            <span className="tnum" style={{ fontSize: 13, color: "var(--text-2)", fontWeight: 600 }}>{INGESTION.storagePct}%</span>
          </div>
          <div className="mono" style={{ marginTop: 11, fontSize: 11, color: "var(--text-4)" }}>embedded-store · evicts oldest beyond {INGESTION.retentionDays}d</div>
        </Card>

        <Card pad>
          <CardHead>Agents</CardHead>
          <div style={{ marginTop: 8 }}>
            {AGENTS.map((n) => (
              <div key={n.name} style={{ display: "flex", alignItems: "center", gap: 10, padding: "9px 0", borderBottom: "1px solid var(--border-faint)" }}>
                <Dot s="run" />
                <span className="mono" style={{ fontSize: 12.5, color: "var(--text)" }}>{n.name}</span>
                <Tag>{n.role}</Tag>
                <span style={{ flex: 1 }} />
                <span className="mono tnum" style={{ fontSize: 12, color: "var(--text-2)", minWidth: 54, textAlign: "right" }}>{n.lps} l/s</span>
                <span className="mono tnum" style={{ fontSize: 11.5, color: "var(--text-3)", minWidth: 70, textAlign: "right" }}>{n.kbps} KB/s</span>
              </div>
            ))}
          </div>
        </Card>
      </div>

      <Card style={{ overflow: "hidden" }}>
        <CardHead>Top streams · 24h</CardHead>
        <Table>
          <thead><tr><th>Service</th><th>Namespace</th><th>Lines · 24h</th><th style={{ width: "32%" }}>Share</th></tr></thead>
          <tbody>
            {TOP_STREAMS.map((s) => {
              const pct = Math.round((s.lines / total) * 100);
              return (
                <tr key={s.svc}>
                  <td><div className="cell-name" style={{ fontWeight: 500 }}><Icon name="services" size={14} style={{ color: "var(--text-3)" }} />{s.svc}</div></td>
                  <td><Tag>{s.ns}</Tag></td>
                  <td className="num tnum">{s.lines.toLocaleString()}</td>
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
      </Card>
    </div>
  );
}
