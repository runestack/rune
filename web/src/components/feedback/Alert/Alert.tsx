import type { CSSProperties, ReactNode } from "react";
import { Icon, type IconName } from "../../primitives/Icon";
import "./Alert.css";

export interface AlertProps {
  tone?: "info" | "success" | "warn" | "error";
  /** Override the default per-tone icon. */
  icon?: IconName;
  title?: ReactNode;
  children?: ReactNode;
  className?: string;
  style?: CSSProperties;
}

const TONE_ICON: Record<NonNullable<AlertProps["tone"]>, IconName> = {
  info: "health",
  success: "check",
  warn: "alert",
  error: "alert",
};

/** Inline status banner. `error` gets role="alert"; others are polite status. */
export function Alert({ tone = "info", icon, title, children, className = "", style }: AlertProps) {
  return (
    <div className={["alert", tone, className].filter(Boolean).join(" ")} role={tone === "error" ? "alert" : "status"} style={style}>
      <Icon name={icon ?? TONE_ICON[tone]} size={15} className="alert-ico" />
      <div className="alert-body">
        {title && <div className="alert-title">{title}</div>}
        {children}
      </div>
    </div>
  );
}
