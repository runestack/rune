import { forwardRef, type TextareaHTMLAttributes } from "react";
import "../forms.css";

export interface TextareaProps extends TextareaHTMLAttributes<HTMLTextAreaElement> {
  invalid?: boolean;
}

export const Textarea = forwardRef<HTMLTextAreaElement, TextareaProps>(function Textarea(
  { invalid, className = "", ...rest },
  ref,
) {
  return (
    <textarea
      ref={ref}
      className={["field-control", invalid ? "bad" : "", className].filter(Boolean).join(" ")}
      {...rest}
    />
  );
});
