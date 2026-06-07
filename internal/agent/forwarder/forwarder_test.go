package forwarder

import (
	"context"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/runestack/rune/internal/agent/outbox"
	"github.com/runestack/rune/pkg/log"
	"github.com/runestack/rune/pkg/observe"
	"github.com/runestack/rune/pkg/types"
)

// fakeSource serves a fixed set of running instances and a canned log reader.
type fakeSource struct {
	instances []*types.Instance
	lines     map[string]string // instanceID -> newline-joined log body
}

func (f *fakeSource) ListRunningInstances(ctx context.Context, namespace string) ([]*types.Instance, error) {
	return f.instances, nil
}

func (f *fakeSource) GetInstanceLogs(ctx context.Context, namespace, instanceID string, opts types.LogOptions) (io.ReadCloser, error) {
	body := f.lines[instanceID]
	return io.NopCloser(strings.NewReader(body)), nil
}

// captureIngester records every batch it receives.
type captureIngester struct {
	mu      sync.Mutex
	records []observe.LogRecord
}

func (c *captureIngester) Ingest(ctx context.Context, records []observe.LogRecord) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.records = append(c.records, records...)
	return nil
}

func (c *captureIngester) all() []observe.LogRecord {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]observe.LogRecord, len(c.records))
	copy(out, c.records)
	return out
}

func TestForwarder_DualTap(t *testing.T) {
	src := &fakeSource{
		instances: []*types.Instance{
			{ID: "api-1", Name: "api-1", Namespace: "default", ServiceName: "api"},
		},
		lines: map[string]string{
			"api-1": "hello world\nerror: boom\n",
		},
	}
	ing := &captureIngester{}
	ob := outbox.New(16, log.GetDefaultLogger())
	ob.Push(outbox.Entry{Kind: outbox.KindEvent, Source: "dataplane", Message: "endpoint changed"})

	fw, err := New(Config{
		Source:        src,
		Ingester:      ing,
		Outbox:        ob,
		NodeID:        "node-1",
		FlushInterval: 20 * time.Millisecond,
		PollInterval:  20 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := fw.Start(context.Background()); err != nil {
		t.Fatal(err)
	}

	// Wait for the taps + flush loop to deliver both workload logs and the
	// outbox event.
	deadline := time.After(2 * time.Second)
	for {
		recs := ing.all()
		var haveWorkload, haveEvent bool
		for _, r := range recs {
			if r.Stream == "event" && r.Line == "endpoint changed" {
				haveEvent = true
			}
			if r.Instance == "api-1" && r.Service == "api" && r.Node == "node-1" {
				haveWorkload = true
			}
		}
		if haveWorkload && haveEvent {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("timed out waiting for dual-tap delivery; got %d records", len(recs))
		case <-time.After(20 * time.Millisecond):
		}
	}

	_ = fw.Stop(context.Background())

	// Verify enrichment + level classification on the workload error line.
	var sawError bool
	for _, r := range ing.all() {
		if r.Line == "error: boom" {
			sawError = true
			if r.Level != "error" || r.Stream != "stderr" {
				t.Errorf("error line not classified: level=%q stream=%q", r.Level, r.Stream)
			}
			if r.Node != "node-1" {
				t.Errorf("missing NodeID stamp: %q", r.Node)
			}
		}
	}
	if !sawError {
		t.Error("error workload line not forwarded")
	}
}

func TestForwarder_RequeueOnIngestFailure(t *testing.T) {
	// flushOnce should re-spool a batch when Ingest fails.
	s := NewMemSpool(0)
	s.Push(observe.LogRecord{Line: "a"})
	fw := &Subsystem{
		spool:  s,
		batchN: 10,
		log:    log.GetDefaultLogger(),
		cfg:    Config{Ingester: failIngester{}},
	}
	fw.flushOnce(context.Background())
	if s.Len() != 1 {
		t.Fatalf("want batch re-spooled (len 1), got %d", s.Len())
	}
}

type failIngester struct{}

func (failIngester) Ingest(ctx context.Context, records []observe.LogRecord) error {
	return io.ErrClosedPipe
}
