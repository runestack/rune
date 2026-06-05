import { createContext, useCallback, useContext, useEffect, useRef, useState, type ReactNode } from "react";
import { Icon, type IconName } from "../../primitives/Icon";
import "./Toast.css";

export interface ToastOptions {
  title?: ReactNode;
  /** Smaller secondary line (mono). */
  message?: ReactNode;
  tone?: "default" | "success" | "warn" | "error";
  /** Override the default per-tone icon. */
  icon?: IconName;
  /** Auto-dismiss after this many ms. 0 keeps it until dismissed. Default 4200. */
  duration?: number;
}

interface ToastItem extends ToastOptions { id: number }

const TONE_ICON: Record<NonNullable<ToastOptions["tone"]>, IconName> = {
  default: "health",
  success: "check",
  warn: "alert",
  error: "alert",
};

const ToastCtx = createContext<(o: ToastOptions) => number>(() => 0);

/** Fire a toast: `const toast = useToast(); toast({ tone: "success", title })`. */
export const useToast = () => useContext(ToastCtx);

export function ToastProvider({ children }: { children: ReactNode }) {
  const [items, setItems] = useState<ToastItem[]>([]);
  const idRef = useRef(0);
  const timers = useRef(new Map<number, ReturnType<typeof setTimeout>>());

  const dismiss = useCallback((id: number) => {
    setItems((l) => l.filter((t) => t.id !== id));
    const h = timers.current.get(id);
    if (h) { clearTimeout(h); timers.current.delete(id); }
  }, []);

  const toast = useCallback((opts: ToastOptions) => {
    const id = ++idRef.current;
    setItems((l) => [...l, { ...opts, id }]);
    const dur = opts.duration ?? 4200;
    if (dur > 0) timers.current.set(id, setTimeout(() => dismiss(id), dur));
    return id;
  }, [dismiss]);

  // Clear any pending timers on unmount.
  useEffect(() => {
    const map = timers.current;
    return () => { for (const h of map.values()) clearTimeout(h); };
  }, []);

  return (
    <ToastCtx.Provider value={toast}>
      {children}
      {items.length > 0 && (
        <div className="toast-wrap">
          {items.map((t) => <ToastView key={t.id} item={t} onClose={() => dismiss(t.id)} />)}
        </div>
      )}
    </ToastCtx.Provider>
  );
}

function ToastView({ item, onClose }: { item: ToastItem; onClose: () => void }) {
  const { title, message, tone = "default", icon } = item;
  return (
    <div className={["toast", tone].filter(Boolean).join(" ")} role="status">
      <Icon name={icon ?? TONE_ICON[tone]} size={17} className="t-ico" />
      <div className="t-body">
        {title && <div className="t-title">{title}</div>}
        {message && <div className="t-sub">{message}</div>}
      </div>
      <button className="t-close" onClick={onClose} aria-label="Dismiss"><Icon name="close" size={13} /></button>
    </div>
  );
}
