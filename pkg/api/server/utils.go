package server

import (
	"context"
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
	case strings.HasPrefix(method, "/rune.api.AdminService/"):
		return "admin", "*"
	default:
		return "*", "*"
	}
}

// methodToResource maps a gRPC method to a resource
func methodToResource(method string) string {
	switch {
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
	if rv.Kind() == reflect.Struct {
		f := rv.FieldByName("Namespace")
		if f.IsValid() && f.Kind() == reflect.String {
			return f.String()
		}
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
