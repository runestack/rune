import type { StatusKey } from "../../../lib/status";
import "./Dot.css";

export interface DotProps {
  s: StatusKey;
  pulse?: boolean;
}

export function Dot({ s, pulse }: DotProps) {
  return <span className={`dot ${s}${pulse ? " pulse" : ""}`} />;
}
