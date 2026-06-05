import type { ReactNode } from "react";
import { Icon } from "../../primitives/Icon";
import "./Checkbox.css";

export interface CheckboxProps {
  checked: boolean;
  onChange: (v: boolean) => void;
  label?: ReactNode;
  disabled?: boolean;
  id?: string;
}

export function Checkbox({ checked, onChange, label, disabled, id }: CheckboxProps) {
  return (
    <label className={`checkbox${disabled ? " disabled" : ""}`}>
      <button
        type="button"
        role="checkbox"
        id={id}
        aria-checked={checked}
        disabled={disabled}
        className={`checkbox-box${checked ? " on" : ""}`}
        onClick={() => !disabled && onChange(!checked)}
      >
        {checked && <Icon name="check" size={12} stroke={3} />}
      </button>
      {label != null && <span className="checkbox-label">{label}</span>}
    </label>
  );
}
