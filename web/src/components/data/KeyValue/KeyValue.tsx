import type { ReactNode } from "react";
import "./KeyValue.css";

export interface KVRow { k: ReactNode; v: ReactNode }

export function KeyValue({ rows, children }: { rows?: KVRow[]; children?: ReactNode }) {
  return (
    <dl className="kv">
      {rows?.map((r, i) => (
        <div key={i} style={{ display: "contents" }}>
          <dt>{r.k}</dt>
          <dd>{r.v}</dd>
        </div>
      ))}
      {children}
    </dl>
  );
}
