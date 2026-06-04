/* ============================================================
   Data hooks — one per resource. Each returns { data, loading, error, reload }.

   In demo mode (no backend session) the hook returns the bundled RUNE.* mock
   synchronously. Otherwise it calls the live Connect client, maps the proto
   response to the screen shape via ../api/map, and exposes loading/error.
   ============================================================ */
import { useCallback, useEffect, useState } from "react";
import { clients } from "./transport";
import { useDemo } from "./demo";
import { RUNE } from "../mock/data";
import type {
  ConfigMap, Instance, Namespace, Node, Policy, Principal,
  Role, Secret, Service, StorageClass, Volume,
} from "../mock/data";
import {
  ageFrom, healthStatusKey, mapConfigmap, mapInstance, mapNamespace, mapNodeHealth,
  mapPolicy, mapPolicyToRole, mapPrincipal, mapSecret, mapService, mapStorageClass,
  mapTokenPrincipal, mapVolume,
} from "./map";
import { InstanceStatus } from "../gen/pkg/api/proto/instance_pb";

export interface Query<T> {
  data: T;
  loading: boolean;
  error: string | null;
  reload: () => void;
}

const ALL_NS = "*";

/**
 * useQuery is the shared engine. `mock` is returned synchronously in demo mode;
 * otherwise `live` is awaited, its result becomes `data`, and loading/error are
 * tracked. The effect re-runs when `deps` change or reload() is called.
 */
function useQuery<T>(
  mock: T,
  live: (signal: AbortSignal) => Promise<T>,
  deps: unknown[] = [],
): Query<T> {
  const demo = useDemo();
  const [data, setData] = useState<T>(mock);
  const [loading, setLoading] = useState(!demo);
  const [error, setError] = useState<string | null>(null);
  const [nonce, setNonce] = useState(0);

  const reload = useCallback(() => setNonce((n) => n + 1), []);

  useEffect(() => {
    if (demo) {
      setData(mock);
      setLoading(false);
      setError(null);
      return;
    }
    const ctrl = new AbortController();
    let alive = true;
    setLoading(true);
    setError(null);
    live(ctrl.signal)
      .then((res) => {
        if (!alive) return;
        setData(res);
        setLoading(false);
      })
      .catch((e: unknown) => {
        if (!alive || ctrl.signal.aborted) return;
        setError(e instanceof Error ? e.message : String(e));
        setLoading(false);
      });
    return () => {
      alive = false;
      ctrl.abort();
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [demo, nonce, ...deps]);

  return { data, loading, error, reload };
}

/* ---------------- namespaces ---------------- */

export function useNamespaces(): Query<Namespace[]> {
  return useQuery<Namespace[]>(
    RUNE.namespaces,
    async (signal) => {
      const [nsRes, svcRes, instRes] = await Promise.all([
        clients.namespaces.listNamespaces({}, { signal }),
        clients.services.listServices({ namespace: ALL_NS }, { signal }),
        clients.instances.listInstances({ namespace: ALL_NS }, { signal }),
      ]);
      const svcByNs = new Map<string, number>();
      for (const s of svcRes.services) svcByNs.set(s.namespace, (svcByNs.get(s.namespace) ?? 0) + 1);
      const instByNs = new Map<string, number>();
      for (const i of instRes.instances) {
        if (i.status === InstanceStatus.DELETED) continue;
        instByNs.set(i.namespace, (instByNs.get(i.namespace) ?? 0) + 1);
      }
      return nsRes.namespaces.map((n) =>
        mapNamespace(n, { services: svcByNs.get(n.name) ?? 0, instances: instByNs.get(n.name) ?? 0 }),
      );
    },
  );
}

/* ---------------- services ---------------- */

export function useServices(ns?: string): Query<Service[]> {
  return useQuery<Service[]>(
    RUNE.services,
    async (signal) => {
      const res = await clients.services.listServices({ namespace: ns || ALL_NS }, { signal });
      return res.services.map(mapService);
    },
    [ns ?? ""],
  );
}

/* ---------------- instances ---------------- */

export function useInstances(node?: string): Query<Instance[]> {
  return useQuery<Instance[]>(
    RUNE.instances,
    async (signal) => {
      const [instRes, svcRes] = await Promise.all([
        clients.instances.listInstances({ namespace: ALL_NS, ...(node ? { nodeId: node } : {}) }, { signal }),
        clients.services.listServices({ namespace: ALL_NS }, { signal }),
      ]);
      const svcNameById = new Map<string, string>();
      for (const s of svcRes.services) svcNameById.set(s.id, s.name);
      return instRes.instances
        .filter((i) => i.status !== InstanceStatus.DELETED)
        .map((i) => mapInstance(i, svcNameById));
    },
    [node ?? ""],
  );
}

/* ---------------- storage ---------------- */

export function useVolumes(): Query<Volume[]> {
  return useQuery<Volume[]>(
    RUNE.volumes,
    async (signal) => {
      const res = await clients.volumes.listVolumes({ namespace: ALL_NS }, { signal });
      return res.volumes.map(mapVolume);
    },
  );
}

export function useStorageClasses(): Query<StorageClass[]> {
  return useQuery<StorageClass[]>(
    RUNE.storageClasses,
    async (signal) => {
      const [scRes, volRes] = await Promise.all([
        clients.storage.listStorageClasses({}, { signal }),
        clients.volumes.listVolumes({ namespace: ALL_NS }, { signal }),
      ]);
      const counts = new Map<string, number>();
      for (const v of volRes.volumes) counts.set(v.storageClassName, (counts.get(v.storageClassName) ?? 0) + 1);
      return scRes.storageClasses.map((c) => mapStorageClass(c, counts.get(c.name) ?? 0));
    },
  );
}

/* ---------------- secrets / configmaps ---------------- */

export function useSecrets(): Query<Secret[]> {
  return useQuery<Secret[]>(
    RUNE.secrets,
    async (signal) => {
      const res = await clients.secrets.listSecrets({ namespace: ALL_NS }, { signal });
      return res.secrets.map(mapSecret);
    },
  );
}

export function useConfigmaps(): Query<ConfigMap[]> {
  return useQuery<ConfigMap[]>(
    RUNE.configmaps,
    async (signal) => {
      const res = await clients.configmaps.listConfigmaps({ namespace: ALL_NS }, { signal });
      return res.configmaps.map(mapConfigmap);
    },
  );
}

/* ---------------- networking (RBAC policies projected) ---------------- */

export function usePolicies(): Query<Policy[]> {
  return useQuery<Policy[]>(
    RUNE.policies,
    async (signal) => {
      const res = await clients.admin.policyList({}, { signal });
      return res.policies.map(mapPolicy);
    },
  );
}

/* ---------------- identity / RBAC ---------------- */

export function useRoles(): Query<Role[]> {
  return useQuery<Role[]>(
    RUNE.roles,
    async (signal) => {
      const [polRes, userRes] = await Promise.all([
        clients.admin.policyList({}, { signal }),
        clients.admin.userList({}, { signal }),
      ]);
      const subjectsByPolicy = new Map<string, number>();
      for (const u of userRes.users) for (const p of u.policies ?? []) {
        subjectsByPolicy.set(p, (subjectsByPolicy.get(p) ?? 0) + 1);
      }
      return polRes.policies.map((p) =>
        mapPolicyToRole(p, subjectsByPolicy.get(p.name) ?? subjectsByPolicy.get(p.id) ?? 0),
      );
    },
  );
}

export function usePrincipals(): Query<Principal[]> {
  return useQuery<Principal[]>(
    RUNE.principals,
    async (signal) => {
      const [userRes, tokRes] = await Promise.all([
        clients.admin.userList({}, { signal }),
        clients.admin.tokenList({}, { signal }).catch(() => ({ tokens: [] })),
      ]);
      const users = userRes.users.map((u) => mapPrincipal(u));
      // Surface service-account tokens that don't correspond to a listed user.
      const known = new Set(users.map((u) => u.name));
      const machineTokens = (tokRes.tokens ?? [])
        .filter((t) => !t.revoked && (t.subjectType || "").toLowerCase() !== "user" && !known.has(t.name))
        .map(mapTokenPrincipal);
      // De-dup machine tokens by name.
      const seen = new Set<string>();
      const machines = machineTokens.filter((m) => (seen.has(m.name) ? false : (seen.add(m.name), true)));
      return [...users, ...machines];
    },
  );
}

/* ---------------- overview ---------------- */

export interface OverviewData {
  cluster: { name: string; context: string; version: string; uptime: string; nodes: number };
  totals: {
    services: number; healthy: number; instances: number; runningInstances: number;
    namespaces: number; cpu: number; mem: number; cpuCores: string; memGi: string;
  };
  nodes: Node[];
  services: Service[];
  events: typeof RUNE.events;
  cpuHistory: number[];
  memHistory: number[];
}

const GiB = 1024 ** 3;

export function useOverview(): Query<OverviewData> {
  const mock: OverviewData = {
    cluster: RUNE.cluster,
    totals: RUNE.totals,
    nodes: RUNE.nodes,
    services: RUNE.services,
    events: RUNE.events,
    cpuHistory: RUNE.cpuHistory,
    memHistory: RUNE.memHistory,
  };
  return useQuery<OverviewData>(mock, async (signal) => {
    const [healthRes, svcRes, instRes, nsRes, verRes] = await Promise.all([
      clients.health.getHealth({ componentType: "node", includeChecks: false }, { signal }),
      clients.services.listServices({ namespace: ALL_NS }, { signal }),
      clients.instances.listInstances({ namespace: ALL_NS }, { signal }),
      clients.namespaces.listNamespaces({}, { signal }),
      clients.health.getServerVersion({}, { signal }).catch(() => null),
    ]);

    const services = svcRes.services.map(mapService);
    const instances = instRes.instances.filter((i) => i.status !== InstanceStatus.DELETED);
    const runningInstances = instances.filter((i) => i.status === InstanceStatus.RUNNING).length;

    // instances per node, for the node cards
    const instByNode = new Map<string, number>();
    for (const i of instances) instByNode.set(i.nodeId, (instByNode.get(i.nodeId) ?? 0) + 1);

    const nodeComponents = healthRes.components.filter((c) => c.componentType === "node");
    const nodes: Node[] = nodeComponents.map((c) => mapNodeHealth(c, instByNode.get(c.id) ?? 0));

    // cluster CPU/mem = mean across nodes that report usage
    const cpuVals = nodes.map((n) => n.cpu).filter((v) => v > 0);
    const memVals = nodes.map((n) => n.mem).filter((v) => v > 0);
    const cpu = cpuVals.length ? Math.round(cpuVals.reduce((a, b) => a + b, 0) / cpuVals.length) : 0;
    const mem = memVals.length ? Math.round(memVals.reduce((a, b) => a + b, 0) / memVals.length) : 0;

    const res0 = nodeComponents[0]?.resources;
    const cpuCores = res0 ? `${res0.cpuCores} vCPU` : "—";
    const memGi = res0 && res0.memTotalBytes > 0n
      ? `${(Number(res0.memUsedBytes) / GiB).toFixed(1)} / ${(Number(res0.memTotalBytes) / GiB).toFixed(1)} GiB`
      : "—";

    const healthy = services.filter((s) => s.status === "run").length;
    const activeNs = nsRes.namespaces.filter((n) =>
      svcRes.services.some((s) => s.namespace === n.name),
    ).length;

    // node mini-history isn't retained server-side; flatten the spark to the
    // current reading so the curve renders without inventing trends.
    const flat = (v: number) => Array(24).fill(v);

    return {
      cluster: {
        name: "rune",
        context: nodeComponents[0]?.name ? `node ${nodeComponents[0].name}` : "dev",
        version: verRes?.version ? `rune ${verRes.version}` : "rune dev",
        uptime: nodeComponents[0]?.timestamp ? ageFrom(nodeComponents[0].timestamp) : "—",
        nodes: nodes.length,
      },
      totals: {
        services: services.length,
        healthy,
        instances: instances.length,
        runningInstances,
        namespaces: activeNs,
        cpu, mem,
        cpuCores, memGi,
      },
      nodes,
      services,
      events: [],
      cpuHistory: flat(cpu),
      memHistory: flat(mem),
    };
  });
}

/* ---------------- audit feed ---------------- */

export interface FeedItem { t: string; kind: string; svc: string; ns: string; msg: string; status: string }

/**
 * useAuditFeed maps recent audit events to the activity-feed shape. The mock
 * uses the rich, HTML-laden RUNE.events; live audit events render as plain
 * text (no HTML) so they are injection-safe via the Feed's innerHTML.
 */
export function useAuditFeed(limit = 8): Query<FeedItem[]> {
  return useQuery<FeedItem[]>(
    RUNE.events as unknown as FeedItem[],
    async (signal) => {
      const res = await clients.audit.listAuditEvents({ limit }, { signal });
      return res.events.map((e) => {
        const outcome = (e.outcome || "").toLowerCase();
        const status = outcome === "success" ? "run" : outcome === "denied" ? "warn" : outcome === "error" ? "fail" : "net";
        const ref = e.resourceRef ? ` ${escapeHtml(e.resourceRef)}` : "";
        const msg = `${escapeHtml(e.actor || "system")} ${escapeHtml(e.action || "acted")}${ref}`;
        return {
          t: e.timestamp ? ageFrom(e.timestamp) + " ago" : "—",
          kind: e.action || "event",
          svc: e.resourceRef || "—",
          ns: e.namespace || "—",
          msg,
          status,
        };
      });
    },
    [limit],
  );
}

function escapeHtml(s: string): string {
  return s.replace(/[&<>"']/g, (c) =>
    ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c] || c),
  );
}

/* re-export so screens can pull status helper from one place if needed */
export { healthStatusKey };
