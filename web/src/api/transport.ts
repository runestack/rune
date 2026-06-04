import { createConnectTransport } from "@connectrpc/connect-web";
import { Code, ConnectError, createPromiseClient, type Interceptor, type Transport } from "@connectrpc/connect";
import { ensureFresh, getAccessToken, refresh } from "./session";

import { AuthService } from "../gen/pkg/api/proto/auth_connect";
import { AdminService } from "../gen/pkg/api/proto/admin_connect";
import { HealthService } from "../gen/pkg/api/proto/health_connect";
import { NamespaceService } from "../gen/pkg/api/proto/namespace_connect";
import { ServiceService } from "../gen/pkg/api/proto/service_connect";
import { InstanceService } from "../gen/pkg/api/proto/instance_connect";
import { LogService } from "../gen/pkg/api/proto/logs_connect";
import { SecretService } from "../gen/pkg/api/proto/secret_connect";
import { ConfigmapService } from "../gen/pkg/api/proto/configmap_connect";
import { VolumeService, StorageClassService } from "../gen/pkg/api/proto/storage_connect";
import { AuditService } from "../gen/pkg/api/proto/audit_connect";

// The SPA is same-origin with runed (served under /ui); the transcoder is at
// /grpc. In dev, Vite proxies /grpc to a local runed.
const BASE_URL = "/grpc";

/** Attaches the bearer access token + x-rune-client header, refreshing a
 *  cookie session before the call and once more on an Unauthenticated reply. */
const authInterceptor: Interceptor = (next) => async (req) => {
  await ensureFresh();
  const apply = () => {
    const tok = getAccessToken();
    if (tok) req.header.set("Authorization", `Bearer ${tok}`);
    req.header.set("x-rune-client", "ui");
  };
  apply();
  try {
    return await next(req);
  } catch (e) {
    const err = ConnectError.from(e);
    // Unary auto-refresh: one retry after a fresh access token. (Streams
    // surface the error to the caller, which re-subscribes.)
    if (err.code === Code.Unauthenticated && !req.stream) {
      if (await refresh()) {
        apply();
        return await next(req);
      }
    }
    throw e;
  }
};

export const transport: Transport = createConnectTransport({
  baseUrl: BASE_URL,
  interceptors: [authInterceptor],
});

export const clients = {
  auth: createPromiseClient(AuthService, transport),
  admin: createPromiseClient(AdminService, transport),
  health: createPromiseClient(HealthService, transport),
  namespaces: createPromiseClient(NamespaceService, transport),
  services: createPromiseClient(ServiceService, transport),
  instances: createPromiseClient(InstanceService, transport),
  logs: createPromiseClient(LogService, transport),
  secrets: createPromiseClient(SecretService, transport),
  configmaps: createPromiseClient(ConfigmapService, transport),
  volumes: createPromiseClient(VolumeService, transport),
  storage: createPromiseClient(StorageClassService, transport),
  audit: createPromiseClient(AuditService, transport),
};
