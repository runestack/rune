import type { ReactNode } from "react";
import { STATUS_LABEL, type StatusKey } from "../../../lib/status";
import "../Dot/Dot.css";
import "./Badge.css";

export interface BadgeProps {
  /** Status key (also a free "accent" variant). */
  s: StatusKey | "accent";
  children?: ReactNode;
}

export function Badge({ s, children }: BadgeProps) {
  // The "accent" variant is a label pill (no status dot); status variants
  // carry a small dot to read at a glance.
  if (s === "accent") return <span className="badge accent">{children}</span>;
  return (
    <span className={`badge ${s}`}>
      <span className={`dot ${s}`} style={{ width: 6, height: 6, boxShadow: "none" }} />
      {children ?? STATUS_LABEL[s]}
    </span>
  );
}
