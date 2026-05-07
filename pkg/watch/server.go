// Package watch implements the gRPC WatchService that streams ordered
// events from the control plane's OrderedLog to subscribers.
//
// The server is a thin adapter: it delegates ordering, retention, and
// backfill semantics to the OrderedLog backend. Its only added
// responsibility is wire-protocol concerns: per-stream send buffer,
// SLOW_CONSUMER detection, COMPACTED status, and graceful shutdown.
package watch

import (
	"context"
	"errors"
	"sync"
	"time"

	pb "github.com/runestack/rune/pkg/api/generated"
	"github.com/runestack/rune/pkg/log"
	"github.com/runestack/rune/pkg/store/orderedlog"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Server implements pb.WatchServiceServer over an orderedlog.OrderedLog.
type Server struct {
	pb.UnimplementedWatchServiceServer

	olog orderedlog.OrderedLog
	log  log.Logger

	// SendTimeout is the per-event send deadline. If a send blocks
	// longer than this, the client is declared slow and the stream is
	// terminated with CODE_SLOW_CONSUMER.
	SendTimeout time.Duration

	// shutdown is closed by Close to signal in-flight streams to
	// finish.
	shutdownMu sync.Mutex
	shutdown   chan struct{}
	closed     bool
}

// NewServer constructs a Server backed by olog.
func NewServer(olog orderedlog.OrderedLog, logger log.Logger) *Server {
	if logger == nil {
		logger = log.GetDefaultLogger().WithComponent("watch")
	} else {
		logger = logger.WithComponent("watch")
	}
	return &Server{
		olog:        olog,
		log:         logger,
		SendTimeout: 5 * time.Second,
		shutdown:    make(chan struct{}),
	}
}

// Close signals all in-flight Subscribe streams to terminate with
// CODE_SHUTTING_DOWN. Idempotent.
func (s *Server) Close() {
	s.shutdownMu.Lock()
	defer s.shutdownMu.Unlock()
	if s.closed {
		return
	}
	s.closed = true
	close(s.shutdown)
}

// Subscribe streams ordered events from req.FromSeq forward.
func (s *Server) Subscribe(req *pb.SubscribeRequest, stream pb.WatchService_SubscribeServer) error {
	ctx := stream.Context()
	clientID := req.GetClientId()
	fromSeq := req.GetFromSeq()

	s.log.Info("watch subscribed",
		log.Str("client_id", clientID),
		log.F("from_seq", fromSeq),
	)

	events, err := s.olog.Watch(ctx, fromSeq)
	if err != nil {
		if errors.Is(err, orderedlog.ErrCompacted) {
			// Best-effort: tell the client to snapshot. We expose the
			// current min retained seq via a Snapshot to keep the
			// status useful.
			minSeq := s.lookupRetainedMin(ctx)
			_ = stream.Send(&pb.WatchEvent{
				Payload: &pb.WatchEvent_Status{Status: &pb.WatchStatus{
					Code:           pb.WatchStatus_CODE_COMPACTED,
					Message:        "from_seq is below retained window; snapshot and resume",
					RetainedMinSeq: minSeq,
				}},
			})
			return nil
		}
		if errors.Is(err, orderedlog.ErrClosed) {
			return status.Error(codes.Unavailable, "orderedlog closed")
		}
		return status.Errorf(codes.Internal, "watch: %v", err)
	}

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-s.shutdown:
			_ = stream.Send(&pb.WatchEvent{
				Payload: &pb.WatchEvent_Status{Status: &pb.WatchStatus{
					Code:    pb.WatchStatus_CODE_SHUTTING_DOWN,
					Message: "server shutting down; reconnect",
				}},
			})
			return nil
		case ev, ok := <-events:
			if !ok {
				// Channel closed by the backend (ctx cancel, orderedlog
				// close, or slow-consumer drop). If our ctx isn't
				// done, treat as slow consumer.
				if ctx.Err() != nil {
					return ctx.Err()
				}
				_ = stream.Send(&pb.WatchEvent{
					Payload: &pb.WatchEvent_Status{Status: &pb.WatchStatus{
						Code:    pb.WatchStatus_CODE_SLOW_CONSUMER,
						Message: "watcher dropped by backend (slow consumer)",
					}},
				})
				return status.Error(codes.ResourceExhausted, "slow consumer")
			}
			if err := s.sendEvent(ctx, stream, ev); err != nil {
				return err
			}
		}
	}
}

// sendEvent does a bounded-time send. We can't actually preempt
// stream.Send, but we can timeout via ctx; here we apply SendTimeout
// to the stream context indirectly by running Send in a goroutine and
// selecting on a timer. If the timer fires, we cancel the stream by
// returning an error: gRPC will tear down the connection.
func (s *Server) sendEvent(ctx context.Context, stream pb.WatchService_SubscribeServer, ev orderedlog.Event) error {
	wEv := toProtoEvent(ev)
	done := make(chan error, 1)
	go func() { done <- stream.Send(wEv) }()
	select {
	case err := <-done:
		if err != nil {
			return status.Errorf(codes.Unavailable, "send: %v", err)
		}
		return nil
	case <-time.After(s.SendTimeout):
		s.log.Warn("watch send timeout; declaring slow consumer",
			log.F("seq", ev.Seq), log.Duration("timeout", s.SendTimeout))
		return status.Error(codes.DeadlineExceeded, "send timeout (slow consumer)")
	case <-ctx.Done():
		return ctx.Err()
	}
}

// lookupRetainedMin tries to read the current retention floor by
// taking a snapshot. We don't expose this on the OrderedLog interface
// directly, so the simplest available signal is "the snapshot's read
// txn sees the current head"; lacking that, we return 0 and let the
// client snapshot regardless.
func (s *Server) lookupRetainedMin(ctx context.Context) uint64 {
	snap, _, err := s.olog.Snapshot(ctx)
	if err != nil {
		return 0
	}
	defer snap.Close()
	// Snapshot interface intentionally only exposes Close in RUNE-039.
	// The richer retained-min query lands when Snapshot is fleshed out
	// for the resync-from-snapshot flow in a later ticket. For now,
	// returning 0 is acceptable: the client will Snapshot anyway and
	// resume from snapshot.Seq.
	return 0
}

func toProtoEvent(ev orderedlog.Event) *pb.WatchEvent {
	muts := make([]*pb.Mutation, 0, len(ev.Mutations))
	for _, m := range ev.Mutations {
		muts = append(muts, &pb.Mutation{
			Kind:         pb.MutationKind(m.Kind),
			ResourceType: m.ResourceType,
			Namespace:    m.Namespace,
			Name:         m.Name,
			Payload:      m.Payload,
		})
	}
	return &pb.WatchEvent{
		Payload: &pb.WatchEvent_Event{Event: &pb.OrderedEvent{
			Seq:       ev.Seq,
			OpType:    ev.OpType,
			Mutations: muts,
		}},
	}
}
