import { Icon } from "../../primitives/Icon";
import "./Search.css";

export interface SearchProps {
  /** Open the command palette. */
  onClick?: () => void;
  placeholder?: string;
  kbd?: string;
  width?: number;
}

/**
 * The topbar search affordance. It's a button (not an input) that opens the
 * command palette — typing happens inside the palette itself.
 */
export function Search({ onClick, placeholder = "Search resources…", kbd = "⌘K", width }: SearchProps) {
  return (
    <button className="search" onClick={onClick} title="Search resources (⌘K)" style={width ? { width } : undefined}>
      <Icon name="search" size={14} />
      <span className="search-ph">{placeholder}</span>
      {kbd && <kbd>{kbd}</kbd>}
    </button>
  );
}
