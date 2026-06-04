/* ============================================================
   Live streaming helpers for Logs (server-stream) and Exec (WS bridge).

   - Logs use LogService.GetLogs (server-streaming) — browser-callable, unlike
     the bidi StreamLogs. See RUNE-200C.
   - Exec uses the /v1/exec/ws WebSocket bridge: the bearer token rides the
     Sec-WebSocket-Protocol header as "rune.bearer.<token>"; binary frames are
     proto-encoded ExecRequest / ExecResponse.
   ============================================================ */
import { clients } from "./transport";
import { getAccessToken, ensureFresh } from "./session";
import { LogRequest } from "../gen/pkg/api/proto/logs_pb";
import {
  ExecRequest, ExecResponse, ExecInitRequest, TerminalSize,
} from "../gen/pkg/api/proto/exec_pb";

export interface LiveLogLine {
  ts: string;
  level: string;
  origin: string;
  content: string;
}

export interface LogStreamOpts {
  target: string;     // service name or instance id
  namespace: string;
  follow: boolean;
  tail: number;
  onLine: (line: LiveLogLine) => void;
  onError: (err: Error) => void;
  onEnd?: () => void;
}

/**
 * streamLogs opens a GetLogs server-stream and pushes each line through onLine.
 * Returns a stop() that aborts the stream — call it on unmount / route change /
 * target change so the connection is torn down cleanly (no leaks).
 */
export function streamLogs(opts: LogStreamOpts): () => void {
  const ctrl = new AbortController();
  let stopped = false;

  (async () => {
    try {
      const req = new LogRequest({
        resourceTarget: opts.target,
        namespace: opts.namespace || "default",
        follow: opts.follow,
        tail: opts.tail > 0 ? opts.tail : 0,
        timestamps: true,
      });
      for await (const resp of clients.logs.getLogs(req, { signal: ctrl.signal })) {
        if (stopped) break;
        // Control frames (status only, no content) are skipped.
        if (!resp.content && !resp.timestamp) continue;
        opts.onLine({
          ts: fmtTs(resp.timestamp),
          level: (resp.logLevel || levelFromContent(resp.content)).toLowerCase(),
          origin: resp.instanceName || resp.instanceId || resp.serviceName || opts.target,
          content: resp.content,
        });
      }
      if (!stopped) opts.onEnd?.();
    } catch (e) {
      if (stopped || ctrl.signal.aborted) return;
      opts.onError(e instanceof Error ? e : new Error(String(e)));
    }
  })();

  return () => {
    stopped = true;
    ctrl.abort();
  };
}

function fmtTs(rfc: string): string {
  if (!rfc) return new Date().toISOString().slice(11, 23);
  const n = Date.parse(rfc);
  if (Number.isNaN(n)) return rfc.slice(11, 23) || rfc;
  return new Date(n).toISOString().slice(11, 23);
}

function levelFromContent(c: string): string {
  const s = (c || "").toLowerCase();
  if (/\b(error|err|fatal|panic)\b/.test(s)) return "error";
  if (/\b(warn|warning)\b/.test(s)) return "warn";
  if (/\bdebug\b/.test(s)) return "debug";
  return "info";
}

/* ---------------- exec WS bridge ---------------- */

export interface ExecSessionOpts {
  service?: string;
  instanceId?: string;
  namespace: string;
  command: string[];
  tty: boolean;
  cols: number;
  rows: number;
  onData: (text: string) => void;       // stdout/stderr decoded to text
  onExit: (code: number, signal?: string) => void;
  onError: (err: Error) => void;
  onOpen?: () => void;
}

export interface ExecSession {
  /** Send raw stdin bytes (e.g. a typed command line). */
  send: (text: string) => void;
  /** Notify the server of a terminal resize. */
  resize: (cols: number, rows: number) => void;
  /** Close the session and tear down the socket. */
  close: () => void;
}

const EXEC_SUBPROTOCOL = "rune.exec.v1";
const EXEC_BEARER_PREFIX = "rune.bearer.";

/**
 * openExecSession opens the exec WebSocket bridge and wires it to callbacks.
 * It sends the init frame on open, then pumps stdin/resize as ExecRequest
 * frames and decodes ExecResponse frames into text/exit/error callbacks.
 */
export async function openExecSession(opts: ExecSessionOpts): Promise<ExecSession> {
  await ensureFresh();
  const token = getAccessToken() || "";
  const enc = new TextEncoder();
  const dec = new TextDecoder();

  const origin = window.location.origin.replace(/^http/, "ws");
  const url = `${origin}/v1/exec/ws`;
  // The bearer token rides as a subprotocol; the negotiated one is the v1 tag.
  const protocols = token ? [EXEC_BEARER_PREFIX + token, EXEC_SUBPROTOCOL] : [EXEC_SUBPROTOCOL];
  const ws = new WebSocket(url, protocols);
  ws.binaryType = "arraybuffer";

  let closed = false;

  const sendFrame = (req: ExecRequest) => {
    if (ws.readyState === WebSocket.OPEN) ws.send(req.toBinary());
  };

  ws.onopen = () => {
    const init = new ExecInitRequest({
      target: opts.instanceId
        ? { case: "instanceId", value: opts.instanceId }
        : { case: "serviceName", value: opts.service || "" },
      namespace: opts.namespace || "default",
      command: opts.command,
      tty: opts.tty,
      terminalSize: new TerminalSize({ width: opts.cols, height: opts.rows }),
    });
    sendFrame(new ExecRequest({ request: { case: "init", value: init } }));
    opts.onOpen?.();
  };

  ws.onmessage = (ev) => {
    if (!(ev.data instanceof ArrayBuffer)) return;
    let resp: ExecResponse;
    try {
      resp = ExecResponse.fromBinary(new Uint8Array(ev.data));
    } catch (e) {
      opts.onError(e instanceof Error ? e : new Error(String(e)));
      return;
    }
    switch (resp.response.case) {
      case "stdout":
      case "stderr":
        opts.onData(dec.decode(resp.response.value));
        break;
      case "status":
        if (resp.response.value && resp.response.value.code && resp.response.value.code !== 0) {
          opts.onError(new Error(resp.response.value.message || `exec status ${resp.response.value.code}`));
        }
        break;
      case "exit": {
        const ex = resp.response.value;
        opts.onExit(ex.code, ex.signaled ? ex.signal : undefined);
        break;
      }
      default:
        break;
    }
  };

  ws.onerror = () => {
    if (!closed) opts.onError(new Error("exec connection error"));
  };

  ws.onclose = () => {
    if (!closed) opts.onExit(0);
    closed = true;
  };

  return {
    send: (text: string) => {
      sendFrame(new ExecRequest({ request: { case: "stdin", value: enc.encode(text) } }));
    },
    resize: (cols: number, rows: number) => {
      sendFrame(new ExecRequest({ request: { case: "resize", value: new TerminalSize({ width: cols, height: rows }) } }));
    },
    close: () => {
      closed = true;
      try { ws.close(); } catch { /* ignore */ }
    },
  };
}
