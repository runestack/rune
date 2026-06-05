import "./Spark.css";

export interface SparkProps {
  data: number[];
  max?: number;
}

export function Spark({ data, max = 100 }: SparkProps) {
  const hiAt = data.indexOf(Math.max(...data));
  return (
    <div className="spark">
      {data.map((d, i) => (
        <i key={i} className={i === hiAt ? "hi" : ""} style={{ height: `${Math.max(4, (d / max) * 100)}%` }} />
      ))}
    </div>
  );
}
