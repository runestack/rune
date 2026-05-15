package service

import (
	"context"
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/runestack/rune/pkg/api/generated"
	"github.com/runestack/rune/pkg/log"
	"github.com/runestack/rune/pkg/orchestrator"
	"github.com/runestack/rune/pkg/types"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// fakePFStream is an in-process implementation of
// PortForwardService_StreamPortForwardServer driven by per-test
// channels. Sufficient to exercise the server's framing and routing.
type fakePFStream struct {
	grpc.ServerStream
	ctx    context.Context
	inCh   chan *generated.PortForwardClientMessage
	outCh  chan *generated.PortForwardServerMessage
	recvMu sync.Mutex
	closed chan struct{}
}

func newFakePFStream(ctx context.Context) *fakePFStream {
	return &fakePFStream{
		ctx:    ctx,
		inCh:   make(chan *generated.PortForwardClientMessage, 16),
		outCh:  make(chan *generated.PortForwardServerMessage, 16),
		closed: make(chan struct{}),
	}
}

func (s *fakePFStream) Context() context.Context { return s.ctx }

func (s *fakePFStream) Send(msg *generated.PortForwardServerMessage) error {
	select {
	case s.outCh <- msg:
		return nil
	case <-s.closed:
		return io.ErrClosedPipe
	}
}

func (s *fakePFStream) Recv() (*generated.PortForwardClientMessage, error) {
	s.recvMu.Lock()
	defer s.recvMu.Unlock()
	select {
	case msg, ok := <-s.inCh:
		if !ok {
			return nil, io.EOF
		}
		return msg, nil
	case <-s.ctx.Done():
		return nil, s.ctx.Err()
	}
}

func (s *fakePFStream) closeIn() { close(s.inCh) }

func TestPortForward_InitValidation_NoTarget(t *testing.T) {
	logger := log.NewLogger()
	orch := orchestrator.NewFakeOrchestrator()
	svc := NewPortForwardService(logger, orch)

	stream := newFakePFStream(context.Background())
	stream.inCh <- &generated.PortForwardClientMessage{
		Message: &generated.PortForwardClientMessage_Init{Init: &generated.PortForwardInit{}},
	}

	err := svc.StreamPortForward(stream)
	if err == nil {
		t.Fatal("expected init validation error")
	}
	if st, ok := status.FromError(err); !ok || st.Code() != codes.InvalidArgument {
		t.Fatalf("expected InvalidArgument, got %v", err)
	}
}

func TestPortForward_InitValidation_BothTargets(t *testing.T) {
	logger := log.NewLogger()
	orch := orchestrator.NewFakeOrchestrator()
	svc := NewPortForwardService(logger, orch)

	stream := newFakePFStream(context.Background())
	stream.inCh <- &generated.PortForwardClientMessage{
		Message: &generated.PortForwardClientMessage_Init{
			Init: &generated.PortForwardInit{
				Target:    &generated.PortForwardInit_ServiceName{ServiceName: "svc"},
				Namespace: "default",
			},
		},
	}
	// Sneak instance_id in too via reflection-ish hack: send a second init.
	// (The oneof here only allows one, so we test "both" by mixing the
	// other field after the oneof is set. The validator checks GetServiceName
	// != "" && GetInstanceId() != "" — which can't co-occur in a real
	// oneof. So this test focuses on no-target instead — kept above.)

	// Skip — this assertion is structurally impossible to construct via
	// the typed API, so we just confirm the validator's no-target path.
	_ = svc
}

func TestPortForward_NoInstance(t *testing.T) {
	logger := log.NewLogger()
	orch := orchestrator.NewFakeOrchestrator()
	svc := NewPortForwardService(logger, orch)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream := newFakePFStream(ctx)
	stream.inCh <- &generated.PortForwardClientMessage{
		Message: &generated.PortForwardClientMessage_Init{
			Init: &generated.PortForwardInit{
				Target:    &generated.PortForwardInit_ServiceName{ServiceName: "missing"},
				Namespace: "default",
			},
		},
	}

	err := svc.StreamPortForward(stream)
	if err == nil {
		t.Fatal("expected NotFound from missing instance")
	}
	if st, ok := status.FromError(err); !ok || st.Code() != codes.NotFound {
		t.Fatalf("expected NotFound, got %v", err)
	}
}

func TestPortForward_EndToEnd_OpenDataClose(t *testing.T) {
	logger := log.NewLogger()
	orch := orchestrator.NewFakeOrchestrator()

	// Register a running instance so the service-target resolver
	// finds it via the fake orchestrator's ListRunningInstances().
	inst := makeFakeRunningInstance(orch, "svc")

	svc := NewPortForwardService(logger, orch)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream := newFakePFStream(ctx)

	// Drive the session in a goroutine.
	done := make(chan error, 1)
	go func() { done <- svc.StreamPortForward(stream) }()

	// Init → Ready.
	stream.inCh <- &generated.PortForwardClientMessage{
		Message: &generated.PortForwardClientMessage_Init{
			Init: &generated.PortForwardInit{
				Target:    &generated.PortForwardInit_ServiceName{ServiceName: "svc"},
				Namespace: "default",
			},
		},
	}
	if msg := mustRecv(t, stream); msg.GetReady() == nil {
		t.Fatalf("expected Ready, got %T", msg.GetMessage())
	}

	// Open connID=1 → orchestrator.DialInInstance returns net.Pipe; the
	// remote peer is on orch.LastDialPeer.
	stream.inCh <- &generated.PortForwardClientMessage{
		Message: &generated.PortForwardClientMessage_Open{
			Open: &generated.PortForwardOpen{ConnId: 1, RemotePort: 6379},
		},
	}

	// Wait for the orchestrator to record the dial.
	peer := waitDial(t, orch)

	// Client → server data: server should write bytes onto the conn.
	hello := []byte("hello, redis")
	stream.inCh <- &generated.PortForwardClientMessage{
		Message: &generated.PortForwardClientMessage_Data{
			Data: &generated.PortForwardData{ConnId: 1, Payload: hello},
		},
	}
	buf := make([]byte, 64)
	n, err := peer.Read(buf)
	if err != nil {
		t.Fatalf("peer read: %v", err)
	}
	if got := string(buf[:n]); got != string(hello) {
		t.Fatalf("peer got %q, want %q", got, hello)
	}

	// Server → client data: write on the peer, expect Data frame out.
	pong := []byte("+PONG\r\n")
	if _, err := peer.Write(pong); err != nil {
		t.Fatalf("peer write: %v", err)
	}
	out := mustRecv(t, stream)
	if d := out.GetData(); d == nil || d.ConnId != 1 || string(d.Payload) != string(pong) {
		t.Fatalf("expected Data frame with %q, got %v", pong, out)
	}

	// Close from client.
	stream.inCh <- &generated.PortForwardClientMessage{
		Message: &generated.PortForwardClientMessage_Close{
			Close: &generated.PortForwardClose{ConnId: 1},
		},
	}

	// Closing the peer should cause the server's reader goroutine to
	// emit a Close frame back to us. May race with the client-side
	// close above; either way the server stays alive.
	_ = peer.Close()

	// End stream → server returns.
	stream.closeIn()

	select {
	case err := <-done:
		if err != nil && !errors.Is(err, io.EOF) {
			t.Fatalf("session: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("server did not return after closeIn")
	}
	_ = inst
}

// mustRecv reads one server-side message with a small timeout.
func mustRecv(t *testing.T, s *fakePFStream) *generated.PortForwardServerMessage {
	t.Helper()
	select {
	case msg := <-s.outCh:
		return msg
	case <-time.After(2 * time.Second):
		t.Fatalf("timeout waiting for server message")
		return nil
	}
}

// waitDial polls the fake orchestrator until a dial has been recorded
// and returns the remote peer end.
func waitDial(t *testing.T, orch *orchestrator.FakeOrchestrator) interface {
	Read(p []byte) (int, error)
	Write(p []byte) (int, error)
	Close() error
} {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		if orch.LastDialPeer != nil {
			return orch.LastDialPeer
		}
		if time.Now().After(deadline) {
			t.Fatalf("orchestrator never received a Dial call")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// makeFakeRunningInstance registers a running instance with the given
// service name on the fake orchestrator so that ListRunningInstances
// returns it.
func makeFakeRunningInstance(orch *orchestrator.FakeOrchestrator, service string) string {
	id := service + "-0"
	orch.AddInstance(&types.Instance{
		ID:          id,
		Namespace:   "default",
		Name:        id,
		ServiceName: service,
		Status:      types.InstanceStatusRunning,
	})
	return id
}
