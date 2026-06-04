import "./Spinner.css";

/**
 * Spinner — a calm, low-contrast loading state. Used by screens while live
 * cluster data is in flight. Matches the muted `.empty` palette.
 */
export function Spinner({ label = "Loading…", height }: { label?: string; height?: number }) {
  return (
    <div className="spinner-wrap" style={height ? { minHeight: height } : undefined}>
      <span className="spinner-ring" aria-hidden />
      <span className="spinner-label">{label}</span>
    </div>
  );
}
