import { useEffect, useState } from "react";
import {
  AppShell, Sidebar, Topbar, SearchPalette, PageHead, TweaksPanel, TweakSection, TweakRadio, TweakColor, ConfirmProvider, ToastProvider,
} from "./components";
import type { NavGroup } from "./components";
import { useTweaks, useThemeMode } from "./lib/theme";
import { DEVTOOLS } from "./lib/devtools";
import { ScopeProvider } from "./lib/scope";
import { ContextSwitcher } from "./ContextSwitcher";
import { ScopeIndicator } from "./ScopeIndicator";
import type { Instance, Service } from "./api/types";
import { clients } from "./api/transport";
import { mapInstance, mapService } from "./api/map";
import { InstanceStatus } from "./gen/pkg/api/proto/instance_pb";
import { Overview } from "./screens/Overview";
import { Services } from "./screens/Services";
import { Instances } from "./screens/Instances";
import { ServiceDrawer } from "./screens/ServiceDrawer";
import { InstanceDrawer } from "./screens/InstanceDrawer";
import { Namespaces } from "./screens/Namespaces";
import { Storage } from "./screens/Storage";
import { Identity } from "./screens/Identity";
import { Logs } from "./screens/Logs";
import { Secrets } from "./screens/Secrets";
import { RuneSight } from "./screens/runesight/RuneSight";
import type { RsTab } from "./screens/runesight/RuneSight";
import { emptyQuery, normQuery } from "./api/observe";
import type { LogQuery, Range } from "./api/observe";

// RuneSight routes map to a tab inside the single RuneSight surface.
const RS_ROUTES: Record<string, RsTab> = {
  "rs-explore": "explore",
  "rs-views": "views",
  "rs-alerts": "alerts",
  "rs-sources": "sources",
};

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
    ],
  },
  {
    group: "Operate",
    items: [
      { id: "logs", label: "Logs & Exec", icon: "logs" },
      { id: "identity", label: "Identity & RBAC", icon: "identity" },
    ],
  },
  {
    group: "Sight",
    items: [
      { id: "rs-explore", label: "Explore", icon: "search" },
      // All Sight surfaces are live (cluster-shared) — no static badge counts.
      { id: "rs-views", label: "Saved Views", icon: "logs" },
      { id: "rs-alerts", label: "Alerts", icon: "alert" },
      { id: "rs-sources", label: "Sources", icon: "instances" },
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
  logs: ["Operate", "Logs & Exec"],
  identity: ["Operate", "Identity & RBAC"],
  "rs-explore": ["Sight", "Explore"],
  "rs-views": ["Sight", "Saved Views"],
  "rs-alerts": ["Sight", "Alerts"],
  "rs-sources": ["Sight", "Sources"],
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
  const [themeMode, setThemeMode] = useThemeMode();
  const [route, setRoute] = useState("overview");
  const [svc, setSvc] = useState<Service | null>(null);
  const [inst, setInst] = useState<Instance | null>(null);
  const [logsSvc, setLogsSvc] = useState<string | null>(null);
  // When the Logs page is opened for a specific instance (e.g. from the
  // instance drawer's Logs button), pre-select it so a FAILED instance's
  // output is one click away instead of buried behind a manual scope pick.
  const [logsInst, setLogsInst] = useState<string | null>(null);
  // RuneSight state is lifted here so tab navigation (Saved Views -> Explore)
  // preserves the active query / range / live-tail across the route remount.
  const [rsQuery, setRsQuery] = useState<LogQuery>(emptyQuery);
  const [rsRange, setRsRange] = useState<Range>("1h");
  const [rsLive, setRsLive] = useState(false);
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
    // Service-level (or any non-instance) navigation clears a prior
    // instance target so the Logs page defaults to the service scope.
    if (r === "logs") setLogsInst(null);
    setRoute(r);
    setSvc(null);
    setInst(null);
    const c = document.querySelector(".content");
    if (c) c.scrollTop = 0;
  }
  // Open the Logs page targeted at a specific instance.
  const openInstLogs = (i: Instance) => {
    setLogsSvc(i.svc);
    setLogsInst(i.id);
    setRoute("logs");
    setSvc(null);
    setInst(null);
    const c = document.querySelector(".content");
    if (c) c.scrollTop = 0;
  };
  // Service and instance drawers are mutually exclusive.
  const openSvc = (s: Service) => { setInst(null); setSvc(s); };
  const openInst = (i: Instance) => { setSvc(null); setInst(i); };

  // Cross-surface back-links (e.g. from a RuneSight log line into the main
  // dashboard): resolve a friendly instance/service name to its live record and
  // open the matching drawer. If the lookup can't resolve it, fall back to
  // routing to the list view so the operator still lands somewhere useful.
  async function openInstanceByName(name: string, ns?: string) {
    try {
      const res = await clients.instances.listInstances({ namespace: "*" });
      const svcRes = await clients.services.listServices({ namespace: "*" });
      const svcNameById = new Map<string, string>();
      for (const s of svcRes.services) svcNameById.set(s.id, s.name);
      const match = res.instances
        .filter((i) => i.status !== InstanceStatus.DELETED)
        .map((i) => mapInstance(i, svcNameById))
        .find((i) => i.id === name && (!ns || i.ns === ns));
      if (match) { go("instances"); openInst(match); return; }
    } catch { /* fall through to the list view */ }
    go("instances");
  }
  async function openServiceByName(name: string, ns?: string) {
    try {
      const res = await clients.services.listServices({ namespace: "*" });
      const match = res.services
        .map(mapService)
        .find((s) => s.name === name && (!ns || s.ns === ns));
      if (match) { go("services"); openSvc(match); return; }
    } catch { /* fall through to the list view */ }
    go("services");
  }

  let screen;
  switch (route) {
    case "overview": screen = <Overview go={go} openSvc={openSvc} />; break;
    case "services": screen = <Services openSvc={openSvc} />; break;
    case "instances": screen = <Instances openInst={openInst} />; break;
    case "namespaces": screen = <Namespaces go={go} />; break;
    case "storage": screen = <Storage />; break;
    case "secrets": screen = <Secrets />; break;
    case "logs": screen = <Logs initialSvc={logsSvc} initialInst={logsInst} />; break;
    case "identity": screen = <Identity />; break;
    default:
      if (route in RS_ROUTES) {
        screen = (
          <RuneSight
            tab={RS_ROUTES[route]}
            go={(t) => go("rs-" + t)}
            query={rsQuery}
            setQuery={setRsQuery}
            range={rsRange}
            setRange={setRsRange}
            live={rsLive}
            setLive={setRsLive}
            loadView={(q) => { setRsQuery(normQuery(q)); go("rs-explore"); }}
            openInstance={openInstanceByName}
            openService={openServiceByName}
          />
        );
      } else {
        screen = <Placeholder title={NAV.flatMap((g) => g.items).find((i) => i.id === route)?.label ?? route} />;
      }
  }

  return (
    <ScopeProvider>
      <ConfirmProvider>
      <ToastProvider>
      <AppShell
        collapsed={navCollapsed}
        contentFlex={route === "logs" || route === "rs-explore"}
        sidebar={
          <Sidebar
            nav={NAV}
            route={route}
            go={go}
            logoVariant={t.logo}
            context={<ContextSwitcher go={go} />}
            user={user}
            onLogout={onLogout}
            theme={themeMode}
            onTheme={setThemeMode}
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
      {inst && <InstanceDrawer inst={inst} onClose={() => setInst(null)} openSvc={openSvc} openInstLogs={openInstLogs} />}

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
