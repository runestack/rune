import { useEffect, useRef, useState } from "react";
import { Icon } from "../Icon";
import { Tooltip } from "../Tooltip";
import "./CopyButton.css";

export interface CopyButtonProps {
  /** Text written to the clipboard. */
  value: string;
  size?: number;
  className?: string;
}

/** Icon button that copies `value` and flips to a check + "Copied" for ~1.2s. */
export function CopyButton({ value, size = 14, className = "" }: CopyButtonProps) {
  const [done, setDone] = useState(false);
  const timer = useRef<ReturnType<typeof setTimeout> | null>(null);

  useEffect(() => () => { if (timer.current) clearTimeout(timer.current); }, []);

  const copy = () => {
    navigator.clipboard?.writeText(value);
    setDone(true);
    if (timer.current) clearTimeout(timer.current);
    timer.current = setTimeout(() => setDone(false), 1200);
  };

  return (
    <Tooltip label={done ? "Copied" : "Copy"}>
      <button
        type="button"
        className={["copy-btn", done ? "done" : "", className].filter(Boolean).join(" ")}
        onClick={copy}
        aria-label={done ? "Copied" : "Copy to clipboard"}
      >
        <Icon name={done ? "check" : "copy"} size={size} />
      </button>
    </Tooltip>
  );
}
