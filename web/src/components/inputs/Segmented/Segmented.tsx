import "./Segmented.css";

export interface SegmentedOption<T extends string = string> {
  value: T;
  label: string;
}

export interface SegmentedProps<T extends string = string> {
  options: (SegmentedOption<T> | T)[];
  value: T;
  onChange: (v: T) => void;
}

/** Segmented control — only for genuinely finite sets (e.g. status filter). */
export function Segmented<T extends string = string>({ options, value, onChange }: SegmentedProps<T>) {
  return (
    <div className="seg">
      {options.map((o) => {
        const opt = typeof o === "string" ? { value: o, label: o } : o;
        return (
          <button
            key={opt.value}
            className={opt.value === value ? "on" : ""}
            onClick={() => onChange(opt.value)}
          >
            {opt.label}
          </button>
        );
      })}
    </div>
  );
}
