import { createContext, useCallback, useContext, useEffect, useState, type ReactNode } from "react";

/**
 * Active namespace scope, shared across the app. "all" means cluster-wide.
 * This is the in-cluster "context" a single session can switch freely (unlike
 * the cluster itself, which is bound to the origin that served the dashboard).
 *
 * Persisted two ways: a `?ns=` query param (so a scoped view is shareable /
 * linkable) and localStorage (so it survives reloads even without the param).
 * Precedence on load: URL param → localStorage → "all".
 */
export interface Scope {
  ns: string;
  setNs: (v: string) => void;
}

const KEY = "rune.scope.ns";
const ScopeCtx = createContext<Scope>({ ns: "all", setNs: () => {} });

export const useScope = () => useContext(ScopeCtx);

function readInitial(): string {
  try {
    const q = new URL(window.location.href).searchParams.get("ns");
    if (q) return q;
    return localStorage.getItem(KEY) || "all";
  } catch {
    return "all";
  }
}

// Reflect the scope in the URL without a navigation/reload, preserving the
// path, hash (e.g. #playground) and any other query params.
function writeUrl(ns: string) {
  try {
    const u = new URL(window.location.href);
    if (ns === "all") u.searchParams.delete("ns");
    else u.searchParams.set("ns", ns);
    window.history.replaceState({}, "", u.pathname + u.search + u.hash);
  } catch {
    /* ignore */
  }
}

export function ScopeProvider({ children }: { children: ReactNode }) {
  const [ns, setNsState] = useState<string>(readInitial);

  // Make the URL match the resolved initial scope (e.g. when it came from
  // localStorage) so the address bar is shareable from the first paint.
  useEffect(() => { writeUrl(ns); }, []); // eslint-disable-line react-hooks/exhaustive-deps

  const setNs = useCallback((v: string) => {
    setNsState(v);
    try { localStorage.setItem(KEY, v); } catch { /* ignore */ }
    writeUrl(v);
  }, []);

  return <ScopeCtx.Provider value={{ ns, setNs }}>{children}</ScopeCtx.Provider>;
}
