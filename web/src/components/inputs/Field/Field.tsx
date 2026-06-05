import type { ReactNode } from "react";
import "../forms.css";

export interface FieldProps {
  label?: ReactNode;
  htmlFor?: string;
  /** Neutral helper text shown when there's no error/success message. */
  hint?: ReactNode;
  /** Red message — takes priority over success/hint. */
  error?: ReactNode;
  /** Green message — shown when there's no error. */
  success?: ReactNode;
  children: ReactNode;
}

/**
 * Labels a control and renders a single status line beneath it: error (red) if
 * present, else success (green), else hint (neutral). Pass `invalid` to the
 * inner control to match the red border.
 */
export function Field({ label, htmlFor, hint, error, success, children }: FieldProps) {
  const tone = error ? "bad" : success ? "ok" : "";
  const msg = error ?? success ?? hint;
  return (
    <div className="field">
      {label != null && <label className="field-label" htmlFor={htmlFor}>{label}</label>}
      {children}
      {msg != null && msg !== "" && <div className={`field-hint ${tone}`}>{msg}</div>}
    </div>
  );
}
