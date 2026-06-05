import { Component, type ErrorInfo, type ReactNode } from "react";

interface Props {
  children: ReactNode;
  /** Custom fallback; receives the error and a reset() to retry rendering. */
  fallback?: (error: Error, reset: () => void) => ReactNode;
}
interface State {
  error: Error | null;
}

/**
 * Top-level error boundary. A render-time throw anywhere below would otherwise
 * white-screen the whole dashboard; this catches it and shows a recoverable
 * fallback. Errors are logged to the console for diagnostics.
 */
export class ErrorBoundary extends Component<Props, State> {
  state: State = { error: null };

  static getDerivedStateFromError(error: Error): State {
    return { error };
  }

  componentDidCatch(error: Error, info: ErrorInfo) {
    console.error("Dashboard render error:", error, info.componentStack);
  }

  reset = () => this.setState({ error: null });

  render() {
    const { error } = this.state;
    if (!error) return this.props.children;
    if (this.props.fallback) return this.props.fallback(error, this.reset);
    return <DefaultFallback error={error} onReset={this.reset} />;
  }
}

// Inline-styled so it renders even if the app stylesheet failed to load.
function DefaultFallback({ error, onReset }: { error: Error; onReset: () => void }) {
  return (
    <div style={{ height: "100vh", display: "grid", placeItems: "center", background: "#161513", color: "#ece8e1", fontFamily: "ui-sans-serif, system-ui, sans-serif", padding: 24 }}>
      <div style={{ maxWidth: 440, textAlign: "center" }}>
        <div style={{ fontFamily: "Georgia, serif", fontSize: 26, fontWeight: 500, marginBottom: 10 }}>
          rune<span style={{ color: "#9e8cfc" }}>.</span>
        </div>
        <h1 style={{ fontSize: 16, fontWeight: 600, margin: "0 0 8px" }}>Something went wrong</h1>
        <p style={{ fontSize: 13, lineHeight: 1.55, color: "#a6a097", margin: "0 0 18px" }}>
          The dashboard hit an unexpected error and stopped rendering. Reloading usually clears it.
        </p>
        <pre style={{ fontFamily: "ui-monospace, monospace", fontSize: 11.5, color: "#726c63", background: "#131210", border: "1px solid #2c2925", borderRadius: 8, padding: "9px 12px", overflow: "auto", textAlign: "left", marginBottom: 18 }}>
          {error.message || String(error)}
        </pre>
        <div style={{ display: "flex", gap: 10, justifyContent: "center" }}>
          <button onClick={onReset} style={{ fontFamily: "inherit", fontSize: 13, fontWeight: 600, padding: "7px 13px", borderRadius: 8, cursor: "pointer", border: "1px solid #3a3631", background: "#262320", color: "#ece8e1" }}>
            Try again
          </button>
          <button onClick={() => window.location.reload()} style={{ fontFamily: "inherit", fontSize: 13, fontWeight: 600, padding: "7px 13px", borderRadius: 8, cursor: "pointer", border: "1px solid #9e8cfc", background: "#9e8cfc", color: "#15121f" }}>
            Reload
          </button>
        </div>
      </div>
    </div>
  );
}
