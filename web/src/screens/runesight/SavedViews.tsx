import { useCallback, useEffect, useState } from "react";
import { Button, Icon, PageHead } from "../../components";
import { parseLogQL, RANGES } from "../../api/observe";
import type { LogQuery, Range } from "../../api/observe";
import { deleteSavedView, listSavedViews, timeAgo } from "../../api/savedViews";
import type { SavedViewData } from "../../api/savedViews";
import { classifyView } from "./mockData";

/* deterministic mini sparkline seeded by the view id — there's no aggregate
   backend yet, so this stands in for the design's 24h volume spark. */
function seededBars(seed: string, n = 24): number[] {
  let h = 0;
  for (let i = 0; i < seed.length; i++) h = (Math.imul(h, 31) + seed.charCodeAt(i)) | 0;
  const out: number[] = [];
  for (let i = 0; i < n; i++) {
    h = (Math.imul(h ^ (h >>> 15), 0x2c1b3c6d) + i) | 0;
    out.push(8 + Math.abs(h % 92));
  }
  return out;
}

function MiniSpark({ seed }: { seed: string }) {
  const bars = seededBars(seed);
  return (
    <div className="rs-vcard-spark">
      {bars.map((h, i) => <i key={i} style={{ height: `${h}%` }} />)}
    </div>
  );
}

/* parse stored logql back into the UI's structured query — a stored view is
   server-validated so this rarely fails, but tolerate garbage anyway. */
function safeParse(logql: string): Partial<LogQuery> | null {
  try { return parseLogQL(logql); } catch { return null; }
}

export interface SavedViewsProps {
  loadView: (q: Partial<LogQuery>) => void;
  setRange: (r: Range) => void;
  go: () => void;
}

export function SavedViews({ loadView, setRange, go }: SavedViewsProps) {
  const [views, setViews] = useState<SavedViewData[] | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [confirmId, setConfirmId] = useState<string | null>(null);

  const refresh = useCallback(() => {
    listSavedViews()
      .then((vs) => { setViews(vs); setError(null); })
      .catch((e: unknown) => setError(e instanceof Error ? e.message : String(e)));
  }, []);
  useEffect(() => { refresh(); }, [refresh]);

  function open(v: SavedViewData) {
    const q = safeParse(v.logql);
    if (!q) return;
    if (v.range && (RANGES as readonly string[]).includes(v.range)) setRange(v.range as Range);
    loadView(q);
  }
  function remove(v: SavedViewData) {
    deleteSavedView(v.name)
      .then(refresh)
      .catch((e: unknown) => setError(e instanceof Error ? e.message : String(e)));
    setConfirmId(null);
  }

  return (
    <div className="wrap">
      <PageHead
        eyebrow="rune sight · saved queries"
        title="Saved <em>Views</em>"
        sub="Reusable LogQL queries your team has saved. Open one to run it in the Explorer."
        actions={<Button size="sm" variant="primary" onClick={go}><Icon name="plus" size={14} />New view</Button>}
      />
      {error && <div className="rs-empty-s" style={{ color: "var(--fail)", marginBottom: 14 }}>{error}</div>}
      {views && views.length === 0 && !error && (
        <div className="rs-vcard" style={{ alignItems: "center", textAlign: "center", padding: "36px 24px", cursor: "default" }}>
          <Icon name="logs" size={24} />
          <div className="rs-vcard-name">No saved views yet</div>
          <div className="rs-empty-s">Save a query from the Explorer.</div>
          <Button size="sm" variant="primary" onClick={go}><Icon name="plus" size={14} />New view</Button>
        </div>
      )}
      <div className="rs-vgrid">
        {(views ?? []).map((v) => {
          const cls = classifyView(safeParse(v.logql) ?? {});
          return (
            <div key={v.id} className="rs-vcard" style={{ position: "relative" }} onClick={() => open(v)}>
              <div className="rs-vcard-head">
                <span className={"rs-vrow-ico tone-" + cls.tone}><Icon name={cls.icon} size={15} /></span>
                <span className="rs-vcard-name">{v.name}</span>
                {v.pinned && <Icon name="pin" size={12} className="rs-vcard-pin" />}
              </div>
              <div className="rs-vcard-q">{v.logql}</div>
              <MiniSpark seed={v.id} />
              {confirmId === v.id ? (
                <div className="rs-saved-confirm" onClick={(e) => e.stopPropagation()}>
                  <span>Delete?</span>
                  <button className="rs-sc-yes" onClick={() => remove(v)}>Delete</button>
                  <button className="rs-sc-no" onClick={() => setConfirmId(null)}>Cancel</button>
                </div>
              ) : (
                <div className="rs-vcard-foot">
                  <span className="rs-vcard-count tnum">last {v.range || "1h"}</span>
                  <span>{v.createdBy || "—"} · {timeAgo(v.updatedAt)}</span>
                </div>
              )}
              {confirmId !== v.id && (
                <button className="rs-saved-del" title="Delete saved view"
                  onClick={(e) => { e.stopPropagation(); setConfirmId(v.id); }}><Icon name="close" size={13} /></button>
              )}
            </div>
          );
        })}
      </div>
    </div>
  );
}
