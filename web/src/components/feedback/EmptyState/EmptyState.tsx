import "./EmptyState.css";
import { Icon } from "../../primitives/Icon";
import type { IconName } from "../../primitives/Icon";

/**
 * EmptyState — a quiet placeholder for empty or errored data. Reuses the muted
 * `.empty` palette; never shouts. `tone="error"` tints the icon for failures
 * (e.g. a live call that couldn't reach runed) with an optional retry action.
 */
export function EmptyState({
  icon, title, hint, tone = "muted", action,
}: {
  icon?: IconName;
  title: string;
  hint?: string;
  tone?: "muted" | "error";
  action?: { label: string; onClick: () => void };
}) {
  return (
    <div className={`emptystate ${tone}`}>
      {icon && <Icon name={icon} size={22} />}
      <div className="es-title">{title}</div>
      {hint && <div className="es-hint">{hint}</div>}
      {action && (
        <button className="es-action" onClick={action.onClick}>
          <Icon name="refresh" size={13} />
          {action.label}
        </button>
      )}
    </div>
  );
}
