// Cloudflare Worker that serves the Rune install one-liners:
//
//   curl -fsSL https://get.runestack.io       | sh        -> scripts/install-cli.sh (CLI)
//   curl -fsSL https://install.runestack.io   | sudo sh   -> scripts/install.sh     (server)
//
// It proxies the raw script from this repo's master branch (so the installer
// LOGIC is always current; the script itself resolves the latest *release* at
// run time) and serves it as text/plain with a short cache.
//
// ── Deploy ────────────────────────────────────────────────────────────────
// 1. Create the Worker (dashboard or `wrangler deploy`).
// 2. Add Worker Routes:        get.runestack.io/*   and   install.runestack.io/*
// 3. Add proxied (orange-cloud) DNS records for `get` and `install`
//    (CNAME to the zone apex / any proxied target — the route does the work).
// TLS is automatic via the Cloudflare edge cert for *.runestack.io.

const FILES = {
  "get.runestack.io": "install-cli.sh",
  "install.runestack.io": "install.sh",
};

// Branch the installer SCRIPTS are served from. master = stable releases,
// dev = prereleases. While everything is a prerelease (no stable release cut
// yet) we serve from `dev` so the one-liners run the current installer logic.
// TODO(stable): switch to `master` once dev→master promotion begins.
// (The script itself still resolves the newest *release* via the GitHub API.)
const RAW_BASE = "https://raw.githubusercontent.com/runestack/rune/dev/scripts/";

export default {
  async fetch(request) {
    const host = new URL(request.url).hostname;
    const file = FILES[host];
    if (!file) {
      return new Response("not found\n", { status: 404, headers: { "content-type": "text/plain" } });
    }
    const upstream = await fetch(RAW_BASE + file, {
      cf: { cacheTtl: 300, cacheEverything: true },
    });
    if (!upstream.ok) {
      return new Response("installer temporarily unavailable\n", {
        status: 502,
        headers: { "content-type": "text/plain" },
      });
    }
    return new Response(upstream.body, {
      headers: {
        "content-type": "text/plain; charset=utf-8",
        "cache-control": "public, max-age=300",
      },
    });
  },
};
