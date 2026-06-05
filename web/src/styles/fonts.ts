/**
 * Self-hosted fonts (latin subset, only the weights the design uses).
 *
 * These ship inside the runed binary alongside the bundle. The dashboard is
 * served under a strict CSP (`font-src 'self'`, `style-src 'self'`), so fonts
 * MUST be same-origin — loading them from the Google Fonts CDN is blocked in
 * production. @fontsource emits same-origin @font-face rules with
 * `font-display: swap`, which Vite fingerprints into /ui/assets.
 */

// Spectral (serif) — titles, logo wordmark. Roman + italic for <em>.
import "@fontsource/spectral/latin-400.css";
import "@fontsource/spectral/latin-500.css";
import "@fontsource/spectral/latin-600.css";
import "@fontsource/spectral/latin-400-italic.css";
import "@fontsource/spectral/latin-500-italic.css";

// Hanken Grotesk (sans) — body / UI.
import "@fontsource/hanken-grotesk/latin-400.css";
import "@fontsource/hanken-grotesk/latin-500.css";
import "@fontsource/hanken-grotesk/latin-600.css";
import "@fontsource/hanken-grotesk/latin-700.css";

// JetBrains Mono (mono) — code, logs, terminal, metrics.
import "@fontsource/jetbrains-mono/latin-400.css";
import "@fontsource/jetbrains-mono/latin-500.css";
import "@fontsource/jetbrains-mono/latin-600.css";
