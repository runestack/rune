/* Saved views API — thin wrappers over ObserveService's saved-view RPCs.
   A saved view is a named LogQL query + relative range token, cluster-shared.
   The server validates the LogQL at save time, so a stored view always parses. */

import {
  DeleteSavedViewRequest,
  ListSavedViewsRequest,
  SavedView as SavedViewMsg,
  SaveViewRequest,
} from "../gen/pkg/api/proto/observe_pb";
import { clients } from "./transport";
import type { Range } from "./observe";

export interface SavedViewData {
  id: string;
  name: string;
  description: string;
  logql: string;
  range: Range | "";
  pinned: boolean;
  createdBy: string;
  createdAt: string; // RFC3339
  updatedAt: string; // RFC3339
}

function fromMsg(m: SavedViewMsg): SavedViewData {
  return {
    id: m.id,
    name: m.name,
    description: m.description,
    logql: m.logql,
    range: (m.range as Range) || "",
    pinned: m.pinned,
    createdBy: m.createdBy,
    createdAt: m.createdAt,
    updatedAt: m.updatedAt,
  };
}

/** List every saved view (server orders pinned-first, then newest-updated). */
export async function listSavedViews(): Promise<SavedViewData[]> {
  const res = await clients.observe.listSavedViews(new ListSavedViewsRequest());
  return res.views.map(fromMsg);
}

/** Upsert a view by name. Returns the stored view (with stamped identity). */
export async function saveView(v: {
  name: string;
  description?: string;
  logql: string;
  range?: string;
  pinned?: boolean;
}): Promise<SavedViewData> {
  const res = await clients.observe.saveView(
    new SaveViewRequest({
      view: new SavedViewMsg({
        name: v.name,
        description: v.description ?? "",
        logql: v.logql,
        range: v.range ?? "",
        pinned: v.pinned ?? false,
      }),
    }),
  );
  if (!res.view) throw new Error("saveView: empty response");
  return fromMsg(res.view);
}

/** Delete a view by name. */
export async function deleteSavedView(name: string): Promise<void> {
  await clients.observe.deleteSavedView(new DeleteSavedViewRequest({ name }));
}

/** Coarse relative time ("just now", "5m ago") from an RFC3339 timestamp. */
export function timeAgo(iso: string): string {
  const t = Date.parse(iso);
  if (Number.isNaN(t)) return "";
  const s = Math.max(0, Math.floor((Date.now() - t) / 1000));
  if (s < 10) return "just now";
  if (s < 60) return `${s}s ago`;
  if (s < 3600) return `${Math.floor(s / 60)}m ago`;
  if (s < 86400) return `${Math.floor(s / 3600)}h ago`;
  return `${Math.floor(s / 86400)}d ago`;
}

/** Slugify a display name into the DNS-1123 name the server requires. */
export function viewSlug(display: string): string {
  return display
    .toLowerCase()
    .replace(/[^a-z0-9-]+/g, "-")
    .replace(/^-+|-+$/g, "")
    .slice(0, 63) || "view";
}
