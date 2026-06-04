import { StrictMode } from "react";
import { createRoot } from "react-dom/client";
import "./styles/tokens.css";
import "./styles/base.css";
import { App } from "./App";
import { Login } from "./screens/Login";
import { Playground } from "./playground/Playground";
import { useSession } from "./api/useSession";
import { useTweaks } from "./lib/theme";
import { Logo } from "./components";

function LoadingSplash() {
  return (
    <div style={{ height: "100vh", display: "grid", placeItems: "center", background: "var(--bg)" }}>
      <div style={{ opacity: 0.5, animation: "dotpulse 1.6s ease-in-out infinite" }}>
        <Logo variant="wordmark" />
      </div>
    </div>
  );
}

function Root() {
  // Apply the persisted theme (accent/edges) before any screen renders.
  useTweaks();
  const s = useSession();

  if (window.location.hash.replace(/^#\/?/, "") === "playground") return <Playground />;

  if (s.phase === "loading") return <LoadingSplash />;

  if (s.phase === "authed" || s.demo) {
    const user = s.demo
      ? { name: "demo", role: "sample data" }
      : { name: s.who?.subjectName || s.who?.subjectId || "user", role: s.who?.policies[0] || "authenticated" };
    return <App user={user} demo={s.demo} onLogout={s.logout} />;
  }

  return <Login logoVariant="wordmark" onAuthed={s.reload} onDemo={() => s.setDemo(true)} />;
}

createRoot(document.getElementById("root")!).render(
  <StrictMode>
    <Root />
  </StrictMode>,
);

window.addEventListener("hashchange", () => window.location.reload());
