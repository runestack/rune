/* Simulated container shell for the exec terminal (faithful to `rune exec`,
   which is effectively root inside the container). Pure logic — no React. */
import type { Instance } from "../mock/data";

export interface ExecLine {
  type: "out" | "err" | "sys" | "cmd";
  text: string;
  prompt?: string;
}

export interface ExecCtx {
  svc: string;
  host: string;
  ns: string | undefined;
  inst: Instance | undefined;
  cwd: string;
  setCwd: (p: string) => void;
}

export function execBanner(svc: string, host: string, inst: Instance | undefined, instId: string | null): ExecLine[] {
  const lines: ExecLine[] = [];
  if (instId) {
    lines.push({ type: "sys", text: `Attaching to instance ${host} on ${inst ? inst.node : "node"} …` });
    if (inst && inst.status === "fail")
      lines.push({ type: "err", text: `warning: instance is Unhealthy — exec may hang. Check: rune health instance ${host} --checks` });
  } else {
    lines.push({ type: "sys", text: `Resolving healthy instance for service/${svc} … selected ${host}` });
  }
  lines.push({ type: "sys", text: `Established TTY session on ${inst ? inst.node : "node"} · uid=0(root) gid=0(root)` });
  lines.push({ type: "out", text: `You are root inside the container. Type 'help' for commands, 'exit' to disconnect.` });
  return lines;
}

function execJoin(base: string, name: string) {
  return (base === "/" ? "" : base) + "/" + name;
}
function execResolve(cwd: string, p?: string) {
  if (!p) return cwd;
  const path = p.startsWith("/") ? p : cwd === "/" ? "/" + p : cwd + "/" + p;
  const stack: string[] = [];
  path.split("/").filter(Boolean).forEach((part) => {
    if (part === ".") return;
    if (part === "..") stack.pop();
    else stack.push(part);
  });
  return "/" + stack.join("/");
}

const EXEC_TREE: Record<string, string[]> = {
  "/": ["app", "bin", "etc", "proc", "tmp", "usr", "var"],
  "/app": ["server", "package.json", "node_modules", "public", "config"],
  "/etc": ["config", "secrets", "hostname", "hosts", "resolv.conf"],
  "/etc/config": ["app.yaml", "log-level", "feature.timeouts"],
  "/etc/secrets": ["db", "jwt"],
  "/etc/secrets/db": ["username", "password", "url"],
  "/etc/secrets/jwt": ["private.pem", "public.pem"],
};
const EXEC_FILES: Record<string, (c: ExecCtx) => string> = {
  "/etc/config/log-level": () => "info",
  "/etc/config/app.yaml": () => ["server:", "  port: 8080", "  workers: 4", "logging:", "  level: info", "  format: json", "database:", "  host: postgres-primary", "  pool: 25"].join("\n"),
  "/etc/config/feature.timeouts": () => "checkout_ms: 4000\nsearch_ms: 1200\nupstream_ms: 8000",
  "/etc/secrets/db/username": () => "acme_app",
  "/etc/secrets/db/password": () => "•••••••••••• (redacted — injected by runed at runtime)",
  "/etc/secrets/db/url": () => "postgres://acme_app:***@postgres-primary:5432/acme",
  "/etc/secrets/jwt/private.pem": () => "-----BEGIN PRIVATE KEY-----\n•••••••• (redacted) ••••••••\n-----END PRIVATE KEY-----",
  "/etc/secrets/jwt/public.pem": () => "-----BEGIN PUBLIC KEY-----\nMFkwEwYHKoZIzj0CAQYIKoZIzj0DAQcDQgAE…\n-----END PUBLIC KEY-----",
  "/etc/hostname": (c) => c.host,
  "/app/package.json": (c) => `{\n  "name": "acme-${c.svc}",\n  "version": "3.11.2",\n  "main": "server.js"\n}`,
};
function execEnv(c: ExecCtx) {
  return [
    "PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
    "HOSTNAME=" + c.host, "NODE_ENV=production", "LOG_LEVEL=info", "PORT=8080",
    "RUNE_SERVICE=" + c.svc, "RUNE_NAMESPACE=" + c.ns, "RUNE_INSTANCE=" + c.host,
    "DATABASE_URL=postgres://acme_app:***@postgres-primary:5432/acme",
    "REDIS_URL=redis://redis-cache:6379", "GHCR_TOKEN=***",
  ];
}

export function execRun(raw: string, ctx: ExecCtx): ExecLine[] {
  const { svc, host, ns, inst, cwd, setCwd } = ctx;
  let cmd = raw;
  let pipeGrep: string | null = null;
  const pIdx = raw.indexOf("|");
  if (pIdx !== -1) {
    cmd = raw.slice(0, pIdx).trim();
    const gm = raw.slice(pIdx + 1).trim().match(/^grep\s+(.+)/);
    if (gm) pipeGrep = gm[1].replace(/['"]/g, "");
  }
  const parts = cmd.split(/\s+/).filter(Boolean);
  const bin = parts[0];
  const args = parts.slice(1);
  const out = (lines: string | string[]): ExecLine[] => {
    let arr = Array.isArray(lines) ? lines : [lines];
    if (pipeGrep) arr = arr.filter((l) => l.toLowerCase().includes(pipeGrep!.toLowerCase()));
    return arr.map((t) => ({ type: "out", text: t }));
  };
  const err = (t: string): ExecLine[] => [{ type: "err", text: t }];

  switch (bin) {
    case "help":
      return out([
        "Simulated container shell — available commands:",
        "  ls [path]              list a directory",
        "  cd <path>              change directory",
        "  cat <file>             print a file",
        "  pwd                    print working directory",
        "  env / printenv         environment variables",
        "  ps aux                 running processes",
        "  whoami / id            current user",
        "  hostname / uname -a    instance / kernel info",
        "  nc -zv <host> <port>   test connectivity",
        "  curl <url>             make an http request",
        "  echo <text>            print text (expands $VARS)",
        "  clear                  clear the screen",
        "  exit                   close the session",
      ]);
    case "whoami": return out("root");
    case "id": return out("uid=0(root) gid=0(root) groups=0(root)");
    case "pwd": return out(cwd);
    case "hostname": return out(host);
    case "uname": return out("Linux " + host + " 6.1.0-rune #1 SMP x86_64 GNU/Linux");
    case "date": return out(new Date().toString());
    case "echo":
      return out(
        args.join(" ")
          .replace(/\$RUNE_SERVICE/g, svc)
          .replace(/\$RUNE_NAMESPACE/g, ns ?? "")
          .replace(/\$HOSTNAME/g, host)
          .replace(/\$LOG_LEVEL/g, "info")
          .replace(/\$PORT/g, "8080"),
      );
    case "ls": {
      const target = args.find((a) => !a.startsWith("-"));
      const p = execResolve(cwd, target || cwd);
      if (EXEC_TREE[p]) {
        const entries = EXEC_TREE[p];
        if (args.some((a) => a.startsWith("-") && a.includes("l")))
          return out(entries.map((e) => {
            const isDir = !!EXEC_TREE[execJoin(p, e)];
            return `${isDir ? "drwxr-xr-x" : "-rw-r--r--"} 1 root root ${String(isDir ? 4096 : 120 + e.length * 7).padStart(5)} May 30 14:0${e.length % 6} ${e}`;
          }));
        return out(entries.join("  "));
      }
      if (EXEC_FILES[p]) return out(p.split("/").pop()!);
      return err(`ls: cannot access '${target || p}': No such file or directory`);
    }
    case "cd": {
      if (!args[0] || args[0] === "~") { setCwd("/app"); return []; }
      const p = execResolve(cwd, args[0]);
      if (EXEC_TREE[p]) { setCwd(p); return []; }
      if (EXEC_FILES[p]) return err(`cd: not a directory: ${args[0]}`);
      return err(`cd: ${args[0]}: No such file or directory`);
    }
    case "cat": {
      if (!args[0]) return err("cat: missing operand");
      const p = execResolve(cwd, args[0]);
      if (EXEC_FILES[p]) return out(EXEC_FILES[p](ctx));
      if (EXEC_TREE[p]) return err(`cat: ${args[0]}: Is a directory`);
      return err(`cat: ${args[0]}: No such file or directory`);
    }
    case "env":
    case "printenv":
      return out(execEnv(ctx));
    case "ps":
      return out([
        "  PID USER     %CPU %MEM    VSZ   RSS STAT  TIME COMMAND",
        `    1 root     ${String(inst ? inst.cpu : 5).padStart(4)}  ${inst ? inst.mem : 8}.1 712304 48120 Ssl   4:21 /app/server --config /etc/config/app.yaml`,
        "   28 root      0.0  0.4  22150  3120 S     0:00 /sbin/runed-shim --instance " + host,
        "   53 root      0.2  0.6  18044  4980 R     0:00 ps aux",
      ]);
    case "nc": {
      const h = args.find((a) => !a.startsWith("-")) || "";
      const port = args[args.length - 1];
      const reachable = ["postgres", "redis", "auth", "api", "loki", "grafana"];
      if (reachable.some((k) => h.includes(k))) return out(`Connection to ${h} ${port} port [tcp/*] succeeded!`);
      return err(`nc: connect to ${h} port ${port} (tcp) failed: Connection refused`);
    }
    case "curl": {
      const url = args.find((a) => a.startsWith("http")) || args[args.length - 1] || "";
      if (url.includes("postgres")) return out(["* Connected to postgres-primary (10.244.6.10) port 5432", "* Server speaks the postgres wire protocol, not HTTP"]);
      if (url.includes("health") || url.includes("healthz")) return out('{"status":"ok","uptime":"6d","checks":{"db":"ok","redis":"ok"}}');
      return out(["* Trying " + url + " …", "< HTTP/1.1 200 OK", "< content-type: application/json", `{"service":"${svc}","ns":"${ns}","ok":true}`]);
    }
    case "top":
    case "htop":
      return err(`${bin}: not available in this container`);
    case "sudo":
      return out(args.length ? execRun(args.join(" "), ctx).map((r) => r.text).join("\n") : "usage: sudo <command>  (you are already root)");
    default:
      return err(`sh: ${bin}: command not found`);
  }
}
