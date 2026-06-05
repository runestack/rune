import "./Tabs.css";

export interface TabsProps<T extends string = string> {
  tabs: { id: T; label: string }[];
  active: T;
  onChange: (id: T) => void;
}

export function Tabs<T extends string = string>({ tabs, active, onChange }: TabsProps<T>) {
  return (
    <div className="tabs">
      {tabs.map((t) => (
        <button key={t.id} className={`tab${t.id === active ? " active" : ""}`} onClick={() => onChange(t.id)}>
          {t.label}
        </button>
      ))}
    </div>
  );
}
