import { useState } from "react";
import { Button, Icon, Logo } from "../components";
import { loginWithToken } from "../api/session";
import { clients } from "../api/transport";
import type { LogoVariant } from "../lib/theme";
import "./Login.css";

export function Login({ logoVariant, onAuthed }: { logoVariant: LogoVariant; onAuthed: () => void }) {
  const [token, setToken] = useState("");
  const [err, setErr] = useState("");
  const [busy, setBusy] = useState(false);

  function submitToken() {
    if (!token.trim()) return;
    loginWithToken(token);
    onAuthed();
  }

  async function bootstrap() {
    setBusy(true);
    setErr("");
    try {
      const r = await clients.admin.adminBootstrap({});
      loginWithToken(r.tokenSecret);
      onAuthed();
    } catch (e) {
      setErr(
        "Bootstrap failed — a cluster admin already exists, or this isn't first run. " +
          "Paste a token from `rune admin token create`, or run `rune ui login`.",
      );
      setBusy(false);
    }
  }

  return (
    <div className="login">
      <div className="login-card">
        <div className="login-brand"><Logo variant={logoVariant} /></div>
        <h1>Sign in to <em>Rune</em></h1>
        <p className="login-sub">Authenticate with a Rune token, or hand off from the CLI. The dashboard uses the same identity and policies as the CLI.</p>

        <div className="login-field">
          <input
            value={token}
            onChange={(e) => setToken(e.target.value)}
            onKeyDown={(e) => e.key === "Enter" && submitToken()}
            placeholder="rune_… token"
            autoFocus
            spellCheck={false}
          />
          <Button variant="primary" onClick={submitToken} disabled={!token.trim()}>
            <Icon name="arrowup" size={14} style={{ transform: "rotate(90deg)" }} />Sign in
          </Button>
        </div>
        <div className="login-err">{err}</div>

        <div className="login-handoff">
          <b style={{ color: "var(--text)" }}>From the CLI</b><br />
          Run <code>rune ui login</code> — it opens this page with a one-time handoff and sets a secure session cookie. No token copy-paste.
        </div>

        <div className="login-or">first run?</div>
        <Button onClick={bootstrap} disabled={busy} style={{ width: "100%", justifyContent: "center" }}>
          <Icon name="bolt" size={14} />{busy ? "Bootstrapping…" : "Bootstrap cluster admin"}
        </Button>
      </div>
    </div>
  );
}
