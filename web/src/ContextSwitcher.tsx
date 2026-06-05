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

  return (
    <div className="sb-ctx-wrap" ref={ref}>
      <button
        className={`sb-context${open ? " open" : ""}`}
        onClick={() => setOpen((o) => !o)}
        aria-haspopup="listbox"
        aria-expanded={open}
      >
        <span className="ctx-dot" />
        <div style={{ minWidth: 0 }}>
          <div className="ctx-name">{cluster.name}</div>
          <div className="ctx-sub">{activeLabel}</div>
        </div>
        <Icon name="chevrond" size={15} style={{ marginLeft: "auto", color: "var(--text-3)" }} />
      </button>

      {open && (
        <div className="dd-menu sb-ctx-menu" role="listbox">
          <div className="sb-ctx-head">
            <div className="ctx-name">{cluster.name}</div>
            <div className="ctx-sub">{cluster.context} · {cluster.version}</div>
          </div>
          <div className="dd-sep" />
          <div className="sb-ctx-label eyebrow">Namespace scope</div>
          <div className="dd-list">
            <div className={`dd-item${ns === "all" ? " sel" : ""}`} onClick={() => pick("all")}>
              <Icon name="namespaces" size={14} /><span>All namespaces</span>
              {ns === "all" && <Icon name="check" size={13} className="ddi-sub" />}
            </div>
            <div className="dd-sep" />
            {namespaces.map((n) => (
              <div key={n.name} className={`dd-item${ns === n.name ? " sel" : ""}`} onClick={() => pick(n.name)}>
                <Dot s="run" /><span>{n.name}</span>
                {ns === n.name && <Icon name="check" size={13} className="ddi-sub" />}
              </div>
            ))}
          </div>
          {go && (
            <>
              <div className="dd-sep" />
              <div className="dd-item" onClick={() => { setOpen(false); go("namespaces"); }}>
                <Icon name="namespaces" size={14} /><span>Manage namespaces →</span>
              </div>
            </>
          )}
        </div>
      )}
    </div>
  );
}
