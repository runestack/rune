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
