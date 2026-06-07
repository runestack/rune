import { Button, Icon, PageHead } from "../../components";
import type { LogQuery } from "../../api/observe";
import { SAVED_VIEWS, classifyView, savedViewLogQL } from "./mockData";
import type { SavedView } from "./mockData";

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

function MiniSpark({ view }: { view: SavedView }) {
  const bars = seededBars(view.id);
  return (
    <div className="rs-vcard-spark">
      {bars.map((h, i) => <i key={i} style={{ height: `${h}%` }} />)}
    </div>
  );
}

export interface SavedViewsProps {
  loadView: (q: Partial<LogQuery>) => void;
  go: () => void;
}

export function SavedViews({ loadView, go }: SavedViewsProps) {
  return (
    <div className="wrap">
      <PageHead
        eyebrow="rune sight · saved queries"
        title="Saved <em>Views</em>"
        sub="Reusable LogQL queries your team has saved. Open one to run it in the Explorer."
        actions={<Button size="sm" variant="primary" onClick={go}><Icon name="plus" size={14} />New view</Button>}
      />
      <div className="rs-vgrid">
        {SAVED_VIEWS.map((v) => {
          const cls = classifyView(v.q);
          return (
            <button key={v.id} className="rs-vcard" onClick={() => loadView(v.q)}>
              <div className="rs-vcard-head">
                <span className={"rs-vrow-ico tone-" + cls.tone}><Icon name={cls.icon} size={15} /></span>
                <span className="rs-vcard-name">{v.name}</span>
                {v.pinned && <Icon name="pin" size={12} className="rs-vcard-pin" />}
              </div>
              <div className="rs-vcard-q">{savedViewLogQL(v.q)}</div>
              <MiniSpark view={v} />
              <div className="rs-vcard-foot">
                <span className="rs-vcard-count tnum">{v.count.toLocaleString()} lines · 24h</span>
                <span>{v.owner} · {v.last}</span>
              </div>
            </button>
          );
        })}
      </div>
    </div>
  );
}
