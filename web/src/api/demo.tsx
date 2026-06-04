import { createContext, useContext } from "react";

/**
 * DemoContext carries the single "demo mode" flag down the tree so data hooks
 * can branch (mock vs live) without prop-drilling. `demo === true` means we are
 * browsing the bundled sample dataset with no backend session; `false` means we
 * are authenticated against a real runed and should call live clients.
 */
const DemoContext = createContext(false);

export const DemoProvider = DemoContext.Provider;

/** Read the current demo-mode flag. Defaults to false (live) outside a provider. */
export function useDemo(): boolean {
  return useContext(DemoContext);
}
