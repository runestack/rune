import type { LogoVariant } from "../../../lib/theme";

/** Angular bind-rune (raido-inspired): geometric straight strokes only. */
export function RuneMark({ size = 26 }: { size?: number }) {
  return (
    <svg
      width={size}
      height={size}
      viewBox="0 0 24 24"
      fill="none"
      aria-hidden="true"
      stroke="currentColor"
      strokeWidth="2"
      strokeLinecap="round"
      strokeLinejoin="round"
    >
      <path d="M7 3v18" />
      <path d="M7 4l8 4.5L7 13" />
      <path d="M11 11l5 9" />
    </svg>
  );
}

const word = (
  <span
    style={{
      fontFamily: "var(--serif)",
      fontSize: 21,
      fontWeight: 500,
      letterSpacing: "-0.01em",
      color: "var(--text)",
    }}
  >
    Rune
  </span>
);

export interface LogoProps {
  variant?: LogoVariant;
  compact?: boolean;
}

export function Logo({ variant = "wordmark", compact = false }: LogoProps) {
  if (variant === "wordmark") {
    return (
      <div className="logo-lockup" style={{ display: "flex", alignItems: "baseline", gap: 0 }}>
        <span
          style={{
            fontFamily: "var(--serif)",
            fontSize: 24,
            fontWeight: 400,
            letterSpacing: "-0.005em",
            color: "var(--text)",
          }}
        >
          rune
        </span>
        <span style={{ color: "var(--accent)", fontSize: 24, fontWeight: 500, lineHeight: 1 }}>.</span>
      </div>
    );
  }
  if (variant === "mono") {
    return (
      <div
        className="logo-lockup"
        style={{
          display: "flex",
          alignItems: "center",
          gap: 0,
          fontFamily: "var(--mono)",
          fontSize: 15,
          fontWeight: 600,
          color: "var(--text)",
        }}
      >
        <span style={{ color: "var(--accent)" }}>~/</span>
        <span>rune</span>
        <span
          style={{
            width: 8,
            height: 16,
            background: "var(--accent)",
            marginLeft: 3,
            borderRadius: 1,
            display: "inline-block",
          }}
        />
      </div>
    );
  }
  if (variant === "tile") {
    return (
      <div className="logo-lockup" style={{ display: "flex", alignItems: "center", gap: 11 }}>
        <div
          style={{
            width: 32,
            height: 32,
            borderRadius: 9,
            background: "var(--accent)",
            color: "#15121f",
            display: "grid",
            placeItems: "center",
            boxShadow: "0 2px 10px rgba(158,140,252,.35)",
          }}
        >
          <RuneMark size={20} />
        </div>
        {!compact && word}
      </div>
    );
  }
  // default "mark": outlined rune in accent + serif word
  return (
    <div className="logo-lockup" style={{ display: "flex", alignItems: "center", gap: 10 }}>
      <div
        style={{
          color: "var(--accent)",
          display: "grid",
          placeItems: "center",
          width: 30,
          height: 30,
          border: "1.5px solid var(--accent-line)",
          borderRadius: 9,
          background: "var(--accent-dim)",
        }}
      >
        <RuneMark size={18} />
      </div>
      {!compact && word}
    </div>
  );
}
