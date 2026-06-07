import { Card, CardHead, Icon, PageHead, Table, Tag } from "../../components";
import type { LogQuery } from "../../api/observe";
import { ALERTS, CHANNELS, savedViewLogQL } from "./mockData";

function renderChan(chan: string) {
  const i = chan.indexOf(":");
  const kind = chan.slice(0, i), v = chan.slice(i + 1);
  return (
    <span className="rs-chan">
      <span className={"rs-chan-k " + kind}>{kind}</span>
      <span className="rs-chan-c">:</span>
      <span className="rs-chan-v">{v}</span>
    </span>
  );
}

export function Alerts({ loadView }: { loadView: (q: Partial<LogQuery>) => void }) {
  const firing = ALERTS.filter((a) => a.status === "firing");
  const pending = ALERTS.filter((a) => a.status === "pending").length;
  const ok = ALERTS.filter((a) => a.status === "ok").length;

  return (
    <div className="wrap">
      <PageHead
        eyebrow="rune sight · log-based alerting"
        title="Alert <em>Rules</em>"
        sub="Rules evaluate a LogQL query on a rolling window and fire when the condition is met."
      />

      {firing.length > 0 && (
        <div className="rs-firing-banner">
          <Icon name="alert" size={18} />
          <span><b>{firing.length} alert{firing.length === 1 ? "" : "s"} firing</b> — {firing.map((a) => a.name).join(", ")}.</span>
        </div>
      )}

      <Card style={{ overflow: "hidden" }}>
        <CardHead
          actions={
            <div style={{ display: "flex", alignItems: "center", gap: 16, marginLeft: "auto" }}>
              <div className="rs-rule-sum">
                {firing.length > 0 && <span className="rs-rs firing"><i />{firing.length} firing</span>}
                {pending > 0 && <span className="rs-rs pending"><i />{pending} pending</span>}
                <span className="rs-rs ok"><i />{ok} healthy</span>
              </div>
              <span className="rs-eval-note">evaluated every 60s</span>
            </div>
          }
        >
          Rules
        </CardHead>
        <Table>
          <thead><tr><th>Rule</th><th>Query (LogQL)</th><th>Condition</th><th>Channel</th></tr></thead>
          <tbody>
            {ALERTS.map((a) => (
              <tr key={a.id} onClick={() => loadView(a.q)}>
                <td>
                  <div className="rs-rule-cell">
                    <span className={"rs-alert-dot " + a.status} />
                    <div className="rs-rule-id">
                      <span className="rs-rule-name">{a.name}</span>
                      {a.since && a.status === "firing" && <span className="rs-since-chip">since {a.since}</span>}
                    </div>
                  </div>
                </td>
                <td className="rs-alert-q">{savedViewLogQL(a.q)}</td>
                <td className="cell-sub">{a.cond}</td>
                <td>{renderChan(a.chan)}</td>
              </tr>
            ))}
          </tbody>
        </Table>
      </Card>

      <Card style={{ overflow: "hidden", marginTop: 16 }}>
        <CardHead>Notification channels</CardHead>
        <Table>
          <thead><tr><th>Channel</th><th>Type</th><th>State</th><th>Last delivery</th></tr></thead>
          <tbody>
            {CHANNELS.map((c) => (
              <tr key={c.id}>
                <td><div className="cell-name" style={{ fontWeight: 500 }}><span className={"rs-chan-ico " + c.kind} />{c.label}</div></td>
                <td><Tag>{c.kind}</Tag></td>
                <td><span className={"rs-chan-state " + c.state}><span className="rs-chan-sdot" />{c.state}</span></td>
                <td className="cell-sub">{c.last}</td>
              </tr>
            ))}
          </tbody>
        </Table>
      </Card>
    </div>
  );
}
