import type { ReactNode } from "react";
import "./AppShell.css";

export function AppShell({ sidebar, topbar, children }: { sidebar: ReactNode; topbar: ReactNode; children: ReactNode }) {
  return (
    <div className="app">
      {sidebar}
      <div className="main">
        {topbar}
        <div className="content">{children}</div>
      </div>
    </div>
  );
}
