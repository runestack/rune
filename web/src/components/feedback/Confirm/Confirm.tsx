import { createContext, useCallback, useContext, useState, type ReactNode } from "react";
import { Modal } from "../Modal";
import { Button } from "../../primitives/Button";
import { Icon, type IconName } from "../../primitives/Icon";
import "./Confirm.css";

/** Optional inline prompt field — replaces window.prompt(). */
export interface ConfirmInput {
  label?: string;
  placeholder?: string;
  defaultValue?: string;
  type?: "text" | "number";
  /** Return an error string to block confirm, or null when valid. */
  validate?: (v: string) => string | null;
}

export interface ConfirmOptions {
  title: string;
  message?: ReactNode;
  confirmLabel?: string;
  /** Pass null to hide the cancel button (a notice/alert dialog). */
  cancelLabel?: string | null;
  tone?: "default" | "danger";
  icon?: IconName;
  input?: ConfirmInput;
}

/** false when dismissed; true when confirmed; the field value when `input` is set. */
export type ConfirmResult = boolean | string;

type Pending = { id: number; opts: ConfirmOptions; resolve: (r: ConfirmResult) => void };

const ConfirmCtx = createContext<(o: ConfirmOptions) => Promise<ConfirmResult>>(
  () => Promise.resolve(false),
);

/** Imperative confirm: `const ok = await confirm({ title, tone: "danger" })`. */
export const useConfirm = () => useContext(ConfirmCtx);

export function ConfirmProvider({ children }: { children: ReactNode }) {
  const [pending, setPending] = useState<Pending | null>(null);

  const confirm = useCallback(
    (opts: ConfirmOptions) =>
      new Promise<ConfirmResult>((resolve) => {
        setPending((prev) => {
          // If a dialog is already open, dismiss it before showing the next.
          prev?.resolve(false);
          return { id: (prev?.id ?? 0) + 1, opts, resolve };
        });
      }),
    [],
  );

  const settle = (p: Pending, r: ConfirmResult) => {
    p.resolve(r);
    setPending((cur) => (cur && cur.id === p.id ? null : cur));
  };

  return (
    <ConfirmCtx.Provider value={confirm}>
      {children}
      {pending && (
        <ConfirmDialog key={pending.id} opts={pending.opts} onResolve={(r) => settle(pending, r)} />
      )}
    </ConfirmCtx.Provider>
  );
}

function ConfirmDialog({ opts, onResolve }: { opts: ConfirmOptions; onResolve: (r: ConfirmResult) => void }) {
  const { title, message, confirmLabel = "Confirm", cancelLabel = "Cancel", tone = "default", icon, input } = opts;
  const [val, setVal] = useState(input?.defaultValue ?? "");
  const [touched, setTouched] = useState(false);
  const err = input?.validate ? input.validate(val) : null;
  const titleId = "confirm-title";
  const msgId = "confirm-msg";

  const onConfirm = () => {
    if (input) {
      if (err) { setTouched(true); return; }
      onResolve(val);
    } else {
      onResolve(true);
    }
  };

  return (
    <Modal
      onClose={() => onResolve(false)}
      labelledBy={titleId}
      describedBy={message ? msgId : undefined}
      width={input ? 460 : 420}
    >
      <div className="confirm">
        <div className="confirm-head">
          {icon && <span className={`confirm-icon ${tone}`}><Icon name={icon} size={17} /></span>}
          <h3 id={titleId} className="confirm-title">{title}</h3>
        </div>
        {message && <div id={msgId} className="confirm-msg">{message}</div>}
        {input && (
          <div className="confirm-field">
            {input.label && <label className="confirm-label">{input.label}</label>}
            <input
              className={`confirm-input${touched && err ? " bad" : ""}`}
              autoFocus
              type={input.type ?? "text"}
              placeholder={input.placeholder}
              value={val}
              onChange={(e) => setVal(e.target.value)}
              onBlur={() => setTouched(true)}
              onKeyDown={(e) => { if (e.key === "Enter") onConfirm(); }}
            />
            {touched && err && <div className="confirm-err">{err}</div>}
          </div>
        )}
        <div className="confirm-actions">
          {cancelLabel !== null && <Button onClick={() => onResolve(false)}>{cancelLabel}</Button>}
          <Button variant={tone === "danger" ? "danger" : "primary"} onClick={onConfirm}>{confirmLabel}</Button>
        </div>
      </div>
    </Modal>
  );
}
