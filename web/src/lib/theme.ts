import { useCallback, useEffect, useState } from "react";
import { DEVTOOLS } from "./devtools";

/* hex helpers for live accent recolor */
export function hexToRgb(h: string): [number, number, number] {
  h = h.replace("#", "");
  if (h.length === 3)
    h = h
      .split("")
      .map((c) => c + c)
      .join("");
  return [
    parseInt(h.slice(0, 2), 16),
    parseInt(h.slice(2, 4), 16),
    parseInt(h.slice(4, 6), 16),
  ];
}
export function mix(hex: string, withWhite: number): string {
  const [r, g, b] = hexToRgb(hex);
  const m = (c: number) => Math.round(c + (255 - c) * withWhite);
  return `rgb(${m(r)}, ${m(g)}, ${m(b)})`;
}
/** Darken a hex toward black by `amount` (0..1). */
export function shade(hex: string, towardBlack: number): string {
  const [r, g, b] = hexToRgb(hex);
  const m = (c: number) => Math.round(c * (1 - towardBlack));
  return `rgb(${m(r)}, ${m(g)}, ${m(b)})`;
}
/**
 * accentText derives the accent color used for accent-on-surface TEXT
 * (active nav item, page-title em, the scope chip). On dark/sand surfaces it is
 * LIGHTENED so it pops against the dark background; on the light (paper) theme
 * it must be DARKENED instead, or it washes out on white. Driven by the live
 * html[data-theme] so it tracks the selected theme.
 */
export function accentText(accent: string): string {
  const light = typeof document !== "undefined" && document.documentElement.getAttribute("data-theme") === "light";
  return light ? shade(accent, 0.4) : mix(accent, 0.32);
}
export function rgba(hex: string, a: number): string {
  const [r, g, b] = hexToRgb(hex);
  return `rgba(${r}, ${g}, ${b}, ${a})`;
}

export type LogoVariant = "mark" | "tile" | "wordmark" | "mono";
export type Edges = "soft" | "crisp" | "sharp";

/* ── Theme mode (base surface palette) ───────────────────────────────
   Selectable surface palette, applied via html[data-theme]. The accent
   tokens stay owned by useTweaks; these only swap the neutral surfaces.
   "dark" is the :root default (no attribute needed); "light"/"sand" are
   token overrides in styles/tokens.css. The swatch color (`sw`) is what
   the footer theme picker shows. */
export type ThemeMode = "dark" | "light" | "sand";

export interface ThemeOption {
  id: ThemeMode;
  label: string;
  /** Swatch fill in the picker (Sand uses a warm sand color, not its bg). */
  sw: string;
}

export const THEMES: ThemeOption[] = [
  { id: "dark", label: "Dark", sw: "#0f0f0f" },
  { id: "light", label: "Light", sw: "#ffffff" },
  { id: "sand", label: "Sand", sw: "#d9b487" },
];

const THEME_STORAGE_KEY = "rune.theme";

/** Apply the base surface palette by setting html[data-theme]. Dark is the
 *  default and clears the attribute so :root governs. */
export function applyThemeMode(mode: ThemeMode): void {
  const root = document.documentElement;
  if (mode === "dark") root.removeAttribute("data-theme");
  else root.setAttribute("data-theme", mode);
  // --accent-text is theme-dependent (lighten on dark, darken on light), so
  // re-derive it from the live accent now that the mode changed.
  const accent = root.style.getPropertyValue("--accent").trim() || TWEAK_DEFAULTS.accent;
  root.style.setProperty("--accent-text", accentText(accent));
}

function readThemeMode(): ThemeMode {
  try {
    const raw = localStorage.getItem(THEME_STORAGE_KEY);
    if (raw === "light" || raw === "sand" || raw === "dark") return raw;
  } catch {
    /* ignore */
  }
  return "dark";
}

/** Theme-mode state, persisted to localStorage and applied to <html>. */
export function useThemeMode(): [ThemeMode, (m: ThemeMode) => void] {
  const [mode, setMode] = useState<ThemeMode>(readThemeMode);

  useEffect(() => {
    applyThemeMode(mode);
    try {
      localStorage.setItem(THEME_STORAGE_KEY, mode);
    } catch {
      /* ignore */
    }
  }, [mode]);

  return [mode, setMode];
}

export interface Tweaks {
  logo: LogoVariant;
  accent: string;
  edges: Edges;
}

export const TWEAK_DEFAULTS: Tweaks = {
  logo: "wordmark",
  accent: "#9e8cfc",
  edges: "crisp",
};

export const EDGE_MAP: Record<Edges, { r: string; s: string; l: string }> = {
  soft: { r: "12px", s: "8px", l: "18px" },
  crisp: { r: "8px", s: "5px", l: "12px" },
  sharp: { r: "3px", s: "2px", l: "5px" },
};

const STORAGE_KEY = "rune.tweaks";

/** Apply accent + edge tokens to :root live. */
export function applyTheme(t: Pick<Tweaks, "accent" | "edges">): void {
  const root = document.documentElement;
  root.style.setProperty("--accent", t.accent);
  root.style.setProperty("--accent-text", accentText(t.accent));
  root.style.setProperty("--accent-dim", rgba(t.accent, 0.13));
  root.style.setProperty("--accent-line", rgba(t.accent, 0.34));
  const e = EDGE_MAP[t.edges] || EDGE_MAP.crisp;
  root.style.setProperty("--radius", e.r);
  root.style.setProperty("--radius-sm", e.s);
  root.style.setProperty("--radius-lg", e.l);
}

/** Tweaks state, persisted to localStorage, applied to :root on change. */
export function useTweaks(): [Tweaks, <K extends keyof Tweaks>(k: K, v: Tweaks[K]) => void] {
  const [t, setT] = useState<Tweaks>(() => {
    // Production always uses the canonical defaults — tweaks are a dev tool, so
    // their persisted state is ignored unless dev tooling is enabled.
    if (DEVTOOLS) {
      try {
        const raw = localStorage.getItem(STORAGE_KEY);
        if (raw) return { ...TWEAK_DEFAULTS, ...JSON.parse(raw) };
      } catch {
        /* ignore */
      }
    }
    return TWEAK_DEFAULTS;
  });

  useEffect(() => {
    applyTheme(t);
    if (!DEVTOOLS) return;
    try {
      localStorage.setItem(STORAGE_KEY, JSON.stringify(t));
    } catch {
      /* ignore */
    }
  }, [t]);

  const setTweak = useCallback(<K extends keyof Tweaks>(k: K, v: Tweaks[K]) => {
    setT((prev) => ({ ...prev, [k]: v }));
  }, []);

  return [t, setTweak];
}
