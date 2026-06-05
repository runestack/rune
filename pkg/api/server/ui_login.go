package server

import (
	"fmt"
	"net/http"
)

// uiLoginHandler serves a deliberately minimal sign-in page (RUNE-201). It is a
// stopgap until the full dashboard SPA ships its own login route: it implements
// only the CLI→browser handoff claim — read ?handoff=<code>, GET the handoff
// endpoint (which sets the HttpOnly refresh cookie), and report success. No
// build step, no framework; the full SPA will replace it.
func (s *APIServer) uiLoginHandler(mountPath string) http.HandlerFunc {
	page := fmt.Sprintf(uiLoginPageTemplate, mountPath)
	return func(w http.ResponseWriter, r *http.Request) {
		// Self-contained page: allow its own inline script/style, restrict the
		// rest, and forbid framing. The full SPA will use a stricter, nonce- or
		// hash-based CSP without 'unsafe-inline'.
		w.Header().Set("Content-Security-Policy", "default-src 'none'; script-src 'unsafe-inline'; style-src 'unsafe-inline'; img-src 'self'; connect-src 'self'")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(page))
	}
}

// uiLoginPageTemplate has one verb (%[1]s, used twice): the dashboard mount
// path (e.g. "/ui"). Standalone, brand-matched to the dashboard (warm near-black
// surface, serif "rune." wordmark, accent dot). No web fonts: this page is
// served under a strict default-src 'none' CSP before the SPA loads, so it uses
// system serif/sans stacks that echo Spectral / Hanken Grotesk. Avoid literal
// '%' in the CSS below — it is run through fmt.Sprintf.
const uiLoginPageTemplate = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<link rel="icon" type="image/svg+xml" href="%[1]s/favicon.svg">
<title>Rune — Sign in</title>
<style>
  :root {
    --bg: #161513; --surface: #1f1d1b; --border: #2c2925; --border-strong: #3a3631;
    --text: #ece8e1; --text-2: #a6a097; --text-3: #726c63;
    --accent: #9e8cfc; --accent-ink: #1a1626; --fail: #e5484d;
    --serif: "Spectral", Georgia, "Times New Roman", serif;
    --sans: "Hanken Grotesk", ui-sans-serif, system-ui, -apple-system, sans-serif;
    --mono: "JetBrains Mono", ui-monospace, "SF Mono", Menlo, monospace;
  }
  * { box-sizing: border-box; }
  body {
    margin: 0; min-height: 100vh; display: grid; place-items: center;
    font-family: var(--sans); font-size: 15px; line-height: 1.5;
    color: var(--text); background: var(--bg);
    background-image: radial-gradient(60vw 60vw at 50vw -10vh, rgba(158,140,252,0.06), transparent 70vh);
    -webkit-font-smoothing: antialiased;
  }
  main {
    width: 24rem; max-width: calc(100vw - 2.5rem); padding: 2.25rem 2rem 2rem;
    text-align: center; background: var(--surface);
    border: 1px solid var(--border); border-radius: 14px;
    box-shadow: 0 1px 2px rgba(0,0,0,0.4), 0 16px 50px rgba(0,0,0,0.35);
  }
  .brand { font-family: var(--serif); font-size: 30px; font-weight: 400; letter-spacing: -0.01em; }
  .brand .dot { color: var(--accent); font-weight: 500; }
  .rule { height: 1px; margin: 1.25rem auto; background: var(--border); }
  #status { margin: 0; color: var(--text-2); min-height: 1.5rem; }
  #status.err { color: var(--fail); }
  .spin {
    width: 18px; height: 18px; margin: 0 auto 0.9rem; border-radius: 999px;
    border: 2px solid var(--border-strong); border-top-color: var(--accent);
    animation: spin 0.7s linear infinite;
  }
  @keyframes spin { to { transform: rotate(360deg); } }
  a.button {
    display: none; margin-top: 1.5rem; padding: 0.6rem 1.1rem;
    font-family: var(--sans); font-size: 14px; font-weight: 500;
    background: var(--accent); color: var(--accent-ink);
    border-radius: 8px; text-decoration: none;
    box-shadow: 0 2px 12px rgba(158,140,252,0.28);
    transition: filter 0.12s ease, transform 0.12s ease;
  }
  a.button:hover { filter: brightness(1.06); transform: translateY(-1px); }
  code {
    font-family: var(--mono); font-size: 0.85em; color: var(--text);
    background: var(--bg); padding: 0.12rem 0.4rem; border-radius: 5px;
    border: 1px solid var(--border);
  }
</style>
</head>
<body>
<main>
  <div class="brand">rune<span class="dot">.</span></div>
  <div class="rule"></div>
  <div id="spinner" class="spin" aria-hidden="true"></div>
  <p id="status">Signing you in…</p>
  <a id="continue" class="button" href="%[1]s/">Continue to dashboard →</a>
</main>
<script>
(function () {
  var status = document.getElementById('status');
  var cont = document.getElementById('continue');
  var spinner = document.getElementById('spinner');
  function fail(html) { status.className = 'err'; status.innerHTML = html; spinner.style.display = 'none'; }
  function idle(html) { status.innerHTML = html; spinner.style.display = 'none'; }
  var code = new URLSearchParams(location.search).get('handoff');
  if (!code) {
    idle('Run <code>rune ui</code> from your terminal to sign in.');
    return;
  }
  fetch('/v1/ui/handoff/' + encodeURIComponent(code), { method: 'GET', credentials: 'same-origin' })
    .then(function (resp) {
      spinner.style.display = 'none';
      if (resp.status === 204) {
        status.textContent = 'Signed in — your dashboard session is active.';
        cont.style.display = 'inline-block';
      } else {
        fail('This sign-in link is invalid or expired. Run <code>rune ui</code> again.');
      }
    })
    .catch(function (e) { fail('Sign-in failed: ' + e); });
})();
</script>
</body>
</html>
`
