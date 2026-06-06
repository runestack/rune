import { useState } from "react";
import { EmptyState, PageHead, Spinner, Tabs } from "../../components";
import { useCapabilities, emptyQuery } from "../../api/observe";
import type { LogQuery, Range } from "../../api/observe";
import { Explorer } from "./Explorer";
import "./RuneSight.css";

type RsTab = "explore" | "views" | "alerts" | "ingestion";

const TABS: { id: RsTab; label: string }[] = [
  { id: "explore", label: "Log Explorer" },
  { id: "views", label: "Saved Views" },
  { id: "alerts", label: "Alert Rules" },
  { id: "ingestion", label: "Ingestion" },
];

/** Placeholder for the stubbed RuneSight surfaces (Saved Views / Alerts / Ingestion). */
function ComingSoon({ eyebrow, title, sub }: { eyebrow: string; title: string; sub: string }) {
  return (
    <div className="wrap">
      <PageHead eyebrow={eyebrow} title={title} sub={sub} />
      <EmptyState icon="logs" title="Coming soon" hint="This RuneSight surface lands in the next slice. The Log Explorer is live now." />
    </div>
  );
}

export function RuneSight() {
  const { data: caps, loading, error, reload } = useCapabilities();
  const [tab, setTab] = useState<RsTab>("explore");
  const [query, setQuery] = useState<LogQuery>(emptyQuery);
  const [range, setRange] = useState<Range>("1h");
  const [live, setLive] = useState(false);

  if (loading) {
    return (
      <div className="wrap">
        <PageHead eyebrow="rune sight · observability" title="RuneSight" sub="Connecting to the observability store." />
        <Spinner label="Checking observability capabilities…" height={320} />
      </div>
    );
  }

  // Capability handshake: observability disabled -> guide the operator to enable it.
  if (!caps.enabled) {
    return (
      <div className="wrap">
        <PageHead
          eyebrow="rune sight · observability"
          title="RuneSight"
          sub="Native log search, histograms and log-based alerting — once observability is enabled."
        />
        <EmptyState
          icon="logs"
          tone="muted"
          title="Observability is disabled"
          hint={`Enable the [observability] feature on runed to turn on log ingestion and search. ${error ? `(${error})` : ""}`}
          action={{ label: "Retry handshake", onClick: reload }}
        />
      </div>
    );
  }

  return (
    <div className="rs-explorer">
      <div style={{ padding: "16px 22px 0" }}>
        <Tabs tabs={TABS} active={tab} onChange={setTab} />
      </div>
      {tab === "explore" && (
        <Explorer query={query} setQuery={setQuery} range={range} setRange={setRange} live={live} setLive={setLive} />
      )}
      {tab === "views" && (
        <ComingSoon eyebrow="rune sight · saved queries" title="Saved <em>Views</em>" sub="Reusable LogQL queries your team has saved." />
      )}
      {tab === "alerts" && (
        <ComingSoon eyebrow="rune sight · log-based alerting" title="Alert <em>Rules</em>" sub="Rules evaluate a LogQL query on a rolling window and fire when the condition is met." />
      )}
      {tab === "ingestion" && (
        <ComingSoon eyebrow="rune logs · pipeline health" title="Ingestion <em>&amp;</em> Retention" sub="rune-agent tails every instance and ships to the RuneSight store." />
      )}
    </div>
  );
}
