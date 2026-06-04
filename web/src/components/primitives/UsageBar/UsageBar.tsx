import "./UsageBar.css";

export interface UsageBarProps {
  /** 0–100 */
  v: number;
  w?: number;
}

/** Neutral gray until it matters: amber past 75%, red past 88%. */
export function UsageBar({ v, w = 70 }: UsageBarProps) {
  const cls = v >= 88 ? "hot" : v >= 75 ? "warn" : "ok";
  return (
    <div style={{ display: "flex", alignItems: "center", gap: 9 }}>
      <div className={`ubar ${cls}`} style={{ width: w }}>
        <span style={{ width: `${v}%` }} />
      </div>
      <span className="mono tnum" style={{ fontSize: 11.5, color: "var(--text-2)", minWidth: 30 }}>
        {v}%
      </span>
    </div>
  );
}
