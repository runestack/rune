import type { ReactNode } from "react";
import "./Tooltip.css";

export interface TooltipProps {
  label: ReactNode;
  placement?: "top" | "bottom";
  children: ReactNode;
}

/**
 * Lightweight CSS tooltip. Shows on hover and on keyboard focus
 * (`:focus-within`), so it works without JS positioning. For icon-only
 * controls, still give the control its own aria-label — this is a visual hint.
 */
export function Tooltip({ label, placement = "top", children }: TooltipProps) {
  return (
    <span className={`tip tip-${placement}`}>
      {children}
      <span className="tip-bubble" role="tooltip">{label}</span>
    </span>
  );
}
