/**
 * Whether dev tooling — the component Playground (`#playground`) and the live
 * Tweaks theme panel — is included in the build.
 *
 * In a production `vite build` this folds to the compile-time constant `false`,
 * so the gated UI and Playground's lazy chunk are tree-shaken out of the
 * shipped bundle. A build can opt the tools back in with `VITE_DEVTOOLS=1`
 * (e.g. for an internal/staging artifact).
 */
export const DEVTOOLS: boolean = import.meta.env.DEV || import.meta.env.VITE_DEVTOOLS === "1";
