import type { ReactNode } from "react";
import "./Table.css";

/** Hairline data table. Provide <thead>/<tbody> children (cols + cell-name/num
 *  helpers keep markup faithful to the design). */
export function Table({ children }: { children: ReactNode }) {
  return <table className="tbl">{children}</table>;
}
