import "./LogWell.css";

export interface LogWellLine { ts: string; level: string; svc: string; msg: string }

export function LogWell({ lines, height }: { lines: LogWellLine[]; height?: number }) {
  return (
    <div className="logwell" style={{ maxHeight: height, overflowY: "auto" }}>
      {lines.map((l, i) => (
        <div className="logline" key={i}>
          <span className="lt">{l.ts}</span>
          <span className={`lv ${l.level}`}>{l.level.toUpperCase()}</span>
          <span className="lsvc">{l.svc}</span>
          <span className="lm">{l.msg}</span>
        </div>
      ))}
    </div>
  );
}
