import { useEffect, useRef, useState } from "react";
import { Dot, Icon } from "./components";
import { useCluster, useNamespaces } from "./api/hooks";
import { useScope } from "./lib/scope";

/**
 * Sidebar context block. The cluster is fixed (bound to the origin that served
 * the dashboard), so the switchable "context" here is the namespace scope. The
 * menu shows cluster identity (read-only) and lets you pick the active scope.
 */
export function ContextSwitcher({ go }: { go?: (r: string) => void }) {
  const { ns, setNs } = useScope();
  const { data: cluster } = useCluster();
  const { data: namespaces } = useNamespaces();
  const [open, setOpen] = useState(false);
  const ref = useRef<HTMLDivElement>(null);

  useEffect(() => {
    const h = (e: MouseEvent) => { if (ref.current && !ref.current.contains(e.target as Node)) setOpen(false); };
    document.addEventListener("mousedown", h);
    return () => document.removeEventListener("mousedown", h);
  }, []);

  const activeLabel = ns === "all" ? "All namespaces" : ns;
  const pick = (v: string) => { setNs(v); setOpen(false); };
  // A namespace is "empty" only with no services AND no data resources — a
  // secrets/config/volume-only namespace still has content.
  const dataCount = (n: { secrets: number; configs: number; volumes: number }) => n.secrets + n.configs + n.volumes;
  const isEmpty = (n: { services: number; secrets: number; configs: number; volumes: number }) => n.services === 0 && dataCount(n) === 0;
  const nsNonEmpty = namespaces.filter((n) => !isEmpty(n)).length;

  return (
    <div className="sb-ctx-wrap" ref={ref}>
      <button
        className={`sb-context${open ? " open" : ""}`}
        onClick={() => setOpen((o) => !o)}
        aria-haspopup="listbox"
        aria-expanded={open}
      >
        <span className="ctx-dot" />
        <div style={{ minWidth: 0, textAlign: "left" }}>
          <div className="ctx-name">{cluster.name}</div>
          <div className="ctx-sub" style={{ whiteSpace: "nowrap", overflow: "hidden", textOverflow: "ellipsis" }}>{activeLabel}</div>
        </div>
        <Icon name="chevrond" size={15} className="ctx-chev" />
      </button>

      {open && (
        <div className="ns-pop" role="listbox">
          <div className="ns-head">
            <div className="ns-cluster">{cluster.name}</div>
            <div className="ns-meta">
              {cluster.context && <span className="ns-endpoint">{cluster.context}</span>}
              {cluster.version && <span className="ns-ver">{cluster.version}</span>}
            </div>
          </div>
          <div className="ns-sep" />
          <div className="ns-scope">
            <div className="eyebrow ns-scope-label">Namespace scope</div>
            <div className={`ns-item${ns === "all" ? " sel" : ""}`} onClick={() => pick("all")}>
              <Icon name="namespaces" size={15} className="ns-ico" />
              <span className="ns-label">All namespaces</span>
              <span className="ns-right">{ns === "all" ? <Icon name="check" size={15} className="ns-check" /> : <span className="ns-count">{nsNonEmpty}</span>}</span>
            </div>
          </div>
          <div className="ns-sep" />
          <div className="ns-list">
            {namespaces.map((n) => (
              <div key={n.name} className={`ns-item${ns === n.name ? " sel" : ""}`} onClick={() => pick(n.name)}>
                <Dot s={n.services > 0 ? "run" : "idle"} />
                <span className="ns-label">{n.name}</span>
                <span className="ns-right">
                  {ns === n.name
                    ? <Icon name="check" size={15} className="ns-check" />
                    : n.services > 0
                      ? <span className="ns-count">{n.services}</span>
                      : dataCount(n) > 0
                        ? <span className="ns-count">{dataCount(n)}</span>
                        : <span className="ns-count ns-empty">empty</span>}
                </span>
              </div>
            ))}
          </div>
          {go && (
            <>
              <div className="ns-sep" />
              <button className="ns-manage" onClick={() => { setOpen(false); go("namespaces"); }}>
                <Icon name="namespaces" size={15} />
                <span>Manage namespaces</span>
                <span className="ns-arrow">→</span>
              </button>
            </>
          )}
        </div>
      )}
    </div>
  );
}
