package client

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"

	"github.com/runestack/rune/pkg/api/generated"
	"github.com/runestack/rune/pkg/log"
)

// PortForwardClient provides access to the gRPC PortForwardService
// (RUNE-122). One PortForwardClient owns one streaming session; the
// CLI multiplexes any number of accepted local connections over it.
type PortForwardClient struct {
	client *Client
	logger log.Logger
	pf     generated.PortForwardServiceClient
}

// NewPortForwardClient creates a new port-forward client.
func NewPortForwardClient(client *Client) *PortForwardClient {
	return &PortForwardClient{
		client: client,
		logger: client.logger.WithComponent("portforward-client"),
		pf:     generated.NewPortForwardServiceClient(client.conn),
	}
}

// PortForwardTarget identifies the instance to bind a forward to.
// Exactly one of Service or InstanceID must be non-empty.
type PortForwardTarget struct {
	Service    string
	InstanceID string
	Namespace  string
	// Pin selects a specific instance when Service has scale>1.
	Pin string
	// Ports the client intends to forward (informational/audit).
	Ports []uint32
}

// PortForwardSession is a single open StreamPortForward call.
//
// The session holds the bidi gRPC stream and serialises Sends through
// an internal channel so callers can fan-out Open / Data frames from
// any goroutine. Recv is single-consumer: typically the CLI runs one
// receive loop that demultiplexes frames into per-conn_id channels.
type PortForwardSession struct {
	stream    generated.PortForwardService_StreamPortForwardClient
	logger    log.Logger
	sendCh    chan *generated.PortForwardClientMessage
	sendErr   chan error
	closed    chan struct{}
	closeOnce sync.Once
}

// PortForwardReady is returned by Ready() once the server has bound
// the stream to a concrete instance.
type PortForwardReady struct {
	InstanceID  string
	Namespace   string
	ServiceName string
}

// Open opens a session and waits for the Ready frame.
func (c *PortForwardClient) Open(ctx context.Context, target PortForwardTarget) (*PortForwardSession, *PortForwardReady, error) {
	if target.Service == "" && target.InstanceID == "" {
		return nil, nil, errors.New("PortForwardTarget requires Service or InstanceID")
	}
	if target.Service != "" && target.InstanceID != "" {
		return nil, nil, errors.New("PortForwardTarget cannot specify both Service and InstanceID")
	}

	stream, err := c.pf.StreamPortForward(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("open port-forward stream: %w", err)
	}

	init := &generated.PortForwardInit{
		Namespace:        target.Namespace,
		Ports:            target.Ports,
		InstanceSelector: target.Pin,
	}
	if target.Service != "" {
		init.Target = &generated.PortForwardInit_ServiceName{ServiceName: target.Service}
	} else {
		init.Target = &generated.PortForwardInit_InstanceId{InstanceId: target.InstanceID}
	}

	if err := stream.Send(&generated.PortForwardClientMessage{
		Message: &generated.PortForwardClientMessage_Init{Init: init},
	}); err != nil {
		return nil, nil, fmt.Errorf("send init: %w", err)
	}

	// First message back must be Ready (or Status on error).
	msg, err := stream.Recv()
	if err != nil {
		return nil, nil, fmt.Errorf("recv ready: %w", err)
	}
	switch m := msg.GetMessage().(type) {
	case *generated.PortForwardServerMessage_Ready:
		ready := &PortForwardReady{
			InstanceID:  m.Ready.GetInstanceId(),
			Namespace:   m.Ready.GetNamespace(),
			ServiceName: m.Ready.GetServiceName(),
		}
		sess := &PortForwardSession{
			stream:  stream,
			logger:  c.logger,
			sendCh:  make(chan *generated.PortForwardClientMessage, 64),
			sendErr: make(chan error, 1),
			closed:  make(chan struct{}),
		}
		go sess.sendLoop()
		return sess, ready, nil
	case *generated.PortForwardServerMessage_Status:
		return nil, nil, fmt.Errorf("port-forward init failed: %s", m.Status.GetMessage())
	default:
		return nil, nil, fmt.Errorf("port-forward: unexpected first frame from server")
	}
}

func (s *PortForwardSession) sendLoop() {
	defer close(s.sendErr)
	for msg := range s.sendCh {
		if err := s.stream.Send(msg); err != nil {
			s.sendErr <- err
			// Drain remaining so producers don't block.
			for range s.sendCh {
			}
			return
		}
	}
	// Half-close on graceful drain.
	_ = s.stream.CloseSend()
}

// SendOpen requests a new multiplexed connection.
func (s *PortForwardSession) SendOpen(connID uint64, remotePort uint32) error {
	return s.enqueue(&generated.PortForwardClientMessage{
		Message: &generated.PortForwardClientMessage_Open{
			Open: &generated.PortForwardOpen{ConnId: connID, RemotePort: remotePort},
		},
	})
}

// SendData forwards a payload for an open conn.
func (s *PortForwardSession) SendData(connID uint64, payload []byte) error {
	return s.enqueue(&generated.PortForwardClientMessage{
		Message: &generated.PortForwardClientMessage_Data{
			Data: &generated.PortForwardData{ConnId: connID, Payload: payload},
		},
	})
}

// SendClose terminates a single conn within the session.
func (s *PortForwardSession) SendClose(connID uint64, errStr string) error {
	return s.enqueue(&generated.PortForwardClientMessage{
		Message: &generated.PortForwardClientMessage_Close{
			Close: &generated.PortForwardClose{ConnId: connID, Error: errStr},
		},
	})
}

func (s *PortForwardSession) enqueue(msg *generated.PortForwardClientMessage) error {
	select {
	case <-s.closed:
		return io.ErrClosedPipe
	case s.sendCh <- msg:
		return nil
	}
}

// Recv reads the next frame from the server. Returns io.EOF on clean
// stream end.
func (s *PortForwardSession) Recv() (*generated.PortForwardServerMessage, error) {
	return s.stream.Recv()
}

// Close half-closes the stream and waits for the send loop to drain.
// Safe to call from any goroutine; idempotent.
func (s *PortForwardSession) Close() error {
	s.closeOnce.Do(func() {
		close(s.closed)
		close(s.sendCh)
		// Wait for sendLoop to drain (channel closes when loop exits).
		<-s.sendErr
	})
	return nil
}
