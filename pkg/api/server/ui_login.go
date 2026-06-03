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
		w.Header().Set("Content-Security-Policy", "default-src 'none'; script-src 'unsafe-inline'; style-src 'unsafe-inline'; connect-src 'self'")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(page))
	}
}

// uiLoginPageTemplate has one %s: the dashboard mount path (e.g. "/ui").
const uiLoginPageTemplate = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Rune — Sign in</title>
<style>
  body { font: 15px/1.5 system-ui, sans-serif; margin: 0; display: grid; place-items: center; min-height: 100vh; background: #0f1115; color: #e6e8eb; }
  main { text-align: center; padding: 2rem; max-width: 28rem; }
  h1 { font-size: 1.25rem; margin: 0 0 1rem; }
  #status { color: #9aa4b2; }
  a.button { display: none; margin-top: 1.25rem; padding: .55rem 1rem; background: #3b82f6; color: #fff; border-radius: .5rem; text-decoration: none; }
  code { background: #1b1f27; padding: .15rem .4rem; border-radius: .3rem; }
</style>
</head>
<body>
<main>
  <h1>Rune Dashboard</h1>
  <p id="status">Signing in…</p>
  <a id="continue" class="button" href="%[1]s/">Continue to dashboard →</a>
</main>
<script>
(async function () {
  var status = document.getElementById('status');
  var cont = document.getElementById('continue');
  var code = new URLSearchParams(location.search).get('handoff');
  if (!code) {
    status.innerHTML = 'Run <code>rune ui login</code> from your terminal to sign in.';
    return;
  }
  try {
    var resp = await fetch('/v1/ui/handoff/' + encodeURIComponent(code), { method: 'GET', credentials: 'same-origin' });
    if (resp.status === 204) {
      status.textContent = 'Signed in — your dashboard session is active.';
      cont.style.display = 'inline-block';
    } else {
      status.innerHTML = 'This sign-in link is invalid or expired. Run <code>rune ui login</code> again.';
    }
  } catch (e) {
    status.textContent = 'Sign-in failed: ' + e;
  }
})();
</script>
</body>
</html>
`
