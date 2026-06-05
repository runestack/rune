/* RUNE-201 browser session.
 *
 * The browser never holds a refresh token (it lives in an HttpOnly cookie at
 * /v1/auth/refresh). We hold only a short-lived ACCESS token in memory and
 * exchange the cookie for a fresh one via POST /v1/auth/refresh.
 *
 * Login paths:
 *   - handoff:  `rune ui` sets the refresh cookie; ?handoff=<code> on the
 *               login page claims it, then we refresh.
 *   - cookie:   a prior session's cookie is still valid → refresh on load.
 *   - token:    pasted static/access token, held in memory (no refresh).
 */

let accessToken: string | null = null;
let expiresAt = 0; // unix seconds; 0 = no expiry (static token)
let mode: "none" | "cookie" | "token" = "none";

type Listener = () => void;
const listeners = new Set<Listener>();
function emit() {
  for (const l of listeners) l();
}
export function subscribe(l: Listener): () => void {
  listeners.add(l);
  return () => listeners.delete(l);
}

export function getAccessToken(): string | null {
  return accessToken;
}
export function isAuthed(): boolean {
  return accessToken != null;
}
export function sessionMode() {
  return mode;
}

function setSession(token: string | null, exp: number, m: typeof mode) {
  accessToken = token;
  expiresAt = exp;
  mode = m;
  emit();
}

/** Exchange the HttpOnly refresh cookie for a fresh access token. */
export async function refresh(): Promise<boolean> {
  try {
    const res = await fetch("/v1/auth/refresh", { method: "POST", credentials: "same-origin" });
    if (!res.ok) return false;
    const body = (await res.json()) as { access_token?: string; accessToken?: string; expires_at?: number; expiresAt?: number };
    const tok = body.access_token ?? body.accessToken;
    if (!tok) return false;
    setSession(tok, body.expires_at ?? body.expiresAt ?? 0, "cookie");
    return true;
  } catch {
    return false;
  }
}

/** Claim a one-time handoff code, which sets the refresh cookie, then refresh. */
export async function claimHandoff(code: string): Promise<boolean> {
  try {
    const res = await fetch(`/v1/ui/handoff/${encodeURIComponent(code)}`, { method: "GET", credentials: "same-origin" });
    // 204 = cookie set; some builds return the token in JSON instead.
    if (res.status === 204) return refresh();
    if (res.ok) {
      const body = (await res.json().catch(() => ({}))) as { token?: string };
      if (body.token) {
        setSession(body.token, 0, "token");
        return true;
      }
      return refresh();
    }
    return false;
  } catch {
    return false;
  }
}

/** Log in by pasting a static/access bearer token (held in memory only). */
export function loginWithToken(token: string) {
  setSession(token.trim(), 0, "token");
}

export function logout() {
  setSession(null, 0, "none");
}

/** Ensure a usable access token before a call; refresh cookie sessions that are
 *  near expiry. Returns false if we have nothing usable. */
export async function ensureFresh(): Promise<boolean> {
  if (!accessToken) return mode === "cookie" ? refresh() : false;
  if (mode === "cookie" && expiresAt > 0) {
    const now = Math.floor(Date.now() / 1000);
    if (now >= expiresAt - 30) return refresh();
  }
  return true;
}

/** Bootstrap the session on app load. Returns true if authenticated. */
export async function bootstrapSession(): Promise<boolean> {
  const params = new URLSearchParams(window.location.search);
  const handoff = params.get("handoff");
  if (handoff) {
    const ok = await claimHandoff(handoff);
    // strip the code from the URL regardless
    params.delete("handoff");
    const q = params.toString();
    window.history.replaceState({}, "", window.location.pathname + (q ? `?${q}` : "") + window.location.hash);
    if (ok) return true;
  }
  // Try an existing refresh cookie.
  return refresh();
}
