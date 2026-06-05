import { forwardRef, type InputHTMLAttributes } from "react";
import "../forms.css";

export interface TextInputProps extends InputHTMLAttributes<HTMLInputElement> {
  /** Render the error border (red). Pair with a Field `error` for the message. */
  invalid?: boolean;
}

export const TextInput = forwardRef<HTMLInputElement, TextInputProps>(function TextInput(
  { invalid, className = "", ...rest },
  ref,
) {
  return (
    <input
      ref={ref}
      className={["field-control", invalid ? "bad" : "", className].filter(Boolean).join(" ")}
      {...rest}
    />
  );
});
