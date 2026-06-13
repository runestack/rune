import { useEffect, useRef, useState, type ReactNode } from "react";
import { Icon, type IconName } from "../../primitives/Icon";
import { Avatar } from "../../primitives/Avatar";
import { Logo } from "../../primitives/Logo";
import { THEMES, type LogoVariant, type ThemeMode } from "../../../lib/theme";
import "./Sidebar.css";

export interface NavItem { id: string; label: string; icon: IconName; count?: number; firing?: boolean }
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
  /** Active surface theme + setter, surfaced in the footer menu. */
  theme?: ThemeMode;
  onTheme?: (m: ThemeMode) => void;
  /** Collapse the sidebar (chevron in the header). */
  onToggleNav?: () => void;
}

export function Sidebar({ nav, route, go, logoVariant, context, user, onLogout, theme = "dark", onTheme, onToggleNav }: SidebarProps) {
  const [menuOpen, setMenuOpen] = useState(false);
  const footRef = useRef<HTMLDivElement>(null);

  // The footer menu closes on outside-click or Escape.
  useEffect(() => {
    if (!menuOpen) return;
    const onDown = (e: MouseEvent) => {
      if (footRef.current && !footRef.current.contains(e.target as Node)) setMenuOpen(false);
    };
    const onKey = (e: KeyboardEvent) => {
      if (e.key === "Escape") setMenuOpen(false);
    };
    document.addEventListener("mousedown", onDown);
    document.addEventListener("keydown", onKey);
    return () => {
      document.removeEventListener("mousedown", onDown);
      document.removeEventListener("keydown", onKey);
    };
  }, [menuOpen]);

  return (
    <aside className="sidebar">
      <div className="sb-head">
        <div className="sb-brand" onClick={() => go("overview")} style={{ cursor: "pointer", minWidth: 0 }}>
          <Logo variant={logoVariant} />
        </div>
        {onToggleNav && (
          <button className="nav-toggle" title="Collapse sidebar" aria-label="Collapse sidebar" onClick={onToggleNav}>
            <Icon name="chevleft" size={16} />
          </button>
        )}
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
                {it.count != null && <span className={"nav-count tnum" + (it.firing ? " firing" : "")}>{it.count}</span>}
              </div>
            ))}
          </div>
        ))}
      </nav>
      <div className="sb-foot" ref={footRef}>
        <Avatar name={user.name} size={28} />
        <div className="who">
          {user.name}
          <small>{user.role}</small>
        </div>
        <button
          className={"sb-foot-menu-btn" + (menuOpen ? " on" : "")}
          title="Menu"
          aria-label="Open menu"
          aria-haspopup="true"
          aria-expanded={menuOpen}
          onClick={() => setMenuOpen((o) => !o)}
        >
          <Icon name="dots" size={16} />
        </button>
        {menuOpen && (
          <div className="sb-foot-pop" role="menu">
            <div className="sb-foot-pop-label eyebrow">Theme</div>
            {THEMES.map((th) => (
              <button
                key={th.id}
                type="button"
                role="menuitemradio"
                aria-checked={theme === th.id}
                className={"sb-theme-row" + (theme === th.id ? " on" : "")}
                onClick={() => {
                  onTheme?.(th.id);
                  setMenuOpen(false);
                }}
              >
                <span className="sb-theme-row-sw" style={{ background: th.sw }} />
                <span className="sb-theme-row-lab">{th.label}</span>
                {theme === th.id && <Icon name="check" size={15} className="sb-theme-row-check" />}
              </button>
            ))}
            {onLogout && (
              <>
                <div className="sb-foot-pop-sep" />
                <button
                  type="button"
                  role="menuitem"
                  className="sb-foot-action"
                  onClick={() => {
                    setMenuOpen(false);
                    onLogout();
                  }}
                >
                  <Icon name="logout" size={16} className="sb-foot-action-ico" />
                  <span>Log out</span>
                </button>
              </>
            )}
          </div>
        )}
      </div>
    </aside>
  );
}
