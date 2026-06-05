import { forwardRef, type ReactNode, type SelectHTMLAttributes } from "react";
import { Icon } from "../../primitives/Icon";
import "../forms.css";

export interface SelectProps extends SelectHTMLAttributes<HTMLSelectElement> {
  invalid?: boolean;
  children?: ReactNode;
}

export const Select = forwardRef<HTMLSelectElement, SelectProps>(function Select(
  { invalid, className = "", children, ...rest },
  ref,
) {
  return (
    <span className="field-select">
      <select
        ref={ref}
        className={["field-control", invalid ? "bad" : "", className].filter(Boolean).join(" ")}
        {...rest}
      >
        {children}
      </select>
      <Icon name="chevrond" size={14} />
    </span>
  );
});
