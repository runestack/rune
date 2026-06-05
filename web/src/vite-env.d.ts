/// <reference types="vite/client" />

interface ImportMetaEnv {
  /** Set to "1" to include dev tools (Playground, Tweaks panel) in a build. */
  readonly VITE_DEVTOOLS?: string;
}

interface ImportMeta {
  readonly env: ImportMetaEnv;
}
