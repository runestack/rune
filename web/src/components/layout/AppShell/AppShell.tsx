import type { ReactNode } from "react";
import "./AppShell.css";

export function AppShell({
  sidebar,
  topbar,
  children,
  collapsed = false,
  contentFlex = false,
}: {
  sidebar: ReactNode;
  topbar: ReactNode;
  children: ReactNode;
  /** When true the sidebar column animates to 0 and content fills the freed space. */
  collapsed?: boolean;
  /** When true the content area becomes a flex column (for full-height screens like Logs). */
  contentFlex?: boolean;
}) {
  return (
    <div className={"app" + (collapsed ? " nav-collapsed" : "")}>
      {sidebar}
      <div className="main">
        {topbar}
        <div className={"content" + (contentFlex ? " content-flex" : "")}>{children}</div>
      </div>
    </div>
  );
}
