export interface ReplicasProps {
  ready: number;
  want: number;
}

export function Replicas({ ready, want }: ReplicasProps) {
  const ok = ready === want;
  return (
    <span className="mono tnum" style={{ fontSize: 12.5, color: ok ? "var(--text)" : "var(--deploy)", fontWeight: 600 }}>
      {ready}
      <span style={{ color: "var(--text-4)" }}>/{want}</span>
    </span>
  );
}
