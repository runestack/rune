package server

import (
	"context"
	"fmt"
	"net"
	"reflect"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
)

// methodToAction maps a gRPC method to (resource, verb)
func methodToAction(method string) (string, string) {
	switch {
	case strings.HasPrefix(method, "/rune.api.ServiceService/Get"):
		return "services", "get"
	case strings.HasPrefix(method, "/rune.api.ServiceService/List"):
		return "services", "list"
	case strings.HasPrefix(method, "/rune.api.ServiceService/Create"):
		return "services", "create"
	case strings.HasPrefix(method, "/rune.api.ServiceService/Update"):
		return "services", "update"
	case strings.HasPrefix(method, "/rune.api.ServiceService/Delete"):
		return "services", "delete"
	case strings.HasPrefix(method, "/rune.api.ServiceService/Scale"):
		return "services", "scale"
	case strings.HasPrefix(method, "/rune.api.ServiceService/Restart"):
		// Restart is scale-class: it changes which containers run without
		// changing the spec. Reusing the verb keeps existing tokens working.
		return "services", "scale"

	case strings.HasPrefix(method, "/rune.api.InstanceService/Get"):
		return "instances", "get"
	case strings.HasPrefix(method, "/rune.api.InstanceService/List"):
		return "instances", "list"
	case strings.HasPrefix(method, "/rune.api.InstanceService/Watch"):
		return "instances", "watch"

	case strings.HasPrefix(method, "/rune.api.LogService/StreamLogs"):
		return "logs", "get"
	case strings.HasPrefix(method, "/rune.api.LogService/GetLogs"):
		// Browser-callable server-streaming logs (RUNE-200C). Same
		// authorization surface as StreamLogs.
		return "logs", "get"
	case strings.HasPrefix(method, "/rune.api.ExecService/StreamExec"):
		return "exec", "exec"
	case strings.HasPrefix(method, "/rune.api.PortForwardService/StreamPortForward"):
		// RUNE-122. Narrower than services.exec — only reaches
		// already-listening ports, never a shell.
		return "services", "port-forward"
	case strings.HasPrefix(method, "/rune.api.HealthService/GetHealth"):
		return "health", "get"

	// Specific cases first: ListConfigmapVersions must not match the generic
	// /List prefix below (versions is a read = get, not list).
	case strings.HasPrefix(method, "/rune.api.ConfigmapService/ListConfigmapVersions"):
		return "configmaps", "get"
	case strings.HasPrefix(method, "/rune.api.ConfigmapService/PatchConfigmap"):
		return "configmaps", "update"
	case strings.HasPrefix(method, "/rune.api.ConfigmapService/RollbackConfigmap"):
		return "configmaps", "update"
	case strings.HasPrefix(method, "/rune.api.ConfigmapService/Get"):
		return "configmaps", "get"
	case strings.HasPrefix(method, "/rune.api.ConfigmapService/List"):
		return "configmaps", "list"
	case strings.HasPrefix(method, "/rune.api.ConfigmapService/Create"):
		return "configmaps", "create"
	case strings.HasPrefix(method, "/rune.api.ConfigmapService/Update"):
		return "configmaps", "update"
	case strings.HasPrefix(method, "/rune.api.ConfigmapService/Delete"):
		return "configmaps", "delete"

	case strings.HasPrefix(method, "/rune.api.SecretService/RollbackSecret"):
		return "secrets", "update"
	case strings.HasPrefix(method, "/rune.api.SecretService/Patch"):
		// PatchSecret is a server-side merge: caller never sees other keys'
		// plaintext, so it only needs secrets:update (not secrets:reveal).
		return "secrets", "update"
	case strings.HasPrefix(method, "/rune.api.SecretService/ListSecretVersions"):
		return "secrets", "get"
	case strings.HasPrefix(method, "/rune.api.SecretService/RevealSecretVersion"):
		return "secrets", "reveal"
	case strings.HasPrefix(method, "/rune.api.SecretService/Reveal"):
		return "secrets", "reveal"
	case strings.HasPrefix(method, "/rune.api.SecretService/Get"):
		return "secrets", "get"
	case strings.HasPrefix(method, "/rune.api.SecretService/List"):
		return "secrets", "list"
	case strings.HasPrefix(method, "/rune.api.SecretService/Create"):
		return "secrets", "create"
	case strings.HasPrefix(method, "/rune.api.SecretService/Update"):
		return "secrets", "update"
	case strings.HasPrefix(method, "/rune.api.SecretService/Delete"):
		return "secrets", "delete"

	case strings.HasPrefix(method, "/rune.api.AuditService/List"):
		return "audit", "list"
	case strings.HasPrefix(method, "/rune.api.AuditService/Get"):
		return "audit", "get"

	case strings.HasPrefix(method, "/rune.api.StorageClassService/Get"):
		return "storageclasses", "get"
	case strings.HasPrefix(method, "/rune.api.StorageClassService/List"):
		return "storageclasses", "list"
	case strings.HasPrefix(method, "/rune.api.StorageClassService/Create"):
		return "storageclasses", "create"
	case strings.HasPrefix(method, "/rune.api.StorageClassService/Update"):
		return "storageclasses", "update"
	case strings.HasPrefix(method, "/rune.api.StorageClassService/Delete"):
		return "storageclasses", "delete"

	case strings.HasPrefix(method, "/rune.api.VolumeService/Get"):
		return "volumes", "get"
	case strings.HasPrefix(method, "/rune.api.VolumeService/List"):
		return "volumes", "list"
	case strings.HasPrefix(method, "/rune.api.VolumeService/Create"):
		return "volumes", "create"
	case strings.HasPrefix(method, "/rune.api.VolumeService/Update"):
		return "volumes", "update"
	case strings.HasPrefix(method, "/rune.api.VolumeService/Delete"):
		return "volumes", "delete"
	case strings.HasPrefix(method, "/rune.api.VolumeService/RetryProvision"):
		return "volumes", "update"
	case strings.HasPrefix(method, "/rune.api.VolumeService/Detach"):
		return "volumes", "update"

	case strings.HasPrefix(method, "/rune.api.SnapshotService/Get"):
		return "snapshots", "get"
	case strings.HasPrefix(method, "/rune.api.SnapshotService/List"):
		return "snapshots", "list"
	case strings.HasPrefix(method, "/rune.api.SnapshotService/Create"):
		return "snapshots", "create"
	case strings.HasPrefix(method, "/rune.api.SnapshotService/Delete"):
		return "snapshots", "delete"
	case strings.HasPrefix(method, "/rune.api.SnapshotService/RestoreVolume"):
		return "volumes", "create"

	// Releases: stateful runeset releases (RUNESET_STATEFUL_RELEASES.md).
	// Cast is the install/upgrade write; Plan is a dry-run read; Rollback is a
	// re-apply write; DeleteRelease is the uninstall.
	case strings.HasPrefix(method, "/rune.api.ReleaseService/Cast"):
		return "releases", "create"
	case strings.HasPrefix(method, "/rune.api.ReleaseService/Plan"):
		return "releases", "get"
	case strings.HasPrefix(method, "/rune.api.ReleaseService/ListReleases"):
		return "releases", "list"
	case strings.HasPrefix(method, "/rune.api.ReleaseService/GetRelease"):
		return "releases", "get"
	case strings.HasPrefix(method, "/rune.api.ReleaseService/History"):
		return "releases", "get"
	case strings.HasPrefix(method, "/rune.api.ReleaseService/DeleteRelease"):
		return "releases", "delete"
	case strings.HasPrefix(method, "/rune.api.ReleaseService/Rollback"):
		return "releases", "update"
	case strings.HasPrefix(method, "/rune.api.ReleaseService/RevealReleaseValues"):
		// Values can carry the same plaintext secrets:reveal gates, so they get
		// their own verb rather than riding on releases:get. No builtin policy
		// below admin grants it — mirrors secrets:reveal.
		return "releases", "reveal"

	// Observability (RuneSight, native). Execute/GetCapabilities are reads of
	// the log store; PushLogs is the agent forwarder's ingest path on
	// multi-node. All map to resource "logs" so an existing logs reader can
	// query history without a new policy.
	case strings.HasPrefix(method, "/rune.api.ObserveService/PushLogs"):
		return "logs", "observe"
	case strings.HasPrefix(method, "/rune.api.ObserveService/"):
		return "logs", "observe"

	case strings.HasPrefix(method, "/rune.api.EventService/"):
		// RUNE-126 Phase 2. Reading the event log is the same privilege
		// shape as describe — both are diagnostic reads across kinds.
		return "*", "get"
	case strings.HasPrefix(method, "/rune.api.DescribeService/Describe"):
		// RUNE-126. A polymorphic diagnostic read across kinds; the
		// method string can't carry the target kind, so it maps to a
		// generic read. Describe never reveals secret *data* — only
		// whether a referenced resource resolved. Any reader token
		// (readonly/readwrite/root) carries get on resource "*".
		return "*", "get"
	case strings.HasPrefix(method, "/rune.api.AuthService/WhoAmI"):
		return "auth", "get"
	case strings.HasPrefix(method, "/rune.api.AdminService/UpgradeServer"):
		// Deliberately NOT ("admin","*"): resource "admin" carries the
		// localhost-only gate (adminUnaryInterceptor), and a remote
		// operator upgrading without SSH is the point of the feature —
		// the alternative, auth.allow_remote_admin, would open the entire
		// admin surface to the network to enable one RPC.
		//
		// "server" rather than "system": "system" is already the built-in
		// namespace name, and a policy author writing rules should not
		// have to hold two meanings for one word. Of the shipped builtins
		// only root and admin (both *:*) grant it, but the authorizer
		// matches any rule naming the resource, so a custom policy can
		// grant it too. Must never be added to publicMethodSuffixes;
		// TestUpgradeServerRBAC pins this case and its methodToResource
		// twin in sync.
		return "server", "upgrade"
	case strings.HasPrefix(method, "/rune.api.AdminService/"):
		return "admin", "*"
	default:
		return "*", "*"
	}
}

// methodToResource maps a gRPC method to a resource
func methodToResource(method string) string {
	switch {
	case strings.HasPrefix(method, "/rune.api.AdminService/UpgradeServer"):
		// Must stay in sync with methodToAction's case above — resource
		// "admin" would re-arm the localhost-only gate.
		return "server"
	case strings.HasPrefix(method, "/rune.api.AdminService/"):
		return "admin"
	default:
		return ""
	}
}

// extractNamespace best-effort extracts a Namespace string field from request
func extractNamespace(req interface{}) string {
	rv := reflect.ValueOf(req)
	if rv.Kind() == reflect.Ptr {
		rv = rv.Elem()
	}
	if rv.Kind() != reflect.Struct {
		return ""
	}
	// Common case: a top-level Namespace field.
	if ns := stringFieldValue(rv, "Namespace"); ns != "" {
		return ns
	}
	// Otherwise the namespace is nested inside the request's payload message:
	// CreateServiceRequest.Service.Namespace, CreateSecretRequest.Secret.Namespace,
	// ReleaseService Cast/Plan's Spec.Namespace, and so on. Scan one level into
	// the request's exported struct / *struct fields and return the first
	// Namespace found. Without this the RBAC check runs with an empty namespace,
	// which never matches a namespace-pinned grant — so a scoped service-account
	// token (`admin service create --namespace X`) is wrongly denied on every
	// write whose request wraps its payload.
	rt := rv.Type()
	for i := 0; i < rv.NumField(); i++ {
		if !rt.Field(i).IsExported() {
			continue
		}
		fv := rv.Field(i)
		if fv.Kind() == reflect.Ptr {
			if fv.IsNil() {
				continue
			}
			fv = fv.Elem()
		}
		if fv.Kind() != reflect.Struct {
			continue
		}
		if ns := stringFieldValue(fv, "Namespace"); ns != "" {
			return ns
		}
	}
	return ""
}

// stringFieldValue returns the named string field of a struct value, or "".
func stringFieldValue(rv reflect.Value, name string) string {
	f := rv.FieldByName(name)
	if f.IsValid() && f.Kind() == reflect.String {
		return f.String()
	}
	return ""
}

func peerFromContext(ctx context.Context) (string, bool) {
	p, ok := peer.FromContext(ctx)
	if !ok || p.Addr == nil {
		return "", false
	}
	return p.Addr.String(), true
}

func isLocalhost(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return host == "localhost"
	}
	return ip.IsLoopback()
}

// loopbackDialAddr converts an HTTP listen address (e.g. ":7861" or
// "0.0.0.0:7861") into a loopback dial target ("127.0.0.1:7861") for
// control-plane port-forwarding. A concrete non-wildcard host is preserved.
func loopbackDialAddr(listenAddr string) string {
	host, port, err := net.SplitHostPort(listenAddr)
	if err != nil {
		return listenAddr
	}
	switch host {
	case "", "0.0.0.0", "::", "[::]":
		host = "127.0.0.1"
	}
	return net.JoinHostPort(host, port)
}

func statusPermissionDenied(msg string) error { return status.Error(codes.PermissionDenied, msg) }

// deniedErr builds an authorization-failure error that names the subject, the
// (resource, verb) attempted, and the namespace the check ran under. The
// namespace is the high-signal part: a denial on a namespace-scoped token
// almost always means the request resolved to a different namespace than the
// grant is pinned to, and surfacing it makes that diagnosable without server
// logs.
func deniedErr(ai *AuthInfo, resource, verb, namespace string) error {
	subject := "unknown subject"
	if ai != nil && ai.SubjectID != "" {
		if ai.SubjectType != "" {
			subject = fmt.Sprintf("%s (%s)", ai.SubjectID, ai.SubjectType)
		} else {
			subject = ai.SubjectID
		}
	}
	nsLabel := fmt.Sprintf("%q", namespace)
	if namespace == "" {
		nsLabel = `"" (cluster-scoped)`
	}
	return statusPermissionDenied(fmt.Sprintf(
		"access denied: subject %s is not permitted to %s %s in namespace %s",
		subject, verb, resource, nsLabel))
}
