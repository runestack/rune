import { useState } from "react";
import {
  AppShell, Sidebar, Topbar, PageHead, TweaksPanel, TweakSection, TweakRadio, TweakColor,
} from "./components";
import type { NavGroup } from "./components";
import { useTweaks } from "./lib/theme";
import { RUNE } from "./mock/data";
import type { Service } from "./mock/data";
import { Overview } from "./screens/Overview";
import { Services } from "./screens/Services";
import { Instances } from "./screens/Instances";
import { ServiceDrawer } from "./screens/ServiceDrawer";
import { Namespaces } from "./screens/Namespaces";
import { Storage } from "./screens/Storage";
import { Networking } from "./screens/Networking";
import { Identity } from "./screens/Identity";
import { Logs } from "./screens/Logs";

const NAV: NavGroup[] = [
  {
    group: "Cluster",
    items: [
      { id: "overview", label: "Overview", icon: "overview" },
      { id: "services", label: "Services", icon: "services", count: RUNE.totals.services },
      { id: "instances", label: "Instances", icon: "instances", count: RUNE.totals.instances },
      { id: "namespaces", label: "Namespaces", icon: "namespaces", count: RUNE.namespaces.length },
    ],
  },
  {
    group: "Config & Data",
    items: [
      { id: "storage", label: "Storage", icon: "storage", count: RUNE.volumes.length },
      { id: "secrets", label: "Secrets & Config", icon: "secrets", count: RUNE.secrets.length + RUNE.configmaps.length },
      { id: "network", label: "Networking", icon: "network", count: RUNE.policies.length },
    ],
  },
  {
    group: "Operate",
    items: [
      { id: "logs", label: "Logs & Exec", icon: "logs" },
      { id: "identity", label: "Identity & RBAC", icon: "identity", count: RUNE.principals.length },
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

export function App() {
  const [t, setTweak] = useTweaks();
  const [route, setRoute] = useState("overview");
  const [svc, setSvc] = useState<Service | null>(null);
  const [logsSvc, setLogsSvc] = useState<string | null>(null);

  function go(r: string, arg?: Service) {
    if (r === "logs" && arg) setLogsSvc(arg.name);
    setRoute(r);
    setSvc(null);
    const c = document.querySelector(".content");
    if (c) c.scrollTop = 0;
  }
  const openSvc = (s: Service) => setSvc(s);

  let screen;
  switch (route) {
    case "overview": screen = <Overview go={go} openSvc={openSvc} />; break;
    case "services": screen = <Services openSvc={openSvc} />; break;
    case "instances": screen = <Instances openSvc={openSvc} />; break;
    case "namespaces": screen = <Namespaces go={go} />; break;
    case "storage": screen = <Storage />; break;
    case "network": screen = <Networking />; break;
    case "logs": screen = <Logs initialSvc={logsSvc} />; break;
    case "identity": screen = <Identity />; break;
    default: screen = <Placeholder title={NAV.flatMap((g) => g.items).find((i) => i.id === route)?.label ?? route} />;
  }

  return (
    <>
      <AppShell
        sidebar={
          <Sidebar
            nav={NAV}
            route={route}
            go={go}
            logoVariant={t.logo}
            cluster={RUNE.cluster}
            user={{ name: "Ore", role: "cluster-admin" }}
          />
        }
        topbar={<Topbar crumbs={CRUMBS[route] ?? ["Cluster"]} />}
      >
        <div key={route} className="fadein">{screen}</div>
      </AppShell>

      {svc && <ServiceDrawer svc={svc} onClose={() => setSvc(null)} go={go} />}

      <TweaksPanel>
        <TweakSection label="Brand" />
        <TweakRadio label="Logo" value={t.logo} options={["mark", "tile", "wordmark", "mono"]} onChange={(v) => setTweak("logo", v)} />
        <TweakColor label="Accent" value={t.accent} options={["#9e8cfc", "#30a46c", "#67ddfd", "#f76809", "#d6409f", "#daa16e"]} onChange={(v) => setTweak("accent", v)} />
        <TweakSection label="Surface" />
        <TweakRadio label="Edges" value={t.edges} options={["soft", "crisp", "sharp"]} onChange={(v) => setTweak("edges", v)} />
      </TweaksPanel>
    </>
  );
}
