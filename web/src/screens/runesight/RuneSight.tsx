import { EmptyState, PageHead, Spinner } from "../../components";
import { useCapabilities } from "../../api/observe";
import type { LogQuery, Range } from "../../api/observe";
import { Explorer } from "./Explorer";
import { SavedViews } from "./SavedViews";
import { Alerts } from "./Alerts";
import { Sources } from "./Sources";
import "./RuneSight.css";

export type RsTab = "explore" | "views" | "alerts" | "sources";

export interface RuneSightProps {
  tab: RsTab;
  go: (tab: RsTab) => void;
  query: LogQuery;
  setQuery: (q: LogQuery | ((q: LogQuery) => LogQuery)) => void;
  range: Range;
  setRange: (r: Range) => void;
  live: boolean;
  setLive: (fn: (l: boolean) => boolean) => void;
  loadView: (q: Partial<LogQuery>) => void;
}

export function RuneSight({ tab, go, query, setQuery, range, setRange, live, setLive, loadView }: RuneSightProps) {
  const { data: caps, loading, error, reload } = useCapabilities();

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

  if (tab === "views") return <SavedViews loadView={loadView} go={() => go("explore")} />;
  if (tab === "alerts") return <Alerts loadView={loadView} />;
  if (tab === "sources") return <Sources />;

  return (
    <Explorer
      query={query}
      setQuery={setQuery}
      range={range}
      setRange={setRange}
      live={live}
      setLive={setLive}
      go={go}
      loadView={loadView}
    />
  );
}
