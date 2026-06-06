import { useEffect, useState } from "react";
import {
  AppShell, Sidebar, Topbar, SearchPalette, PageHead, TweaksPanel, TweakSection, TweakRadio, TweakColor, ConfirmProvider, ToastProvider,
} from "./components";
import type { NavGroup } from "./components";
import { useTweaks } from "./lib/theme";
import { DEVTOOLS } from "./lib/devtools";
import { ScopeProvider } from "./lib/scope";
import { ContextSwitcher } from "./ContextSwitcher";
import { ScopeIndicator } from "./ScopeIndicator";
import type { Instance, Service } from "./api/types";
import { Overview } from "./screens/Overview";
import { Services } from "./screens/Services";
import { Instances } from "./screens/Instances";
import { ServiceDrawer } from "./screens/ServiceDrawer";
import { InstanceDrawer } from "./screens/InstanceDrawer";
import { Namespaces } from "./screens/Namespaces";
import { Storage } from "./screens/Storage";
import { Networking } from "./screens/Networking";
import { Identity } from "./screens/Identity";
import { Logs } from "./screens/Logs";
import { Secrets } from "./screens/Secrets";
import { RuneSight } from "./screens/runesight/RuneSight";

const NAV: NavGroup[] = [
  {
    group: "Cluster",
    items: [
      { id: "overview", label: "Overview", icon: "overview" },
      { id: "services", label: "Services", icon: "services" },
      { id: "instances", label: "Instances", icon: "instances" },
      { id: "namespaces", label: "Namespaces", icon: "namespaces" },
    ],
  },
  {
    group: "Config & Data",
    items: [
      { id: "storage", label: "Storage", icon: "storage" },
      { id: "secrets", label: "Secrets & Config", icon: "secrets" },
      { id: "network", label: "Networking", icon: "network" },
    ],
  },
  {
    group: "Observe",
    items: [
      { id: "runesight", label: "RuneSight", icon: "search" },
    ],
  },
  {
    group: "Operate",
    items: [
      { id: "logs", label: "Logs & Exec", icon: "logs" },
      { id: "identity", label: "Identity & RBAC", icon: "identity" },
    ],
  },
];

const CRUMBS: Record<string, string[]> = {
  overview: ["Cluster", "Overview"],
  services: ["Cluster", "Services"],
  instances: ["Cluster", "Instances"],
  namespaces: ["Cluster", "Namespaces"],
  storage: ["Data", "Storage"],
  secrets: ["Data", "Secrets & Config"],
  network: ["Data", "Networking"],
  runesight: ["Observe", "RuneSight"],
  logs: ["Operate", "Logs & Exec"],
  identity: ["Operate", "Identity & RBAC"],
};

function Placeholder({ title }: { title: string }) {
  return (
    <div className="wrap">
      <PageHead eyebrow="Coming next" title={title} sub="This screen lands in the next slice. The shell, navigation, drawer and design system are live." />
      <div className="empty">Screen under construction.</div>
    </div>
  );
}

export interface AppProps {
  user: { name: string; role: string };
  onLogout?: () => void;
}

export function App({ user, onLogout }: AppProps) {
  const [t, setTweak] = useTweaks();
  const [route, setRoute] = useState("overview");
  const [svc, setSvc] = useState<Service | null>(null);
  const [inst, setInst] = useState<Instance | null>(null);
  const [logsSvc, setLogsSvc] = useState<string | null>(null);
  const [navCollapsed, setNavCollapsed] = useState(() => {
    try { return localStorage.getItem("rune-nav-collapsed") === "1"; } catch { return false; }
  });
  const [searchOpen, setSearchOpen] = useState(false);

  useEffect(() => {
    try { localStorage.setItem("rune-nav-collapsed", navCollapsed ? "1" : "0"); } catch { /* ignore */ }
  }, [navCollapsed]);
  const toggleNav = () => setNavCollapsed((v) => !v);

  // ⌘K / Ctrl-K opens the command palette; "/" opens it when not typing.
  useEffect(() => {
    const h = (e: KeyboardEvent) => {
      if ((e.metaKey || e.ctrlKey) && e.key.toLowerCase() === "k") { e.preventDefault(); setSearchOpen(true); }
      else if (e.key === "/" && !searchOpen) {
        const el = e.target as HTMLElement | null;
        const tag = el?.tagName;
        if (tag !== "INPUT" && tag !== "TEXTAREA" && !el?.isContentEditable) { e.preventDefault(); setSearchOpen(true); }
      }
    };
    window.addEventListener("keydown", h);
    return () => window.removeEventListener("keydown", h);
  }, [searchOpen]);

  function go(r: string, arg?: Service) {
    if (r === "logs" && arg) setLogsSvc(arg.name);
    setRoute(r);
    setSvc(null);
    setInst(null);
    const c = document.querySelector(".content");
    if (c) c.scrollTop = 0;
  }
  // Service and instance drawers are mutually exclusive.
  const openSvc = (s: Service) => { setInst(null); setSvc(s); };
  const openInst = (i: Instance) => { setSvc(null); setInst(i); };

  let screen;
  switch (route) {
    case "overview": screen = <Overview go={go} openSvc={openSvc} />; break;
    case "services": screen = <Services openSvc={openSvc} />; break;
    case "instances": screen = <Instances openInst={openInst} />; break;
    case "namespaces": screen = <Namespaces go={go} />; break;
    case "storage": screen = <Storage />; break;
    case "secrets": screen = <Secrets />; break;
    case "network": screen = <Networking />; break;
    case "runesight": screen = <RuneSight />; break;
    case "logs": screen = <Logs initialSvc={logsSvc} />; break;
    case "identity": screen = <Identity />; break;
    default: screen = <Placeholder title={NAV.flatMap((g) => g.items).find((i) => i.id === route)?.label ?? route} />;
  }

  return (
    <ScopeProvider>
      <ConfirmProvider>
      <ToastProvider>
      <AppShell
        collapsed={navCollapsed}
        contentFlex={route === "logs" || route === "runesight"}
        sidebar={
          <Sidebar
            nav={NAV}
            route={route}
            go={go}
            logoVariant={t.logo}
            context={<ContextSwitcher go={go} />}
            user={user}
            onLogout={onLogout}
            onToggleNav={toggleNav}
          />
        }
        topbar={
          <Topbar
            crumbs={CRUMBS[route] ?? ["Cluster"]}
            scope={<ScopeIndicator />}
            collapsed={navCollapsed}
            onToggleNav={toggleNav}
            onSearch={() => setSearchOpen(true)}
          />
        }
      >
        <div key={route} className="fadein">{screen}</div>
      </AppShell>

      {searchOpen && (
        <SearchPalette nav={NAV} onClose={() => setSearchOpen(false)} go={go} openSvc={openSvc} openInst={openInst} />
      )}

      {svc && <ServiceDrawer svc={svc} onClose={() => setSvc(null)} go={go} openInst={openInst} />}
      {inst && <InstanceDrawer inst={inst} onClose={() => setInst(null)} go={go} openSvc={openSvc} />}

      {DEVTOOLS && (
        <TweaksPanel>
          <TweakSection label="Brand" />
          <TweakRadio label="Logo" value={t.logo} options={["mark", "tile", "wordmark", "mono"]} onChange={(v) => setTweak("logo", v)} />
          <TweakColor label="Accent" value={t.accent} options={["#9e8cfc", "#30a46c", "#67ddfd", "#f76809", "#d6409f", "#daa16e"]} onChange={(v) => setTweak("accent", v)} />
          <TweakSection label="Surface" />
          <TweakRadio label="Edges" value={t.edges} options={["soft", "crisp", "sharp"]} onChange={(v) => setTweak("edges", v)} />
        </TweaksPanel>
      )}
      </ToastProvider>
      </ConfirmProvider>
    </ScopeProvider>
  );
}
