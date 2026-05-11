package watch

import (
	"context"
	"encoding/binary"
	"errors"
	"net"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dgraph-io/badger/v4"
	pb "github.com/runestack/rune/pkg/api/generated"
	"github.com/runestack/rune/pkg/log"
	"github.com/runestack/rune/pkg/store/orderedlog"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"
)

// --- helpers ---------------------------------------------------------

const opPut = "test.put"

type putOp struct {
	Key, Val []byte
}

func (p *putOp) OpType() string { return opPut }
func (p *putOp) Marshal() ([]byte, error) {
	out := make([]byte, 4+len(p.Key)+len(p.Val))
	binary.BigEndian.PutUint16(out[0:2], uint16(len(p.Key)))
	binary.BigEndian.PutUint16(out[2:4], uint16(len(p.Val)))
	copy(out[4:], p.Key)
	copy(out[4+len(p.Key):], p.Val)
	return out, nil
}
func unmarshalPut(b []byte) (orderedlog.Op, error) {
	if len(b) < 4 {
		return nil, errors.New("short")
	}
	kl := int(binary.BigEndian.Uint16(b[0:2]))
	vl := int(binary.BigEndian.Uint16(b[2:4]))
	if 4+kl+vl > len(b) {
		return nil, errors.New("short body")
	}
	return &putOp{Key: b[4 : 4+kl], Val: b[4+kl : 4+kl+vl]}, nil
}
func putApplier(tx orderedlog.Txn, op orderedlog.Op) ([]orderedlog.Mutation, error) {
	p := op.(*putOp)
	if err := tx.Set(append([]byte("kv/"), p.Key...), p.Val); err != nil {
		return nil, err
	}
	return []orderedlog.Mutation{{
		Kind: orderedlog.MutationPut, ResourceType: "kv",
		Name: string(p.Key), Payload: p.Val,
	}}, nil
}

func newOlog(t *testing.T) *orderedlog.BadgerBackend {
	t.Helper()
	dir := t.TempDir()
	db, err := badger.Open(badger.DefaultOptions(filepath.Join(dir, "olog")).WithLogger(nil))
	if err != nil {
		t.Fatalf("badger: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	be := orderedlog.NewBadgerBackend(db, orderedlog.BackendOptions{
		Logger: log.GetDefaultLogger().WithComponent("test.olog"),
	})
	if err := be.Open(); err != nil {
		t.Fatalf("olog open: %v", err)
	}
	t.Cleanup(func() { _ = be.Close() })
	if err := be.Register(opPut, putApplier, unmarshalPut); err != nil {
		t.Fatalf("register: %v", err)
	}
	return be
}

func newServer(t *testing.T, be orderedlog.OrderedLog) (*grpc.ClientConn, *Server) {
	t.Helper()
	lis := bufconn.Listen(1 << 16)
	ws := NewServer(be, log.GetDefaultLogger())
	gs := grpc.NewServer()
	pb.RegisterWatchServiceServer(gs, ws)
	go func() { _ = gs.Serve(lis) }()
	t.Cleanup(func() { gs.Stop() })

	conn, err := grpc.NewClient(
		"passthrough:///bufnet",
		grpc.WithContextDialer(func(_ context.Context, _ string) (net.Conn, error) {
			return lis.DialContext(context.Background())
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn, ws
}

// --- tests -----------------------------------------------------------

func TestSubscribe_DeliversBackfillThenLive(t *testing.T) {
	be := newOlog(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Pre-populate 3 events.
	for i := 0; i < 3; i++ {
		if _, err := be.Propose(ctx, &putOp{Key: []byte{byte('a' + i)}, Val: []byte{byte(i)}}); err != nil {
			t.Fatalf("propose %d: %v", i, err)
		}
	}

	conn, _ := newServer(t, be)
	stub := pb.NewWatchServiceClient(conn)
	stream, err := stub.Subscribe(ctx, &pb.SubscribeRequest{FromSeq: 0, ClientId: "test"})
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}

	// Receive backfill (3 events).
	got := make([]uint64, 0, 6)
	for len(got) < 3 {
		msg, err := stream.Recv()
		if err != nil {
			t.Fatalf("recv backfill: %v", err)
		}
		ev := msg.GetEvent()
		if ev == nil {
			t.Fatalf("expected event, got status: %+v", msg.GetStatus())
		}
		got = append(got, ev.GetSeq())
	}
	want := []uint64{1, 2, 3}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("backfill seq[%d]=%d, want %d", i, got[i], want[i])
		}
	}

	// Now propose more, expect them live.
	for i := 0; i < 2; i++ {
		if _, err := be.Propose(ctx, &putOp{Key: []byte{byte('x' + i)}, Val: []byte{1}}); err != nil {
			t.Fatalf("propose live %d: %v", i, err)
		}
	}
	for len(got) < 5 {
		msg, err := stream.Recv()
		if err != nil {
			t.Fatalf("recv live: %v", err)
		}
		ev := msg.GetEvent()
		if ev == nil {
			t.Fatalf("expected event, got status: %+v", msg.GetStatus())
		}
		got = append(got, ev.GetSeq())
	}
	wantLive := []uint64{1, 2, 3, 4, 5}
	for i, w := range wantLive {
		if got[i] != w {
			t.Fatalf("seq[%d]=%d, want %d", i, got[i], w)
		}
	}
}

func TestSubscribe_FromSpecificSeq(t *testing.T) {
	be := newOlog(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for i := 0; i < 5; i++ {
		_, _ = be.Propose(ctx, &putOp{Key: []byte{byte('a' + i)}, Val: []byte{1}})
	}
	conn, _ := newServer(t, be)
	stub := pb.NewWatchServiceClient(conn)
	stream, err := stub.Subscribe(ctx, &pb.SubscribeRequest{FromSeq: 2})
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	want := []uint64{3, 4, 5}
	for _, w := range want {
		msg, err := stream.Recv()
		if err != nil {
			t.Fatalf("recv: %v", err)
		}
		if got := msg.GetEvent().GetSeq(); got != w {
			t.Fatalf("seq=%d, want %d", got, w)
		}
	}
}

func TestSubscribe_ShutdownEmitsStatus(t *testing.T) {
	be := newOlog(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	conn, ws := newServer(t, be)
	stub := pb.NewWatchServiceClient(conn)
	stream, err := stub.Subscribe(ctx, &pb.SubscribeRequest{FromSeq: 0})
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}

	// Close server; expect a SHUTTING_DOWN status then EOF.
	go func() {
		time.Sleep(50 * time.Millisecond)
		ws.Close()
	}()
	for {
		msg, err := stream.Recv()
		if err != nil {
			break // server-side return causes EOF
		}
		if st := msg.GetStatus(); st != nil {
			if st.GetCode() != pb.WatchStatus_CODE_SHUTTING_DOWN {
				t.Fatalf("status code=%v, want SHUTTING_DOWN", st.GetCode())
			}
			return
		}
	}
	t.Fatal("did not receive SHUTTING_DOWN before EOF")
}

func TestSubscribe_CompactedReportsStatus(t *testing.T) {
	dir := t.TempDir()
	db, err := badger.Open(badger.DefaultOptions(filepath.Join(dir, "olog")).WithLogger(nil))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	// Tiny retention so we can prune fast.
	be := orderedlog.NewBadgerBackend(db, orderedlog.BackendOptions{
		Logger:             log.GetDefaultLogger().WithComponent("test.olog"),
		RetentionMaxEvents: 2,
		RetentionInterval:  20 * time.Millisecond,
	})
	if err := be.Open(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = be.Close() })
	if err := be.Register(opPut, putApplier, unmarshalPut); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for i := 0; i < 6; i++ {
		_, _ = be.Propose(ctx, &putOp{Key: []byte{byte('a' + i)}, Val: []byte{1}})
	}
	// Wait for retention pass to evict early seqs.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		ch, err := be.Watch(ctx, 0)
		if err == nil {
			// drain & close so we don't leak the sub.
			subCtx, subCancel := context.WithCancel(ctx)
			subCancel()
			_ = ch
			_ = subCtx
			time.Sleep(50 * time.Millisecond)
			continue
		}
		if errors.Is(err, orderedlog.ErrCompacted) {
			break
		}
	}

	conn, _ := newServer(t, be)
	stub := pb.NewWatchServiceClient(conn)
	stream, err := stub.Subscribe(ctx, &pb.SubscribeRequest{FromSeq: 0})
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	msg, err := stream.Recv()
	if err != nil {
		t.Fatalf("recv: %v", err)
	}
	st := msg.GetStatus()
	if st == nil || st.GetCode() != pb.WatchStatus_CODE_COMPACTED {
		t.Fatalf("expected COMPACTED status, got %+v", msg)
	}
	// Stream should EOF after the status.
	_, err = stream.Recv()
	if err == nil {
		t.Fatal("expected EOF after COMPACTED")
	}
}

func TestClient_DeliversAndRecordsLastSeq(t *testing.T) {
	be := newOlog(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	conn, _ := newServer(t, be)

	var (
		mu      sync.Mutex
		got     []uint64
		gotN    atomic.Int32
		expectN = int32(5)
	)
	done := make(chan struct{})
	handler := func(ctx context.Context, ev orderedlog.Event) error {
		mu.Lock()
		got = append(got, ev.Seq)
		mu.Unlock()
		if gotN.Add(1) == expectN {
			close(done)
		}
		return nil
	}
	cli := NewClient(conn, 0, handler, nil, ClientOptions{ClientID: "t"})

	runErr := make(chan error, 1)
	go func() { runErr <- cli.Run(ctx) }()

	// Propose 5 events.
	for i := 0; i < 5; i++ {
		_, err := be.Propose(ctx, &putOp{Key: []byte{byte('a' + i)}, Val: []byte{1}})
		if err != nil {
			t.Fatalf("propose: %v", err)
		}
	}

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatalf("client only got %d events", gotN.Load())
	}

	cancel()
	if err := <-runErr; err != nil {
		t.Fatalf("client.Run: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	for i, s := range got {
		if s != uint64(i+1) {
			t.Fatalf("got[%d]=%d, want %d", i, s, i+1)
		}
	}
	if cli.LastSeq() != 5 {
		t.Fatalf("LastSeq=%d, want 5", cli.LastSeq())
	}
}

func TestClient_FatalHandlerErrorTerminates(t *testing.T) {
	be := newOlog(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	conn, _ := newServer(t, be)
	handler := func(ctx context.Context, ev orderedlog.Event) error {
		return errors.New("boom")
	}
	cli := NewClient(conn, 0, handler, nil, ClientOptions{})

	runErr := make(chan error, 1)
	go func() { runErr <- cli.Run(ctx) }()

	if _, err := be.Propose(ctx, &putOp{Key: []byte{'a'}, Val: []byte{1}}); err != nil {
		t.Fatal(err)
	}

	select {
	case err := <-runErr:
		if err == nil {
			t.Fatal("expected error from Run")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return")
	}
}
