import type { ReactNode } from "react";
import { Icon, type IconName } from "../../primitives/Icon";
import { Avatar } from "../../primitives/Avatar";
import { Logo } from "../../primitives/Logo";
import type { LogoVariant } from "../../../lib/theme";
import "./Sidebar.css";

export interface NavItem { id: string; label: string; icon: IconName; count?: number }
export interface NavGroup { group: string; items: NavItem[] }

export interface SidebarProps {
  nav: NavGroup[];
  route: string;
  go: (id: string) => void;
  logoVariant: LogoVariant;
  /** The cluster/namespace context block (see ContextSwitcher). */
  context?: ReactNode;
  user: { name: string; role: string };
  onLogout?: () => void;
}

export function Sidebar({ nav, route, go, logoVariant, context, user, onLogout }: SidebarProps) {
  return (
    <aside className="sidebar">
      <div className="sb-head">
        <div onClick={() => go("overview")} style={{ cursor: "pointer" }}>
          <Logo variant={logoVariant} />
        </div>
      </div>
      {context}
      <nav className="sb-nav">
        {nav.map((grp) => (
          <div key={grp.group}>
            <div className="sb-group-label eyebrow">{grp.group}</div>
            {grp.items.map((it) => (
              <div key={it.id} className={`nav-item${route === it.id ? " active" : ""}`} onClick={() => go(it.id)}>
                <span className="nav-ico"><Icon name={it.icon} size={17} /></span>
                {it.label}
                {it.count != null && <span className="nav-count tnum">{it.count}</span>}
              </div>
            ))}
          </div>
        ))}
      </nav>
      <div className="sb-foot">
        <Avatar name={user.name} size={28} />
        <div className="who">
          {user.name}
          <small>{user.role}</small>
        </div>
        <button
          className="sb-logout"
          title={onLogout ? "Sign out" : undefined}
          onClick={onLogout}
          style={{ marginLeft: "auto", background: "none", border: "none", color: "var(--text-3)", cursor: onLogout ? "pointer" : "default", display: "flex", padding: 2 }}
        >
          <Icon name={onLogout ? "external" : "dots"} size={16} />
        </button>
      </div>
    </aside>
  );
}
