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
  const label = children ?? (s !== "accent" ? STATUS_LABEL[s] : null);
  return (
    <span className={`badge ${s}`}>
      <span className={`dot ${s}`} style={{ width: 6, height: 6, boxShadow: "none" }} />
      {label}
    </span>
  );
}
