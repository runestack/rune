package client

import (
	"context"
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/runestack/rune/pkg/api/generated"
	"github.com/runestack/rune/pkg/log"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
)

// fakeBidiStream implements PortForwardService_StreamPortForwardClient.
// Send just records the call; Recv blocks until the test wakes it.
type fakeBidiStream struct {
	grpc.ClientStream
	sent      int64
	sendDelay time.Duration
	recvCh    chan *generated.PortForwardServerMessage
}

func (s *fakeBidiStream) Send(*generated.PortForwardClientMessage) error {
	if s.sendDelay > 0 {
		time.Sleep(s.sendDelay)
	}
	atomic.AddInt64(&s.sent, 1)
	return nil
}

func (s *fakeBidiStream) Recv() (*generated.PortForwardServerMessage, error) {
	msg, ok := <-s.recvCh
	if !ok {
		return nil, io.EOF
	}
	return msg, nil
}

func (s *fakeBidiStream) CloseSend() error             { return nil }
func (s *fakeBidiStream) Context() context.Context     { return context.Background() }
func (s *fakeBidiStream) Header() (metadata.MD, error) { return nil, nil }
func (s *fakeBidiStream) Trailer() metadata.MD         { return nil }
func (s *fakeBidiStream) SendMsg(_ any) error          { return nil }
func (s *fakeBidiStream) RecvMsg(_ any) error          { return nil }

// newTestSession builds a PortForwardSession bound to fakeBidiStream
// without going through gRPC. Returns the session, the stream, and a
// recv-channel the test can use to inject server messages.
func newTestSession(sendDelay time.Duration) (*PortForwardSession, *fakeBidiStream) {
	stream := &fakeBidiStream{
		sendDelay: sendDelay,
		recvCh:    make(chan *generated.PortForwardServerMessage, 4),
	}
	sess := &PortForwardSession{
		stream:  stream,
		logger:  log.NewLogger(),
		sendCh:  make(chan *generated.PortForwardClientMessage, 64),
		sendErr: make(chan error, 1),
		closed:  make(chan struct{}),
	}
	go sess.sendLoop()
	return sess, stream
}

// TestSession_CloseRaceWithInFlightEnqueue is the regression test for
// the panic we hit on the operator's laptop:
//
//	panic: send on closed channel
//	  PortForwardSession.enqueue
//	  PortForwardSession.SendClose / SendData / SendOpen
//	  Daemon.proxyConn (after sess.Close() ran in the reconnect path)
//
// The root cause was that Close() closed both s.closed AND s.sendCh,
// so a concurrent enqueue's `select` could pick the `sendCh <- msg`
// case and panic on a closed channel. The fix is to never close
// sendCh — only signal via s.closed.
//
// This test hammers SendData / SendClose / SendOpen from many
// goroutines while Close runs, and asserts the process doesn't
// panic. Without the fix it crashes within a few hundred iterations
// reliably.
func TestSession_CloseRaceWithInFlightEnqueue(t *testing.T) {
	for trial := 0; trial < 50; trial++ {
		sess, _ := newTestSession(0)

		const goroutines = 32
		var startWG, endWG sync.WaitGroup
		startWG.Add(1)
		for g := 0; g < goroutines; g++ {
			endWG.Add(1)
			go func(id uint64) {
				defer endWG.Done()
				startWG.Wait()
				for i := 0; i < 200; i++ {
					// Mix of the three Send* shapes — all funnel
					// through enqueue.
					_ = sess.SendOpen(id, 8080)
					_ = sess.SendData(id, []byte("ping"))
					_ = sess.SendClose(id, "done")
				}
			}(uint64(g))
		}
		startWG.Done()

		// Race Close into the workload.
		go func() {
			time.Sleep(time.Duration(trial%5) * time.Millisecond)
			_ = sess.Close()
		}()

		endWG.Wait()
		// If we get here without panicking, the regression is fixed.

		// After Close, every enqueue must return ErrClosedPipe.
		if err := sess.SendData(0, []byte("late")); !errors.Is(err, io.ErrClosedPipe) {
			t.Fatalf("trial %d: expected ErrClosedPipe after Close, got %v", trial, err)
		}
	}
}

// TestSession_CloseIdempotent ensures Close can be called any number
// of times without blowing up.
func TestSession_CloseIdempotent(t *testing.T) {
	sess, _ := newTestSession(0)
	for i := 0; i < 5; i++ {
		if err := sess.Close(); err != nil {
			t.Fatalf("Close #%d returned %v", i, err)
		}
	}
}

// TestSession_PostCloseEnqueueReturnsError ensures a single enqueue
// after Close cleanly returns ErrClosedPipe.
func TestSession_PostCloseEnqueueReturnsError(t *testing.T) {
	sess, _ := newTestSession(0)
	_ = sess.Close()
	if err := sess.SendData(1, []byte("x")); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("expected ErrClosedPipe, got %v", err)
	}
	if err := sess.SendClose(1, ""); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("expected ErrClosedPipe, got %v", err)
	}
	if err := sess.SendOpen(1, 80); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("expected ErrClosedPipe, got %v", err)
	}
}
