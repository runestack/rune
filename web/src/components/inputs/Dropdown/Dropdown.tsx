import { useEffect, useRef, useState, type ReactNode } from "react";
import { Icon } from "../../primitives/Icon";
import "./Dropdown.css";

export interface DropdownProps {
  /** Trigger label content (e.g. an eyebrow + bold value). */
  label: ReactNode;
  width?: number;
  /** Render-prop menu body; receives a close() callback. */
  children: (close: () => void) => ReactNode;
}

/** Searchable/scrollable picker — used everywhere a filter can grow unbounded. */
export function Dropdown({ label, width = 260, children }: DropdownProps) {
  const [open, setOpen] = useState(false);
  const ref = useRef<HTMLDivElement>(null);
  useEffect(() => {
    const h = (e: MouseEvent) => {
      if (ref.current && !ref.current.contains(e.target as Node)) setOpen(false);
    };
    document.addEventListener("mousedown", h);
    return () => document.removeEventListener("mousedown", h);
  }, []);
  return (
    <div className="dd" ref={ref}>
      <button className="dd-trigger" onClick={() => setOpen((o) => !o)}>
        {label}
        <Icon name="chevrond" size={14} style={{ marginLeft: "auto", color: "var(--text-3)" }} />
      </button>
      {open && (
        <div className="dd-menu" style={{ width }}>
          {children(() => setOpen(false))}
        </div>
      )}
    </div>
  );
}
