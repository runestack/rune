import { useState } from "react";
import { Button, Icon, Logo } from "../components";
import { loginWithToken } from "../api/session";
import type { LogoVariant } from "../lib/theme";
import "./Login.css";

export function Login({ logoVariant, onAuthed }: { logoVariant: LogoVariant; onAuthed: () => void }) {
  const [token, setToken] = useState("");

  function submitToken() {
    if (!token.trim()) return;
    loginWithToken(token);
    onAuthed();
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

        <div className="login-handoff">
          <b style={{ color: "var(--text)" }}>From the CLI</b><br />
          Run <code>rune ui</code> — it opens this page with a one-time handoff and sets a secure session cookie. No token copy-paste.
        </div>

        <div className="login-or">first run?</div>
        <div className="login-handoff">
          Bootstrap runs on the server only, not from the browser. Run <code>rune admin bootstrap</code> on the host to mint the cluster-admin token, then paste it above (or use <code>rune ui</code>).
        </div>
      </div>
    </div>
  );
}
