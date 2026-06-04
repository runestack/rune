import { Icon, type IconName } from "../../primitives/Icon";
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
  cluster: { name: string; context: string; version: string };
  user: { name: string; role: string };
  demo?: boolean;
  onLogout?: () => void;
}

export function Sidebar({ nav, route, go, logoVariant, cluster, user, demo, onLogout }: SidebarProps) {
  return (
    <aside className="sidebar">
      <div className="sb-head">
        <div onClick={() => go("overview")} style={{ cursor: "pointer" }}>
          <Logo variant={logoVariant} />
        </div>
      </div>
      <div className="sb-context" onClick={() => go("namespaces")}>
        <span className="ctx-dot" />
        <div style={{ minWidth: 0 }}>
          <div className="ctx-name">{cluster.name}</div>
          <div className="ctx-sub">{cluster.context} · {cluster.version}</div>
        </div>
        <Icon name="chevrond" size={15} style={{ marginLeft: "auto", color: "var(--text-3)" }} />
      </div>
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
        <div className="avatar">{(user.name[0] || "?").toUpperCase()}</div>
        <div className="who">
          {user.name}
          <small>{demo ? "demo session" : user.role}</small>
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
