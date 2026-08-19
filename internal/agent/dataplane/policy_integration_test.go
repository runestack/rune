package dataplane

import (
	"context"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/runestack/rune/pkg/networking/endpoints"
	"github.com/runestack/rune/pkg/networking/localinstances"
	"github.com/runestack/rune/pkg/networking/policy"
	"github.com/runestack/rune/pkg/types"
)

// startEcho stands up a 127.0.0.1 TCP echo server.
func startEcho(t *testing.T) *net.TCPAddr {
	t.Helper()
	echo, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("echo listen: %v", err)
	}
	t.Cleanup(func() { _ = echo.Close() })
	go func() {
		for {
			c, err := echo.Accept()
			if err != nil {
				return
			}
			go func(conn net.Conn) {
				defer conn.Close()
				_, _ = io.Copy(conn, conn)
			}(c)
		}
	}()
	return echo.Addr().(*net.TCPAddr)
}

// startDataplane builds + starts a dev-mode Subsystem with both
// endpoints and local_instances Op kinds registered.
func startDataplane(t *testing.T) *Subsystem {
	t.Helper()
	ol := newTestOlog(t)
	if err := endpoints.Register(ol); err != nil {
		t.Fatalf("endpoints register: %v", err)
	}
	if err := localinstances.Register(ol); err != nil {
		t.Fatalf("local_instances register: %v", err)
	}
	dp, err := New(Config{OrderedLog: ol, Mode: ModeDev, Node: StaticNodeID("nodeA")})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := dp.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() {
		stopCtx, c := context.WithTimeout(context.Background(), 2*time.Second)
		defer c()
		_ = dp.Stop(stopCtx)
	})
	return dp
}

// publishEndpoint pushes a single healthy endpoint via the public
// publisher API.
func publishEndpoint(t *testing.T, dp *Subsystem, svcID string, ip string, port int) {
	t.Helper()
	pub := endpoints.NewPublisher(dp.cfg.OrderedLog)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := pub.Update(ctx, svcID, []types.Endpoint{{IP: ip, Port: port, Healthy: true}}); err != nil {
		t.Fatalf("publish endpoints: %v", err)
	}
}

func TestPolicyIngress_DenyByDefault(t *testing.T) {
	echo := startEcho(t)
	dp := startDataplane(t)

	proxyPort := mustFreePort(t)
	svc := &types.Service{
		ID:        "api",
		Namespace: "prod",
		Ports: []types.ServicePort{
			{Name: "tcp", Port: proxyPort, TargetPort: echo.Port, Protocol: "tcp"},
		},
		Discovery: &types.ServiceDiscovery{VIP: "ignored-in-dev"},
		// Default-deny: any ingress rule defined; loopback isn't covered.
		NetworkPolicy: &types.ServiceNetworkPolicy{
			Ingress: []types.IngressRule{
				{From: []types.NetworkPolicyPeer{{CIDR: "10.0.0.0/24"}}},
			},
		},
	}
	if err := dp.RegisterService(svc); err != nil {
		t.Fatalf("RegisterService: %v", err)
	}
	publishEndpoint(t, dp, "api", "127.0.0.1", echo.Port)
	if !waitForCache(dp, "api", 1, 2*time.Second) {
		t.Fatal("cache did not learn endpoints")
	}

	// 127.0.0.1 is not in 10.0.0.0/24 → connection should be dropped.
	conn, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", itoa(proxyPort)), 2*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	_, _ = conn.Write([]byte("ping"))
	conn.(*net.TCPConn).CloseWrite()
	got, _ := io.ReadAll(conn)
	if len(got) != 0 {
		t.Fatalf("expected empty (denied), got %q", got)
	}
}

func TestPolicyIngress_AllowByCIDR(t *testing.T) {
	echo := startEcho(t)
	dp := startDataplane(t)

	proxyPort := mustFreePort(t)
	svc := &types.Service{
		ID:        "api2",
		Namespace: "prod",
		Ports: []types.ServicePort{
			{Name: "tcp", Port: proxyPort, TargetPort: echo.Port, Protocol: "tcp"},
		},
		Discovery: &types.ServiceDiscovery{VIP: "ignored"},
		NetworkPolicy: &types.ServiceNetworkPolicy{
			Ingress: []types.IngressRule{
				{From: []types.NetworkPolicyPeer{{CIDR: "127.0.0.0/8"}}},
			},
		},
	}
	if err := dp.RegisterService(svc); err != nil {
		t.Fatalf("RegisterService: %v", err)
	}
	publishEndpoint(t, dp, "api2", "127.0.0.1", echo.Port)
	if !waitForCache(dp, "api2", 1, 2*time.Second) {
		t.Fatal("cache did not learn endpoints")
	}

	conn, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", itoa(proxyPort)), 2*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	_, _ = conn.Write([]byte("hi"))
	conn.(*net.TCPConn).CloseWrite()
	got, _ := io.ReadAll(conn)
	if string(got) != "hi" {
		t.Fatalf("expected echo 'hi', got %q", got)
	}
}

func TestPolicyIngress_LocalInstancesIdentity(t *testing.T) {
	echo := startEcho(t)
	dp := startDataplane(t)

	// Pre-populate the LocalInstances table with 127.0.0.1 -> service "web/prod"
	// to simulate a same-node container peer.
	pub := localinstances.NewPublisher(dp.cfg.OrderedLog)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := pub.Update(ctx, "nodeA", map[string]types.InstanceIdentity{
		"127.0.0.1": {InstanceID: "i1", Service: "web", Namespace: "prod"},
	}); err != nil {
		t.Fatalf("publish local_instances: %v", err)
	}
	// Wait for the table to apply.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if dp.LocalInstances().Size() == 1 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if dp.LocalInstances().Size() != 1 {
		t.Fatalf("local_instances table not hydrated, size=%d", dp.LocalInstances().Size())
	}

	proxyPort := mustFreePort(t)
	svc := &types.Service{
		ID:        "api3",
		Namespace: "prod",
		Ports: []types.ServicePort{
			{Name: "tcp", Port: proxyPort, TargetPort: echo.Port, Protocol: "tcp"},
		},
		Discovery: &types.ServiceDiscovery{VIP: "ignored"},
		NetworkPolicy: &types.ServiceNetworkPolicy{
			Ingress: []types.IngressRule{
				// Allow only "web" in "prod" by service-name.
				{From: []types.NetworkPolicyPeer{{Service: "web", Namespace: "prod"}}},
			},
		},
	}
	if err := dp.RegisterService(svc); err != nil {
		t.Fatalf("RegisterService: %v", err)
	}
	publishEndpoint(t, dp, "api3", "127.0.0.1", echo.Port)
	if !waitForCache(dp, "api3", 1, 2*time.Second) {
		t.Fatal("cache did not learn endpoints")
	}

	conn, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", itoa(proxyPort)), 2*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	_, _ = conn.Write([]byte("howdy"))
	conn.(*net.TCPConn).CloseWrite()
	got, _ := io.ReadAll(conn)
	if string(got) != "howdy" {
		t.Fatalf("expected echo 'howdy', got %q", got)
	}
}

func TestPolicyIngress_NoPolicyServiceIsOpen(t *testing.T) {
	echo := startEcho(t)
	dp := startDataplane(t)

	proxyPort := mustFreePort(t)
	svc := &types.Service{
		ID: "open",
		Ports: []types.ServicePort{
			{Name: "tcp", Port: proxyPort, TargetPort: echo.Port, Protocol: "tcp"},
		},
		Discovery: &types.ServiceDiscovery{VIP: "ignored"},
	}
	if err := dp.RegisterService(svc); err != nil {
		t.Fatalf("RegisterService: %v", err)
	}
	publishEndpoint(t, dp, "open", "127.0.0.1", echo.Port)
	if !waitForCache(dp, "open", 1, 2*time.Second) {
		t.Fatal("cache did not learn endpoints")
	}

	conn, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", itoa(proxyPort)), 2*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	_, _ = conn.Write([]byte("open"))
	conn.(*net.TCPConn).CloseWrite()
	got, _ := io.ReadAll(conn)
	if string(got) != "open" {
		t.Fatalf("expected echo 'open', got %q", got)
	}
}

// publishLocalIdentity maps 127.0.0.1 -> (service, namespace) on
// nodeA and waits for the table to apply, simulating a same-node
// container peer.
func publishLocalIdentity(t *testing.T, dp *Subsystem, service, namespace string) {
	t.Helper()
	pub := localinstances.NewPublisher(dp.cfg.OrderedLog)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := pub.Update(ctx, "nodeA", map[string]types.InstanceIdentity{
		"127.0.0.1": {InstanceID: "i1", Service: service, Namespace: namespace},
	}); err != nil {
		t.Fatalf("publish local_instances: %v", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if dp.LocalInstances().Size() == 1 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("local_instances table not hydrated, size=%d", dp.LocalInstances().Size())
}

func TestPolicyEgress_DenySameNodeSource(t *testing.T) {
	echo := startEcho(t)
	dp := startDataplane(t)
	publishLocalIdentity(t, dp, "web", "prod")

	// The source service "web" may only dial db/infra. The open
	// destination service "api" must therefore be unreachable even
	// though it has no ingress policy of its own.
	if err := dp.RegisterService(&types.Service{
		ID:        "web-id",
		Name:      "web",
		Namespace: "prod",
		NetworkPolicy: &types.ServiceNetworkPolicy{
			Egress: []types.EgressRule{
				{To: []types.NetworkPolicyPeer{{Service: "db", Namespace: "infra"}}},
			},
		},
	}); err != nil {
		t.Fatalf("RegisterService(web): %v", err)
	}

	proxyPort := mustFreePort(t)
	dst := &types.Service{
		ID:        "api-id",
		Name:      "api",
		Namespace: "prod",
		Ports: []types.ServicePort{
			{Name: "tcp", Port: proxyPort, TargetPort: echo.Port, Protocol: "tcp"},
		},
		Discovery: &types.ServiceDiscovery{VIP: "ignored"},
	}
	if err := dp.RegisterService(dst); err != nil {
		t.Fatalf("RegisterService(api): %v", err)
	}
	publishEndpoint(t, dp, "api-id", "127.0.0.1", echo.Port)
	if !waitForCache(dp, "api-id", 1, 2*time.Second) {
		t.Fatal("cache did not learn endpoints")
	}

	conn, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", itoa(proxyPort)), 2*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	_, _ = conn.Write([]byte("ping"))
	conn.(*net.TCPConn).CloseWrite()
	got, _ := io.ReadAll(conn)
	if len(got) != 0 {
		t.Fatalf("expected empty (egress denied), got %q", got)
	}
	// An empty read alone also happens on no_endpoints/dial_failed, so
	// pin the actual decision too.
	res := dp.evaluatePolicy("api-id", net.ParseIP("127.0.0.1"), net.ParseIP("127.0.0.1"), proxyPort, "tcp")
	if res.Decision != policy.DecisionDeny || res.Direction != policy.DirectionEgress {
		t.Fatalf("want egress deny, got %v/%s (%s)", res.Decision, res.Reason, res.Direction)
	}
}

func TestPolicyEgress_AllowMatchingRule(t *testing.T) {
	echo := startEcho(t)
	dp := startDataplane(t)
	publishLocalIdentity(t, dp, "web", "prod")

	// "web" explicitly allows dialing api/prod → the connection must
	// pass egress and then the destination's (absent) ingress policy.
	if err := dp.RegisterService(&types.Service{
		ID:        "web-id2",
		Name:      "web",
		Namespace: "prod",
		NetworkPolicy: &types.ServiceNetworkPolicy{
			Egress: []types.EgressRule{
				{To: []types.NetworkPolicyPeer{{Service: "api", Namespace: "prod"}}},
			},
		},
	}); err != nil {
		t.Fatalf("RegisterService(web): %v", err)
	}

	proxyPort := mustFreePort(t)
	dst := &types.Service{
		ID:        "api-id2",
		Name:      "api",
		Namespace: "prod",
		Ports: []types.ServicePort{
			{Name: "tcp", Port: proxyPort, TargetPort: echo.Port, Protocol: "tcp"},
		},
		Discovery: &types.ServiceDiscovery{VIP: "ignored"},
	}
	if err := dp.RegisterService(dst); err != nil {
		t.Fatalf("RegisterService(api): %v", err)
	}
	publishEndpoint(t, dp, "api-id2", "127.0.0.1", echo.Port)
	if !waitForCache(dp, "api-id2", 1, 2*time.Second) {
		t.Fatal("cache did not learn endpoints")
	}

	conn, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", itoa(proxyPort)), 2*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	_, _ = conn.Write([]byte("hello"))
	conn.(*net.TCPConn).CloseWrite()
	got, _ := io.ReadAll(conn)
	if string(got) != "hello" {
		t.Fatalf("expected echo 'hello', got %q", got)
	}
}

func TestPolicyEgress_UnregisterClearsSourcePolicy(t *testing.T) {
	dp := startDataplane(t)
	publishLocalIdentity(t, dp, "web", "prod")

	if err := dp.RegisterService(&types.Service{
		ID:        "web-id3",
		Name:      "web",
		Namespace: "prod",
		NetworkPolicy: &types.ServiceNetworkPolicy{
			Egress: []types.EgressRule{
				{To: []types.NetworkPolicyPeer{{Service: "db", Namespace: "infra"}}},
			},
		},
	}); err != nil {
		t.Fatalf("RegisterService(web): %v", err)
	}
	if err := dp.RegisterService(&types.Service{
		ID:        "api-id3",
		Name:      "api",
		Namespace: "prod",
		Discovery: &types.ServiceDiscovery{VIP: "ignored"},
	}); err != nil {
		t.Fatalf("RegisterService(api): %v", err)
	}

	src := net.ParseIP("127.0.0.1")
	dst := net.ParseIP("127.0.0.1")
	res := dp.evaluatePolicy("api-id3", src, dst, 8080, "tcp")
	if res.Decision != policy.DecisionDeny || res.Direction != policy.DirectionEgress {
		t.Fatalf("want egress deny, got %v/%s (%s)", res.Decision, res.Reason, res.Direction)
	}
	// The denial must be attributed to the source, whose rules denied
	// it — not to the destination, which has no policy at all.
	if res.PolicyOwner.Name != "web" || res.PolicyOwner.Namespace != "prod" {
		t.Fatalf("want denial attributed to prod/web, got %q", res.PolicyOwner.String())
	}

	// Tearing down listeners must NOT drop policy: registerServiceDataplane
	// calls UnregisterService mid-reconcile on a VIP change, and the
	// source's instances keep running across that window.
	dp.UnregisterService("web-id3")
	res = dp.evaluatePolicy("api-id3", src, dst, 8080, "tcp")
	if res.Decision != policy.DecisionDeny {
		t.Fatalf("listener teardown must not open an enforcement hole, got %v/%s", res.Decision, res.Reason)
	}

	// Deleting the service is the one thing that drops its policy.
	dp.dropPolicy("web-id3")
	res = dp.evaluatePolicy("api-id3", src, dst, 8080, "tcp")
	if res.Decision == policy.DecisionDeny {
		t.Fatalf("want allow after delete, got %v/%s", res.Decision, res.Reason)
	}
}

// A port-less source service never gets a VIP listener, so it never
// reaches RegisterService — but it can still dial other services and
// must have its egress policy enforced. Exercises the reconciler path
// rather than RegisterService directly.
func TestPolicyEgress_PortlessSourceIsStillPolicied(t *testing.T) {
	dp := startDataplane(t)
	publishLocalIdentity(t, dp, "worker", "prod")

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	// A worker with no inbound ports, only egress rules.
	worker := &types.Service{
		ID:        "worker-id",
		Name:      "worker",
		Namespace: "prod",
		NetworkPolicy: &types.ServiceNetworkPolicy{
			Egress: []types.EgressRule{
				{To: []types.NetworkPolicyPeer{{Service: "db", Namespace: "infra"}}},
			},
		},
	}
	if err := dp.reconcileOneService(ctx, worker, false); err != nil {
		t.Fatalf("reconcileOneService(worker): %v", err)
	}
	if err := dp.RegisterService(&types.Service{
		ID:        "api-id4",
		Name:      "api",
		Namespace: "prod",
		Discovery: &types.ServiceDiscovery{VIP: "ignored"},
	}); err != nil {
		t.Fatalf("RegisterService(api): %v", err)
	}

	res := dp.evaluatePolicy("api-id4", net.ParseIP("127.0.0.1"), net.ParseIP("127.0.0.1"), 8080, "tcp")
	if res.Decision != policy.DecisionDeny || res.Direction != policy.DirectionEgress {
		t.Fatalf("want egress deny for port-less source, got %v/%s", res.Decision, res.Reason)
	}
}

// Egress and ingress compose as AND: passing the source's egress rules
// does not exempt a connection from the destination's ingress rules.
func TestPolicyEgress_AllowStillSubjectToIngress(t *testing.T) {
	dp := startDataplane(t)
	publishLocalIdentity(t, dp, "web", "prod")

	// web may dial api...
	if err := dp.RegisterService(&types.Service{
		ID: "web-id5", Name: "web", Namespace: "prod",
		NetworkPolicy: &types.ServiceNetworkPolicy{
			Egress: []types.EgressRule{
				{To: []types.NetworkPolicyPeer{{Service: "api", Namespace: "prod"}}},
			},
		},
	}); err != nil {
		t.Fatalf("RegisterService(web): %v", err)
	}
	// ...but api only accepts 10.0.0.0/24, and the peer is loopback.
	if err := dp.RegisterService(&types.Service{
		ID: "api-id5", Name: "api", Namespace: "prod",
		Discovery: &types.ServiceDiscovery{VIP: "ignored"},
		NetworkPolicy: &types.ServiceNetworkPolicy{
			Ingress: []types.IngressRule{
				{From: []types.NetworkPolicyPeer{{CIDR: "10.0.0.0/24"}}},
			},
		},
	}); err != nil {
		t.Fatalf("RegisterService(api): %v", err)
	}

	lo := net.ParseIP("127.0.0.1")
	res := dp.evaluatePolicy("api-id5", lo, lo, 8080, "tcp")
	if res.Decision != policy.DecisionDeny {
		t.Fatalf("egress allow must not bypass ingress: got %v/%s", res.Decision, res.Reason)
	}
	if res.Direction != policy.DirectionIngress {
		t.Fatalf("want the ingress rules to be the denier, got %q", res.Direction)
	}
	if res.PolicyOwner.Name != "api" {
		t.Fatalf("want denial attributed to api, got %q", res.PolicyOwner.String())
	}
}

// An explicit egress allow is reported as an allow when the
// destination carries no ingress policy of its own; otherwise egress
// decisions never reach the allow counter.
func TestPolicyEgress_AllowIsReportedWhenDestinationIsOpen(t *testing.T) {
	dp := startDataplane(t)
	publishLocalIdentity(t, dp, "web", "prod")

	if err := dp.RegisterService(&types.Service{
		ID: "web-id6", Name: "web", Namespace: "prod",
		NetworkPolicy: &types.ServiceNetworkPolicy{
			Egress: []types.EgressRule{
				{To: []types.NetworkPolicyPeer{{Service: "api", Namespace: "prod"}}},
			},
		},
	}); err != nil {
		t.Fatalf("RegisterService(web): %v", err)
	}
	if err := dp.RegisterService(&types.Service{
		ID: "api-id6", Name: "api", Namespace: "prod",
		Discovery: &types.ServiceDiscovery{VIP: "ignored"},
	}); err != nil {
		t.Fatalf("RegisterService(api): %v", err)
	}

	lo := net.ParseIP("127.0.0.1")
	res := dp.evaluatePolicy("api-id6", lo, lo, 8080, "tcp")
	if res.Decision != policy.DecisionAllow || res.Direction != policy.DirectionEgress {
		t.Fatalf("want reported egress allow, got %v/%s (%s)", res.Decision, res.Reason, res.Direction)
	}
}

// A source whose IP is not in the LocalInstances table cannot be
// identified, so its egress rules are not consulted and the
// connection proceeds to the destination's ingress check. This is a
// deliberate choice — the ingress controller and host-originated
// dials legitimately have no instance identity — and it is the reason
// egress is containment between known services, not a boundary that
// holds against an unidentified source.
func TestPolicyEgress_UnidentifiedSourceIsNotEgressFiltered(t *testing.T) {
	dp := startDataplane(t)
	publishLocalIdentity(t, dp, "web", "prod")

	if err := dp.RegisterService(&types.Service{
		ID: "web-id7", Name: "web", Namespace: "prod",
		NetworkPolicy: &types.ServiceNetworkPolicy{
			Egress: []types.EgressRule{
				{To: []types.NetworkPolicyPeer{{Service: "db", Namespace: "infra"}}},
			},
		},
	}); err != nil {
		t.Fatalf("RegisterService(web): %v", err)
	}
	if err := dp.RegisterService(&types.Service{
		ID: "api-id7", Name: "api", Namespace: "prod",
		Discovery: &types.ServiceDiscovery{VIP: "ignored"},
	}); err != nil {
		t.Fatalf("RegisterService(api): %v", err)
	}

	// 10.1.2.3 is in no LocalInstances entry.
	unknown := net.ParseIP("10.1.2.3")
	res := dp.evaluatePolicy("api-id7", unknown, net.ParseIP("127.0.0.1"), 8080, "tcp")
	if res.Decision == policy.DecisionDeny {
		t.Fatalf("unidentified source is not egress-filtered; got %v/%s", res.Decision, res.Reason)
	}
}

func TestPolicyEvaluate_DirectAccess(t *testing.T) {
	// Sanity: PolicyFor returns the compiled instance.
	dp := startDataplane(t)
	svc := &types.Service{
		ID:        "x",
		Namespace: "ns",
		Ports:     []types.ServicePort{{Name: "p", Port: mustFreePort(t), Protocol: "tcp"}},
		Discovery: &types.ServiceDiscovery{VIP: "ignored"},
		NetworkPolicy: &types.ServiceNetworkPolicy{
			Ingress: []types.IngressRule{{From: []types.NetworkPolicyPeer{{CIDR: "10.0.0.0/8"}}}},
		},
	}
	if err := dp.RegisterService(svc); err != nil {
		t.Fatalf("Register: %v", err)
	}
	c := dp.PolicyFor("x")
	if c == nil {
		t.Fatal("expected compiled policy")
	}
	r := c.EvaluateIngress(policy.PeerInfo{IP: net.ParseIP("10.1.2.3")}, 80, "tcp")
	if r.Decision != policy.DecisionAllow {
		t.Fatalf("want Allow, got %v", r.Decision)
	}
	r = c.EvaluateIngress(policy.PeerInfo{IP: net.ParseIP("8.8.8.8")}, 80, "tcp")
	if r.Decision != policy.DecisionDeny {
		t.Fatalf("want Deny, got %v", r.Decision)
	}
	// Sanity check Reason is a known token.
	if !strings.HasPrefix(string(r.Reason), "no_") && !strings.HasPrefix(string(r.Reason), "port_") &&
		!strings.HasPrefix(string(r.Reason), "cross_") {
		t.Fatalf("unexpected reason token %q", r.Reason)
	}
}
