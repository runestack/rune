import type { ReactNode } from "react";
import "./PageHead.css";

export interface PageHeadProps {
  eyebrow?: string;
  /** Title may contain <em> for accented italic words. */
  title: string;
  sub?: string;
  actions?: ReactNode;
}

export function PageHead({ eyebrow, title, sub, actions }: PageHeadProps) {
  return (
    <div className="page-head" style={{ display: "flex", alignItems: "flex-end", gap: 20 }}>
      <div style={{ flex: 1 }}>
        {eyebrow && <div className="eyebrow">{eyebrow}</div>}
        <h1 className="page-title" dangerouslySetInnerHTML={{ __html: title }} />
        {sub && <p className="page-sub">{sub}</p>}
      </div>
      {actions && <div style={{ display: "flex", gap: 9, flex: "none" }}>{actions}</div>}
    </div>
  );
}
