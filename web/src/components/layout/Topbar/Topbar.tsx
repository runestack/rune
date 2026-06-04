import { Fragment, type ReactNode } from "react";
import { Icon } from "../../primitives/Icon";
import { Search } from "../../inputs/Search";
import "./Topbar.css";

export function Topbar({ crumbs, actions }: { crumbs: string[]; actions?: ReactNode }) {
  return (
    <div className="topbar">
      <div className="crumb">
        {crumbs.map((c, i) => (
          <Fragment key={i}>
            {i > 0 && <span className="sep"><Icon name="chevron" size={13} /></span>}
            {i === crumbs.length - 1 ? <b>{c}</b> : <span>{c}</span>}
          </Fragment>
        ))}
      </div>
      <div className="spacer" />
      <Search />
      {actions}
    </div>
  );
}
