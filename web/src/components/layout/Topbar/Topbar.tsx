import { Fragment, type ReactNode } from "react";
import { Icon } from "../../primitives/Icon";
import { Search } from "../../inputs/Search";
import "./Topbar.css";

export function Topbar({
  crumbs,
  scope,
  actions,
  collapsed = false,
  onToggleNav,
  onSearch,
}: {
  crumbs: string[];
  scope?: ReactNode;
  actions?: ReactNode;
  /** Sidebar collapsed — show the reopen (panel) button. */
  collapsed?: boolean;
  onToggleNav?: () => void;
  /** Open the command palette. */
  onSearch?: () => void;
}) {
  return (
    <div className="topbar">
      {collapsed && onToggleNav && (
        <button className="nav-toggle topbar-toggle" title="Open sidebar" aria-label="Open sidebar" onClick={onToggleNav}>
          <Icon name="panel" size={16} />
        </button>
      )}
      <div className="crumb">
        {crumbs.map((c, i) => (
          <Fragment key={i}>
            {i > 0 && <span className="sep"><Icon name="chevron" size={13} /></span>}
            {i === crumbs.length - 1 ? <b>{c}</b> : <span>{c}</span>}
          </Fragment>
        ))}
      </div>
      {scope}
      <div className="spacer" />
      <Search onClick={onSearch} />
      {actions}
    </div>
  );
}
