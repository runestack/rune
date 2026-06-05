import type { ReactNode } from "react";
import "./Kpi.css";

export function KpiRow({ children }: { children: ReactNode }) {
  return <div className="kpi-row">{children}</div>;
}

export interface KpiProps {
  hero?: boolean;
  label: ReactNode;
  value: ReactNode;
  sub?: ReactNode;
}

export function Kpi({ hero, label, value, sub }: KpiProps) {
  return (
    <div className={`kpi${hero ? " kpi-hero" : ""}`}>
      <div className="kpi-label">{label}</div>
      <div className="kpi-val">
        {hero && <span className="kpi-tick" />}
        {value}
      </div>
      {sub && <div className="kpi-sub">{sub}</div>}
    </div>
  );
}
