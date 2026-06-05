import { Icon } from "./components";
import { useScope } from "./lib/scope";

/**
 * Topbar chip showing the active namespace scope. When scoped it highlights and
 * offers a one-click "clear" back to all namespaces, so the current filter is
 * always visible from any screen (not just the sidebar).
 */
export function ScopeIndicator() {
  const { ns, setNs } = useScope();
  const scoped = ns !== "all";
  return (
    <span className={`scope-chip${scoped ? " active" : ""}`} title="Active namespace scope">
      <Icon name="namespaces" size={13} />
      <span>{scoped ? ns : "All namespaces"}</span>
      {scoped && (
        <button className="scope-clear" onClick={() => setNs("all")} aria-label="Clear namespace scope">
          <Icon name="close" size={11} />
        </button>
      )}
    </span>
  );
}
