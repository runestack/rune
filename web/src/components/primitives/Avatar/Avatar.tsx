import { Icon } from "../Icon";
import "./Avatar.css";

export interface AvatarProps {
  name: string;
  /** "user" → gradient circle with an initial; "machine" → square with a glyph. */
  type?: "user" | "machine";
  size?: number;
}

export function Avatar({ name, type = "user", size = 26 }: AvatarProps) {
  return (
    <span
      className={`avatar ${type}`}
      style={{ width: size, height: size, fontSize: Math.round(size * 0.42) }}
      aria-hidden="true"
    >
      {type === "machine" ? <Icon name="terminal" size={Math.round(size * 0.5)} /> : (name[0] || "?").toUpperCase()}
    </span>
  );
}
