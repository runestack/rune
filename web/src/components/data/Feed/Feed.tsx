import "./Feed.css";

export interface FeedEvent { t: string; ns: string; msg: string; status: string }

export function Feed({ events }: { events: FeedEvent[] }) {
  return (
    <div className="feed">
      {events.map((e, i) => (
        <div className="feed-item" key={i}>
          <div className="feed-rail">
            <span className="fd-dot" style={{ background: `var(--${e.status})` }} />
            {i < events.length - 1 && <span className="fd-line" />}
          </div>
          <div className="feed-body">
            <div className="feed-msg" dangerouslySetInnerHTML={{ __html: e.msg }} />
            <div className="feed-time">{e.t} · {e.ns}</div>
          </div>
        </div>
      ))}
    </div>
  );
}
