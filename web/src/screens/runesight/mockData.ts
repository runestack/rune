/* ============================================================
   RuneSight — small presentation helpers ported from the design
   bundle, plus the query-history seed for the Saved Queries panel.

   All surface data is live: log query / histogram / facets via
   ObserveService (see ../../api/observe), Saved Views via the
   saved-view RPCs (see ../../api/savedViews), Alert Rules and
   Notification Channels via the alerting RPCs (see ../../api/alerting),
   and Sources via stats + metric queries (see ../../api/sources).
   ============================================================ */
import type { LogQuery } from "../../api/observe";
import { normQuery } from "../../api/observe";
import type { IconName } from "../../components";

/* ---- query history seed for the Saved Queries panel ---- */
export const HISTORY_SEED = [
  { q: { namespaces: ["production"], services: ["api-core"] } as Partial<LogQuery>, ts: "12:41:08", hits: "318", dur: "42ms" },
  { q: { levels: ["error"] } as Partial<LogQuery>, ts: "12:38:54", hits: "1.1k", dur: "61ms" },
];

/* derive a meaningful icon + tone from a saved view's query (design: classifyView) */
export type Tone = "red" | "tan" | "cyan" | "green" | "accent";
export function classifyView(q0: Partial<LogQuery>): { icon: IconName; tone: Tone } {
  const q = normQuery(q0);
  const levels = q.levels, services = q.services, text = (q.text || "").toLowerCase();
  if (levels.includes("error") || /error|exception|panic|fail/.test(text)) return { icon: "alert", tone: "red" };
  if (/50\d|5xx|timeout|unavailable/.test(text)) return { icon: "bolt", tone: "tan" };
  if (services.some((s) => /auth|login|token/.test(s)) || /auth|denied|reject|forbidden/.test(text)) return { icon: "shield", tone: "cyan" };
  if (/rollback|deploy|cast|image|release/.test(text) || services.some((s) => /deploy|indexer|ci/.test(s))) return { icon: "refresh", tone: "green" };
  if (/latency|slow|dur|p9\d|timeout/.test(text)) return { icon: "clock", tone: "tan" };
  if (levels.includes("warn")) return { icon: "alert", tone: "tan" };
  if (services.length) return { icon: "services", tone: "accent" };
  if (q.namespaces.length) return { icon: "namespaces", tone: "accent" };
  return { icon: "search", tone: "accent" };
}
