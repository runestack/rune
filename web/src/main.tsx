import { StrictMode } from "react";
import { createRoot } from "react-dom/client";
import "./styles/tokens.css";
import "./styles/base.css";
import { App } from "./App";
import { Playground } from "./playground/Playground";

// The dashboard is the default; the component playground lives at #/playground.
function Root() {
  const isPlayground = window.location.hash.replace(/^#\/?/, "") === "playground";
  return isPlayground ? <Playground /> : <App />;
}

createRoot(document.getElementById("root")!).render(
  <StrictMode>
    <Root />
  </StrictMode>,
);

// Re-render on hash change so #/playground toggles without a reload.
window.addEventListener("hashchange", () => window.location.reload());
