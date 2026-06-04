import { StrictMode } from "react";
import { createRoot } from "react-dom/client";
import "./styles/tokens.css";
import "./styles/base.css";
import { Playground } from "./playground/Playground";

// Slice 1: the entry renders the component Playground. The full dashboard app
// (shell + screens) lands next and will route the playground under #/playground.
createRoot(document.getElementById("root")!).render(
  <StrictMode>
    <Playground />
  </StrictMode>,
);
