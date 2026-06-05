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
export function rgba(hex: string, a: number): string {
  const [r, g, b] = hexToRgb(hex);
  return `rgba(${r}, ${g}, ${b}, ${a})`;
}

export type LogoVariant = "mark" | "tile" | "wordmark" | "mono";
export type Edges = "soft" | "crisp" | "sharp";

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
  root.style.setProperty("--accent-text", mix(t.accent, 0.32));
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
