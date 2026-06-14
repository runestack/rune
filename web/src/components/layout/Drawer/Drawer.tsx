import { useEffect, type ReactNode } from "react";
import { createPortal } from "react-dom";
import { Icon } from "../../primitives/Icon";
import { Button } from "../../primitives/Button";
import "./Drawer.css";

export function Drawer({ onClose, children }: { onClose: () => void; children: ReactNode }) {
  useEffect(() => {
    const h = (e: KeyboardEvent) => { if (e.key === "Escape") onClose(); };
    window.addEventListener("keydown", h);
    return () => window.removeEventListener("keydown", h);
  }, [onClose]);
  // Portal to <body> so the fixed scrim/panel are positioned against the
  // viewport, not whatever transformed/animated ancestor (e.g. a `.fadein`
  // screen wrapper) would otherwise become their containing block and clip
  // the panel to a short height.
  return createPortal(
    <>
      <div className="drawer-scrim" onClick={onClose} />
      <div className="drawer" role="dialog">
        <Button icon variant="ghost" className="drawer-close" onClick={onClose}><Icon name="close" size={17} /></Button>
        {children}
      </div>
    </>,
    document.body,
  );
}
