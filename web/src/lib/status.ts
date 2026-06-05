/** Status semantics — color is reserved for these, used sparingly. */
export type StatusKey = "run" | "deploy" | "warn" | "fail" | "idle" | "net";

export const STATUS_LABEL: Record<StatusKey, string> = {
  run: "Running",
  deploy: "Deploying",
  warn: "Degraded",
  fail: "Failed",
  idle: "Idle",
  net: "Active",
};
