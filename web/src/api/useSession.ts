import { useEffect, useState } from "react";
import { bootstrapSession, isAuthed, logout as doLogout, sessionMode, subscribe } from "./session";
import { clients } from "./transport";

export interface WhoAmI {
  subjectId: string;
  subjectName: string;
  subjectEmail: string;
  policies: string[];
}

export type SessionPhase = "loading" | "authed" | "anon";

export interface SessionState {
  phase: SessionPhase;
  who: WhoAmI | null;
  mode: "none" | "cookie" | "token";
  reload: () => void;
  logout: () => void;
}

export function useSession(): SessionState {
  const [phase, setPhase] = useState<SessionPhase>("loading");
  const [who, setWho] = useState<WhoAmI | null>(null);
  const [, force] = useState(0);

  useEffect(() => subscribe(() => force((n) => n + 1)), []);

  useEffect(() => {
    let alive = true;
    (async () => {
      const ok = isAuthed() || (await bootstrapSession());
      if (!alive) return;
      if (!ok) {
        setPhase("anon");
        return;
      }
      try {
        const r = await clients.auth.whoAmI({});
        if (!alive) return;
        setWho({ subjectId: r.subjectId, subjectName: r.subjectName, subjectEmail: r.subjectEmail, policies: r.policies });
        setPhase("authed");
      } catch {
        if (alive) setPhase("anon");
      }
    })();
    return () => {
      alive = false;
    };
    // re-run when something forces (login/logout)
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [phase === "loading"]);

  function reload() {
    setWho(null);
    setPhase("loading");
  }
  function logout() {
    doLogout();
    setWho(null);
    setPhase("anon");
  }

  return { phase, who, mode: sessionMode(), reload, logout };
}
