/**
 * RUNE-200 streaming de-risk smoke (Phase 2 spike).
 *
 * Proves the dashboard's real transport works end-to-end against a live `runed`
 * using the SAME Connect stack the browser SPA will use (@connectrpc/connect +
 * generated clients), exercising both call shapes through the vanguard
 * transcoder on /grpc:
 *
 *   1. UNARY            AdminService.AdminBootstrap (auth-exempt) -> root token
 *                       AuthService.WhoAmI (authenticated)
 *   2. SERVER-STREAMING NamespaceService.WatchNamespaces -> ADDED events stream
 *
 * Connect server-streaming over HTTP/1.1 is exactly what connect-web uses in a
 * browser over plaintext, so a pass here closes the Phase-1 residual risk that
 * vanguard might not stream correctly. (True client/bidi streaming — exec — is
 * NOT validated here: browsers can't do bidi over HTTP, so exec will need a
 * WebSocket bridge in Phase 4. That's a known constraint, not a regression.)
 *
 * Run:  RUNE_URL=http://127.0.0.1:7861/grpc npm run smoke
 * Env:  RUNE_URL   transcoder base (default http://127.0.0.1:7861/grpc)
 *       RUNE_TOKEN optional; if set, skips AdminBootstrap and uses this token
 */
import { createConnectTransport } from "@connectrpc/connect-node";
import { createPromiseClient, type Interceptor } from "@connectrpc/connect";
import { AdminService } from "../src/gen/pkg/api/proto/admin_connect.js";
import { AuthService } from "../src/gen/pkg/api/proto/auth_connect.js";
import { NamespaceService } from "../src/gen/pkg/api/proto/namespace_connect.js";

const baseUrl = process.env.RUNE_URL ?? "http://127.0.0.1:7861/grpc";

function makeTransport(token: string) {
  const auth: Interceptor = (next) => async (req) => {
    if (token) req.header.set("Authorization", `Bearer ${token}`);
    req.header.set("x-rune-client", "ui"); // audit source=ui
    return next(req);
  };
  return createConnectTransport({
    baseUrl,
    httpVersion: "1.1", // mirrors connect-web over plaintext (browser h1.1)
    interceptors: [auth],
  });
}

async function main() {
  console.log(`[smoke] target ${baseUrl}`);

  // ---- obtain a token (self-bootstrap unless one is provided) -------------
  let token = process.env.RUNE_TOKEN ?? "";
  if (!token) {
    const admin = createPromiseClient(AdminService, makeTransport(""));
    const boot = await admin.adminBootstrap({});
    token = boot.tokenSecret;
    console.log(`[smoke] UNARY AdminBootstrap OK — subject=${boot.subjectId}`);
  } else {
    console.log("[smoke] using RUNE_TOKEN from env");
  }

  const transport = makeTransport(token);

  // ---- 1. unary -----------------------------------------------------------
  const authc = createPromiseClient(AuthService, transport);
  const who = await authc.whoAmI({});
  console.log(
    `[smoke] UNARY WhoAmI OK — subject=${who.subjectId} policies=[${who.policies.join(",")}]`,
  );

  // ---- 2. server-streaming ------------------------------------------------
  const nsc = createPromiseClient(NamespaceService, transport);
  let frames = 0;
  const ac = new AbortController();
  const timeout = setTimeout(() => ac.abort(), 5000);
  try {
    for await (const ev of nsc.watchNamespaces({}, { signal: ac.signal })) {
      frames++;
      console.log(
        `[smoke] STREAM frame ${frames}: namespace=${ev.namespace?.name} event=${ev.eventType}`,
      );
      if (frames >= 2) break; // system + default proves the stream flows
    }
  } catch (e) {
    // An abort after we already received frames is fine; only fail if none.
    if (frames === 0) throw e;
  } finally {
    clearTimeout(timeout);
  }

  if (frames < 1) {
    console.error("[smoke] FAIL: no server-streamed frames received");
    process.exit(1);
  }
  console.log(`[smoke] SERVER-STREAMING OK — ${frames} frame(s) over the transcoder`);
  console.log("[smoke] PASS ✅");
}

main().catch((e) => {
  console.error("[smoke] FAIL ❌:", e?.message ?? e);
  process.exit(1);
});
