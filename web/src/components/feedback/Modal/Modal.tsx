import { useEffect, useRef, type ReactNode } from "react";
import "./Modal.css";

export interface ModalProps {
  onClose: () => void;
  children: ReactNode;
  /** id of the element that titles the dialog (for aria-labelledby). */
  labelledBy?: string;
  /** id of the element that describes the dialog (for aria-describedby). */
  describedBy?: string;
  width?: number;
  /** Dismiss when the scrim is clicked. Default true. */
  closeOnScrim?: boolean;
}

const FOCUSABLE =
  'button, [href], input, select, textarea, [tabindex]:not([tabindex="-1"])';

/**
 * Centered, accessible overlay dialog. Traps Tab focus, closes on Escape,
 * restores focus to the previously-focused element on unmount, and marks
 * itself aria-modal. Escape is handled in the capture phase and stops
 * propagation so a confirm opened over a Drawer dismisses only the dialog.
 */
export function Modal({ onClose, children, labelledBy, describedBy, width = 440, closeOnScrim = true }: ModalProps) {
  const panelRef = useRef<HTMLDivElement>(null);

  useEffect(() => {
    const prevFocus = document.activeElement as HTMLElement | null;
    const panel = panelRef.current;
    const focusables = () =>
      panel
        ? Array.from(panel.querySelectorAll<HTMLElement>(FOCUSABLE)).filter((el) => !el.hasAttribute("disabled"))
        : [];
    (focusables()[0] ?? panel)?.focus();

    const onKey = (e: KeyboardEvent) => {
      if (e.key === "Escape") {
        e.stopPropagation();
        onClose();
        return;
      }
      if (e.key === "Tab") {
        const f = focusables();
        if (!f.length) { e.preventDefault(); return; }
        const active = document.activeElement as HTMLElement;
        const idx = f.indexOf(active);
        if (e.shiftKey && idx <= 0) { e.preventDefault(); f[f.length - 1].focus(); }
        else if (!e.shiftKey && idx === f.length - 1) { e.preventDefault(); f[0].focus(); }
      }
    };
    window.addEventListener("keydown", onKey, true);
    return () => {
      window.removeEventListener("keydown", onKey, true);
      prevFocus?.focus?.();
    };
  }, [onClose]);

  return (
    <>
      <div className="modal-scrim" onClick={closeOnScrim ? onClose : undefined} />
      <div className="modal-wrap">
        <div
          ref={panelRef}
          className="modal"
          role="dialog"
          aria-modal="true"
          aria-labelledby={labelledBy}
          aria-describedby={describedBy}
          style={{ width }}
          tabIndex={-1}
        >
          {children}
        </div>
      </div>
    </>
  );
}
