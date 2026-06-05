import { useState, type ReactNode } from "react";
import { Icon } from "../../primitives/Icon";
import "./TweaksPanel.css";

export function TweakSection({ label }: { label: string }) {
  return <div className="twk-sect">{label}</div>;
}

export function TweakRadio<T extends string>({
  label,
  value,
  options,
  onChange,
}: {
  label: string;
  value: T;
  options: T[];
  onChange: (v: T) => void;
}) {
  return (
    <div className="twk-row">
      <div className="twk-lbl">{label}</div>
      <div className="twk-seg">
        {options.map((o) => (
          <button key={o} className={o === value ? "on" : ""} onClick={() => onChange(o)}>
            {o}
          </button>
        ))}
      </div>
    </div>
  );
}

export function TweakColor({
  label,
  value,
  options,
  onChange,
}: {
  label: string;
  value: string;
  options: string[];
  onChange: (v: string) => void;
}) {
  return (
    <div className="twk-row">
      <div className="twk-lbl">{label}</div>
      <div className="twk-sw">
        {options.map((c) => (
          <button
            key={c}
            className={c === value ? "on" : ""}
            style={{ background: c }}
            onClick={() => onChange(c)}
            aria-label={c}
          />
        ))}
      </div>
    </div>
  );
}

export function TweaksPanel({ children }: { children: ReactNode }) {
  const [open, setOpen] = useState(true);
  if (!open)
    return (
      <button
        className="twk"
        style={{ width: "auto", padding: "9px 12px", display: "flex", alignItems: "center", gap: 8, cursor: "pointer", color: "var(--text-2)" }}
        onClick={() => setOpen(true)}
      >
        <Icon name="bolt" size={15} style={{ color: "var(--accent-text)" }} />
        <b style={{ fontSize: 12.5 }}>Tweaks</b>
      </button>
    );
  return (
    <div className="twk">
      <div className="twk-hd">
        <Icon name="bolt" size={15} className="twk-ico" />
        <b>Tweaks</b>
        <button className="twk-x" onClick={() => setOpen(false)} aria-label="Close">
          <Icon name="close" size={15} />
        </button>
      </div>
      <div className="twk-body">{children}</div>
    </div>
  );
}
