/**
 * RUNE-200 transport CAPABILITY check (logs/exec investigation).
 *
 * Answers: can the dashboard, using the SAME browser transport the SPA uses
 * (@connectrpc/connect-web), actually drive logs and exec?
 *
 * The result matters because LogService.StreamLogs and ExecService.StreamExec
 * are declared BiDiStreaming. Browsers cannot do full-duplex over fetch, so
 * connect-web (and gRPC-Web) cannot call bidi methods at all — regardless of
 * how the server behaves internally.
 *
 * This script contrasts, against a live runed:
 *   - server-streaming (WatchNamespaces) via the BROWSER transport  -> works
 *   - bidi (StreamLogs, StreamExec)       via the BROWSER transport  -> rejected
 *   - bidi (StreamLogs)                    via a NODE h2 transport     -> reachable
 *
 * Run: RUNE_URL=http://127.0.0.1:7861/grpc npx tsx scripts/streamcheck.ts
 */
import { createConnectTransport as webTransport } from "@connectrpc/connect-web";
import { createGrpcTransport } from "@connectrpc/connect-node";
import { createPromiseClient, type Interceptor, Code, ConnectError } from "@connectrpc/connect";
import { AdminService } from "../src/gen/pkg/api/proto/admin_connect.js";
import { NamespaceService } from "../src/gen/pkg/api/proto/namespace_connect.js";
import { LogService } from "../src/gen/pkg/api/proto/logs_connect.js";
import { ExecService } from "../src/gen/pkg/api/proto/exec_connect.js";
import { LogRequest } from "../src/gen/pkg/api/proto/logs_pb.js";
import { ExecRequest } from "../src/gen/pkg/api/proto/exec_pb.js";

const baseUrl = process.env.RUNE_URL ?? "http://127.0.0.1:7861/grpc";

async function* once<T>(msg: T): AsyncIterable<T> {
  yield msg;
}

function authInterceptor(token: string): Interceptor {
  return (next) => async (req) => {
    if (token) req.header.set("Authorization", `Bearer ${token}`);
    req.header.set("x-rune-client", "ui");
    return next(req);
  };
}

async function main() {
  console.log(`[streamcheck] target ${baseUrl}\n`);

  // Bootstrap a root token via the browser transport (unary works).
  const boot = await createPromiseClient(
    AdminService,
    webTransport({ baseUrl, interceptors: [authInterceptor("")] }),
  ).adminBootstrap({});
  const token = boot.tokenSecret;

  const web = webTransport({ baseUrl, interceptors: [authInterceptor(token)] });
  // connect-node grpc transport speaks real gRPC over h2c — a bidi-capable
  // (non-browser) client, e.g. what a server-side WebSocket bridge would use.
  const node = createGrpcTransport({ baseUrl, interceptors: [authInterceptor(token)] });

  const results: Record<string, string> = {};

  // 1. server-streaming via BROWSER transport — expected to work.
  try {
    const nsc = createPromiseClient(NamespaceService, web);
    let n = 0;
    for await (const _ of nsc.watchNamespaces({})) {
      if (++n >= 1) break;
    }
    results["server-streaming (WatchNamespaces) via connect-web"] = `OK (${n} frame)`;
  } catch (e) {
    results["server-streaming (WatchNamespaces) via connect-web"] =
      `FAILED: ${(e as Error).message}`;
  }

  // 2. bidi StreamLogs via BROWSER transport — expected to be rejected.
  try {
    const logs = createPromiseClient(LogService, web);
    const stream = logs.streamLogs(
      once(new LogRequest({ resourceTarget: "any", namespace: "default", follow: false, tail: 10 })),
    );
    for await (const _ of stream) break;
    results["bidi (StreamLogs) via connect-web"] = "UNEXPECTEDLY OK";
  } catch (e) {
    const ce = ConnectError.from(e);
    results["bidi (StreamLogs) via connect-web"] = `REJECTED [${Code[ce.code]}]: ${ce.rawMessage}`;
  }

  // 3. bidi StreamExec via BROWSER transport — expected to be rejected.
  try {
    const exec = createPromiseClient(ExecService, web);
    const stream = exec.streamExec(once(new ExecRequest({})));
    for await (const _ of stream) break;
    results["bidi (StreamExec) via connect-web"] = "UNEXPECTEDLY OK";
  } catch (e) {
    const ce = ConnectError.from(e);
    results["bidi (StreamExec) via connect-web"] = `REJECTED [${Code[ce.code]}]: ${ce.rawMessage}`;
  }

  // 4. bidi StreamLogs via NODE h2 transport — transport supports bidi; this
  //    reaches the server (which then errors on a bogus target). Proves the
  //    server path is fine and a bidi-capable proxy could drive it.
  try {
    const logs = createPromiseClient(LogService, node);
    const stream = logs.streamLogs(
      once(new LogRequest({ resourceTarget: "any", namespace: "default", follow: false, tail: 10 })),
    );
    for await (const _ of stream) break;
    results["bidi (StreamLogs) via connect-node h2"] = "OK (stream opened, no logs)";
  } catch (e) {
    const ce = ConnectError.from(e);
    results["bidi (StreamLogs) via connect-node h2"] =
      `reached server, code [${Code[ce.code]}]: ${ce.rawMessage}`;
  }

  console.log("RESULTS");
  console.log("=".repeat(72));
  for (const [k, v] of Object.entries(results)) {
    console.log(`  ${k}\n     → ${v}\n`);
  }
}

main().catch((e) => {
  console.error("[streamcheck] error:", e?.message ?? e);
  process.exit(1);
});
