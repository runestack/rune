import type { ReactNode } from "react";
import "./Card.css";

export interface CardProps {
  children: ReactNode;
  className?: string;
  pad?: boolean;
  style?: React.CSSProperties;
}

export function Card({ children, className = "", pad = false, style }: CardProps) {
  return (
    <div className={`card${pad ? " card-pad" : ""} ${className}`} style={style}>
      {children}
    </div>
  );
}

export interface CardHeadProps {
  children: ReactNode;
  actions?: ReactNode;
}

export function CardHead({ children, actions }: CardHeadProps) {
  return (
    <div className="card-head">
      <h3>{children}</h3>
      {actions && <div className="ch-act">{actions}</div>}
    </div>
  );
}
