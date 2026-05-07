package watch

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	pb "github.com/runestack/rune/pkg/api/generated"
	"github.com/runestack/rune/pkg/log"
	"github.com/runestack/rune/pkg/store/orderedlog"
	"google.golang.org/grpc"
)

// EventHandler processes events delivered to the client. It MUST NOT
// block for an unbounded amount of time: a slow handler will cause
// the server to drop the stream as a slow consumer. Handlers should
// either persist quickly or hand off to a worker. Returning a non-nil
// error terminates the watch loop with that error.
type EventHandler func(ctx context.Context, ev orderedlog.Event) error

// SnapshotHandler is invoked when the server reports CODE_COMPACTED.
// The handler must take a fresh snapshot of state from the control
// plane (out-of-band) and return the seq it observed. The watch loop
// resumes from snapshotSeq + 1.
type SnapshotHandler func(ctx context.Context) (snapshotSeq uint64, err error)

// ClientOptions tunes the watch client.
type ClientOptions struct {
	// ClientID is reported to the server for logs / metrics.
	ClientID string

	// InitialBackoff / MaxBackoff bound the reconnect backoff after
	// transport errors. Defaults: 250ms / 30s.
	InitialBackoff time.Duration
	MaxBackoff     time.Duration

	Logger log.Logger
}

func (o *ClientOptions) defaults() {
	if o.InitialBackoff <= 0 {
		o.InitialBackoff = 250 * time.Millisecond
	}
	if o.MaxBackoff <= 0 {
		o.MaxBackoff = 30 * time.Second
	}
	if o.Logger == nil {
		o.Logger = log.GetDefaultLogger().WithComponent("watch.client")
	}
}

// Client is a resilient client for WatchService. It reconnects on
// transport errors with exponential backoff, advances fromSeq across
// reconnects so no event is delivered twice, and invokes the snapshot
// handler when the server reports COMPACTED.
type Client struct {
	conn      grpc.ClientConnInterface
	stub      pb.WatchServiceClient
	handler   EventHandler
	snapshot  SnapshotHandler
	opts      ClientOptions
	log       log.Logger
	lastSeq   uint64
}

// NewClient constructs a Client over an existing grpc.ClientConn.
// startSeq is the seq immediately preceding the first event the
// client wants (i.e. delivery starts at startSeq+1). Pass 0 to
// receive everything in retention.
func NewClient(conn grpc.ClientConnInterface, startSeq uint64, handler EventHandler, snapshot SnapshotHandler, opts ClientOptions) *Client {
	opts.defaults()
	return &Client{
		conn:     conn,
		stub:     pb.NewWatchServiceClient(conn),
		handler:  handler,
		snapshot: snapshot,
		opts:     opts,
		log:      opts.Logger,
		lastSeq:  startSeq,
	}
}

// Run blocks driving the watch loop until ctx is cancelled. It
// returns nil on clean shutdown (ctx cancellation) or a non-nil
// error if the handler or snapshot handler returns one.
func (c *Client) Run(ctx context.Context) error {
	backoff := c.opts.InitialBackoff
	for {
		if ctx.Err() != nil {
			return nil
		}
		err := c.runOnce(ctx)
		switch {
		case err == nil:
			// Server-initiated graceful close (e.g. SHUTTING_DOWN);
			// reconnect immediately.
			backoff = c.opts.InitialBackoff
			continue
		case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
			return nil
		case errors.Is(err, errCompacted):
			if c.snapshot == nil {
				return fmt.Errorf("watch: server reported COMPACTED but no SnapshotHandler set")
			}
			snapSeq, sErr := c.snapshot(ctx)
			if sErr != nil {
				return fmt.Errorf("watch: snapshot handler: %w", sErr)
			}
			c.lastSeq = snapSeq
			c.log.Info("watch resumed from snapshot", log.F("snapshot_seq", snapSeq))
			backoff = c.opts.InitialBackoff
			continue
		case errors.Is(err, errFatalHandler):
			return err
		default:
			c.log.Warn("watch transport error; reconnecting",
				log.Err(err), log.Duration("backoff", backoff))
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(backoff):
			}
			backoff *= 2
			if backoff > c.opts.MaxBackoff {
				backoff = c.opts.MaxBackoff
			}
		}
	}
}

// LastSeq returns the most recently observed seq. Safe to call after Run returns.
func (c *Client) LastSeq() uint64 { return c.lastSeq }

var (
	errCompacted    = errors.New("watch: server reported COMPACTED")
	errFatalHandler = errors.New("watch: handler returned fatal error")
)

func (c *Client) runOnce(ctx context.Context) error {
	stream, err := c.stub.Subscribe(ctx, &pb.SubscribeRequest{
		FromSeq:  c.lastSeq,
		ClientId: c.opts.ClientID,
	})
	if err != nil {
		return fmt.Errorf("subscribe: %w", err)
	}
	for {
		msg, err := stream.Recv()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		if st := msg.GetStatus(); st != nil {
			switch st.GetCode() {
			case pb.WatchStatus_CODE_COMPACTED:
				return errCompacted
			case pb.WatchStatus_CODE_SHUTTING_DOWN:
				c.log.Info("watch server shutting down; will reconnect")
				return nil
			case pb.WatchStatus_CODE_SLOW_CONSUMER:
				// Not really retryable from the client's POV, but
				// reconnect honestly: maybe load eased. The agent's
				// outbox will buffer in the meantime.
				return fmt.Errorf("watch: slow consumer drop: %s", st.GetMessage())
			default:
				return fmt.Errorf("watch: unknown status code %v: %s", st.GetCode(), st.GetMessage())
			}
		}
		ev := msg.GetEvent()
		if ev == nil {
			// Malformed event; ignore but keep reading.
			continue
		}
		if ev.GetSeq() <= c.lastSeq {
			// Duplicate or out-of-order; the server contract forbids
			// this, but stay defensive.
			c.log.Warn("watch dropped non-monotonic event",
				log.F("seq", ev.GetSeq()), log.F("last_seq", c.lastSeq))
			continue
		}
		if err := c.handler(ctx, fromProtoEvent(ev)); err != nil {
			return fmt.Errorf("%w: %v", errFatalHandler, err)
		}
		c.lastSeq = ev.GetSeq()
	}
}

func fromProtoEvent(p *pb.OrderedEvent) orderedlog.Event {
	muts := make([]orderedlog.Mutation, 0, len(p.GetMutations()))
	for _, m := range p.GetMutations() {
		muts = append(muts, orderedlog.Mutation{
			Kind:         orderedlog.MutationKind(m.GetKind()),
			ResourceType: m.GetResourceType(),
			Namespace:    m.GetNamespace(),
			Name:         m.GetName(),
			Payload:      m.GetPayload(),
		})
	}
	return orderedlog.Event{
		Seq:       p.GetSeq(),
		OpType:    p.GetOpType(),
		Mutations: muts,
	}
}
