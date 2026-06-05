import { lazy, StrictMode, Suspense } from "react";
import { createRoot } from "react-dom/client";
import "./styles/fonts";
import "./styles/tokens.css";
import "./styles/base.css";
import { App } from "./App";
import { Login } from "./screens/Login";
import { useSession } from "./api/useSession";
import { useTweaks } from "./lib/theme";
import { DEVTOOLS } from "./lib/devtools";
import { ErrorBoundary, Logo } from "./components";

// The component Playground is a dev-only surface. Lazy + gated so it is
// tree-shaken out of production builds (no chunk emitted when DEVTOOLS is off).
const Playground = DEVTOOLS
  ? lazy(() => import("./playground/Playground").then((m) => ({ default: m.Playground })))
  : null;

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

  if (Playground && window.location.hash.replace(/^#\/?/, "") === "playground")
    return <Suspense fallback={<LoadingSplash />}><Playground /></Suspense>;

  if (s.phase === "loading") return <LoadingSplash />;

  if (s.phase === "authed") {
    const user = { name: s.who?.subjectName || s.who?.subjectId || "user", role: s.who?.policies[0] || "authenticated" };
    return <App user={user} onLogout={s.logout} />;
  }

  return <Login logoVariant="wordmark" onAuthed={s.reload} />;
}

// Cache the React root on the container element so Vite HMR re-evaluating this
// entry module reuses it instead of calling createRoot() on an already-rooted
// node — which logs "createRoot() on a container that has already been passed
// to createRoot()" and leaks the previous root.
const container = document.getElementById("root")! as HTMLElement & {
  _reactRoot?: ReturnType<typeof createRoot>;
};
const root = container._reactRoot ?? (container._reactRoot = createRoot(container));
root.render(
  <StrictMode>
    <ErrorBoundary>
      <Root />
    </ErrorBoundary>
  </StrictMode>,
);

window.addEventListener("hashchange", () => window.location.reload());
