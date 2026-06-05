import type { ReactNode } from "react";
import "./Switch.css";

export interface SwitchProps {
  checked: boolean;
  onChange: (v: boolean) => void;
  label?: ReactNode;
  disabled?: boolean;
  id?: string;
}

/** Binary on/off toggle. Use for state that applies immediately (not form submit). */
export function Switch({ checked, onChange, label, disabled, id }: SwitchProps) {
  return (
    <label className={`switch${disabled ? " disabled" : ""}`}>
      <button
        type="button"
        role="switch"
        id={id}
        aria-checked={checked}
        disabled={disabled}
        className={`switch-track${checked ? " on" : ""}`}
        onClick={() => !disabled && onChange(!checked)}
      >
        <span className="switch-thumb" />
      </button>
      {label != null && <span className="switch-label">{label}</span>}
    </label>
  );
}
