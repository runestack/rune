import { Icon } from "../../primitives/Icon";
import "./Search.css";

export interface SearchProps {
  value?: string;
  onChange?: (v: string) => void;
  placeholder?: string;
  kbd?: string;
  width?: number;
}

export function Search({ value, onChange, placeholder = "Search resources…", kbd = "⌘K", width }: SearchProps) {
  return (
    <div className="search" style={width ? { width } : undefined}>
      <Icon name="search" size={14} />
      <input
        value={value}
        placeholder={placeholder}
        onChange={onChange ? (e) => onChange(e.target.value) : undefined}
      />
      {kbd && <kbd>{kbd}</kbd>}
    </div>
  );
}
