import type { ReactNode } from "react";
import "./Tag.css";

export function Tag({ children }: { children: ReactNode }) {
  return <span className="tag">{children}</span>;
}
