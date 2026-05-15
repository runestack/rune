package service

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"

	"github.com/runestack/rune/pkg/api/generated"
	"github.com/runestack/rune/pkg/log"
	"github.com/runestack/rune/pkg/orchestrator"
	"github.com/runestack/rune/pkg/types"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// PortForwardService implements the gRPC PortForwardService (RUNE-122).
//
// One gRPC stream per `rune port-forward` invocation. The client
// multiplexes any number of accepted local connections over the
// stream using a per-connection conn_id; the server holds one
// net.Conn per conn_id, fanning bytes through.
type PortForwardService struct {
	generated.UnimplementedPortForwardServiceServer

	logger       log.Logger
	orchestrator orchestrator.Orchestrator
}

// NewPortForwardService constructs a PortForwardService.
func NewPortForwardService(logger log.Logger, orch orchestrator.Orchestrator) *PortForwardService {
	return &PortForwardService{
		logger:       logger.WithComponent("portforward-service"),
		orchestrator: orch,
	}
}

// pfReadBufSize is the per-connection read buffer for the
// container→client direction. Tuned to a small page-aligned value;
// the gRPC stream itself does the framing.
const pfReadBufSize = 16 * 1024

// pfConn is the server-side state for one multiplexed connection.
type pfConn struct {
	id   uint64
	conn net.Conn
}

// StreamPortForward is the single bidi RPC implementing the service.
func (s *PortForwardService) StreamPortForward(stream generated.PortForwardService_StreamPortForwardServer) error {
	ctx, cancel := context.WithCancel(stream.Context())
	defer cancel()

	// 1. Init.
	initReq, err := s.receiveInit(stream)
	if err != nil {
		return err
	}

	// 2. Resolve target. The instance binding is fixed for the
	//    lifetime of the stream — every subsequent Open frame dials
	//    the same instance. This matches kubectl semantics and keeps
	//    the model simple.
	instance, err := s.resolveTarget(ctx, initReq)
	if err != nil {
		return err
	}

	// 3. Send Ready.
	if err := stream.Send(&generated.PortForwardServerMessage{
		Message: &generated.PortForwardServerMessage_Ready{
			Ready: &generated.PortForwardReady{
				InstanceId:  instance.ID,
				Namespace:   instance.Namespace,
				ServiceName: instance.ServiceName,
			},
		},
	}); err != nil {
		return err
	}

	// 4. Run the session.
	return s.runSession(ctx, stream, instance)
}

// receiveInit reads the first frame off the stream and validates it.
func (s *PortForwardService) receiveInit(stream generated.PortForwardService_StreamPortForwardServer) (*generated.PortForwardInit, error) {
	msg, err := stream.Recv()
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to receive init: %v", err)
	}
	init := msg.GetInit()
	if init == nil {
		return nil, status.Errorf(codes.InvalidArgument, "first message must be Init")
	}
	if init.GetServiceName() == "" && init.GetInstanceId() == "" {
		return nil, status.Errorf(codes.InvalidArgument, "Init must specify service_name or instance_id")
	}
	if init.GetServiceName() != "" && init.GetInstanceId() != "" {
		return nil, status.Errorf(codes.InvalidArgument, "Init must not specify both service_name and instance_id")
	}
	return init, nil
}

// resolveTarget binds the stream to a running instance. For a service
// target, picks the first Running instance. For an instance target,
// validates it exists and is running.
func (s *PortForwardService) resolveTarget(ctx context.Context, init *generated.PortForwardInit) (*types.Instance, error) {
	namespace := types.NS(init.Namespace)

	if iid := init.GetInstanceId(); iid != "" {
		// Direct instance binding. We only need metadata + running
		// state here — the real dial happens later, per Open frame.
		inst, err := s.orchestrator.GetInstanceByID(ctx, namespace, iid)
		if err != nil {
			if IsNotFound(err) {
				return nil, status.Errorf(codes.NotFound, "instance not found: %s", iid)
			}
			return nil, status.Errorf(codes.Internal, "failed to get instance: %v", err)
		}
		statusInfo, err := s.orchestrator.GetInstanceStatus(ctx, namespace, iid)
		if err == nil {
			inst.Status = statusInfo.Status
		}
		if inst.Status != types.InstanceStatusRunning {
			return nil, status.Errorf(codes.FailedPrecondition, "instance is not running, status: %s", inst.Status)
		}
		return inst, nil
	}

	// Service-based: enumerate running instances and pick.
	svc := init.GetServiceName()
	instances, err := s.orchestrator.ListRunningInstances(ctx, namespace)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to list instances: %v", err)
	}
	pin := init.GetInstanceSelector()
	for _, inst := range instances {
		if inst.ServiceName != svc {
			continue
		}
		if pin != "" && inst.ID != pin {
			continue
		}
		return inst, nil
	}
	if pin != "" {
		return nil, status.Errorf(codes.NotFound, "instance %s not found (or not running) for service %s/%s", pin, namespace, svc)
	}
	return nil, status.Errorf(codes.NotFound, "no running instances for service %s/%s", namespace, svc)
}

// runSession owns the per-stream connection table and goroutines.
func (s *PortForwardService) runSession(ctx context.Context, stream generated.PortForwardService_StreamPortForwardServer, instance *types.Instance) error {
	// Single goroutine serializes Send: gRPC stream.Send is not safe
	// for concurrent use, so we funnel all outbound messages through
	// a channel.
	out := make(chan *generated.PortForwardServerMessage, 64)
	var sendWG sync.WaitGroup
	sendWG.Add(1)
	go func() {
		defer sendWG.Done()
		for msg := range out {
			if err := stream.Send(msg); err != nil {
				// Once Send fails the stream is unrecoverable;
				// drain remaining messages so producers don't
				// block, but stop trying to Send.
				s.logger.Debug("port-forward stream send failed; draining", log.Err(err))
				for range out {
				}
				return
			}
		}
	}()

	var mu sync.Mutex
	conns := map[uint64]*pfConn{}
	var connsWG sync.WaitGroup

	// Helper to tear down everything cleanly when the stream ends.
	closeAll := func() {
		mu.Lock()
		for _, c := range conns {
			_ = c.conn.Close()
		}
		conns = map[uint64]*pfConn{}
		mu.Unlock()
	}

	// Receive loop.
	var sessionErr error
	for {
		msg, err := stream.Recv()
		if err != nil {
			if err == io.EOF {
				break
			}
			// Client cancellation / disconnect is benign.
			if errors.Is(ctx.Err(), context.Canceled) {
				break
			}
			sessionErr = err
			break
		}

		switch m := msg.GetMessage().(type) {
		case *generated.PortForwardClientMessage_Init:
			// Second Init mid-stream is a protocol error.
			s.sendStatus(out, codes.InvalidArgument, "unexpected Init mid-stream")

		case *generated.PortForwardClientMessage_Open:
			open := m.Open
			if open == nil {
				continue
			}
			s.handleOpen(ctx, instance, open, &mu, conns, &connsWG, out)

		case *generated.PortForwardClientMessage_Data:
			d := m.Data
			if d == nil {
				continue
			}
			mu.Lock()
			c, ok := conns[d.ConnId]
			mu.Unlock()
			if !ok {
				// Unknown conn_id — client error; tell them and move on.
				out <- closeMsg(d.ConnId, fmt.Sprintf("unknown conn_id %d", d.ConnId))
				continue
			}
			if _, err := c.conn.Write(d.Payload); err != nil {
				out <- closeMsg(d.ConnId, err.Error())
				mu.Lock()
				delete(conns, d.ConnId)
				mu.Unlock()
				_ = c.conn.Close()
			}

		case *generated.PortForwardClientMessage_Close:
			cl := m.Close
			if cl == nil {
				continue
			}
			mu.Lock()
			if c, ok := conns[cl.ConnId]; ok {
				delete(conns, cl.ConnId)
				_ = c.conn.Close()
			}
			mu.Unlock()

		default:
			// Unknown frame: log and continue.
			s.logger.Debug("port-forward: unknown frame", log.Str("instance", instance.ID))
		}
	}

	// Drain.
	closeAll()
	connsWG.Wait()
	close(out)
	sendWG.Wait()

	return sessionErr
}

// handleOpen dials the requested port and spawns a reader goroutine.
func (s *PortForwardService) handleOpen(
	ctx context.Context,
	instance *types.Instance,
	open *generated.PortForwardOpen,
	mu *sync.Mutex,
	conns map[uint64]*pfConn,
	wg *sync.WaitGroup,
	out chan<- *generated.PortForwardServerMessage,
) {
	if open.RemotePort == 0 || open.RemotePort > 65535 {
		out <- closeMsg(open.ConnId, fmt.Sprintf("invalid port: %d", open.RemotePort))
		return
	}

	mu.Lock()
	if _, exists := conns[open.ConnId]; exists {
		mu.Unlock()
		out <- closeMsg(open.ConnId, fmt.Sprintf("conn_id %d already open", open.ConnId))
		return
	}
	mu.Unlock()

	conn, _, err := s.orchestrator.DialInInstance(ctx, instance.Namespace, instance.ID, open.RemotePort)
	if err != nil {
		out <- closeMsg(open.ConnId, err.Error())
		return
	}

	pc := &pfConn{id: open.ConnId, conn: conn}
	mu.Lock()
	conns[open.ConnId] = pc
	mu.Unlock()

	wg.Add(1)
	go func() {
		defer wg.Done()
		defer func() {
			mu.Lock()
			delete(conns, open.ConnId)
			mu.Unlock()
			_ = conn.Close()
		}()

		buf := make([]byte, pfReadBufSize)
		for {
			n, err := conn.Read(buf)
			if n > 0 {
				payload := make([]byte, n)
				copy(payload, buf[:n])
				select {
				case out <- &generated.PortForwardServerMessage{
					Message: &generated.PortForwardServerMessage_Data{
						Data: &generated.PortForwardData{ConnId: open.ConnId, Payload: payload},
					},
				}:
				case <-ctx.Done():
					return
				}
			}
			if err != nil {
				closeErr := ""
				if err != io.EOF {
					closeErr = err.Error()
				}
				select {
				case out <- closeMsg(open.ConnId, closeErr):
				case <-ctx.Done():
				}
				return
			}
		}
	}()
}

// sendStatus emits a terminal session-level Status message (best-effort).
// gRPC codes are a small bounded enum (0..16) so the conversion to
// int32 is always safe.
func (s *PortForwardService) sendStatus(out chan<- *generated.PortForwardServerMessage, code codes.Code, msg string) {
	select {
	case out <- &generated.PortForwardServerMessage{
		Message: &generated.PortForwardServerMessage_Status{
			Status: &generated.Status{Code: int32(code), Message: msg}, //nolint:gosec // G115: codes.Code is a small bounded enum
		},
	}:
	default:
	}
}

func closeMsg(connID uint64, errStr string) *generated.PortForwardServerMessage {
	return &generated.PortForwardServerMessage{
		Message: &generated.PortForwardServerMessage_Close{
			Close: &generated.PortForwardClose{ConnId: connID, Error: errStr},
		},
	}
}
