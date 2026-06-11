package clickhouse

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/runestack/rune/pkg/observe"
)

// Parser stages are not lowered to SQL yet: the adapter must reject them with
// ErrCapabilityUnsupported (and advertise Parsers=false) rather than silently
// ignoring the stages.
func TestExecute_RejectsParserStages(t *testing.T) {
	s, err := New(Config{DSN: "clickhouse://localhost:9000"})
	if err != nil {
		t.Fatal(err)
	}
	if s.Capabilities().Parsers {
		t.Fatal("clickhouse must advertise Parsers=false")
	}
	q, err := observe.ParseLogQL(`{service="api"} | logfmt | status >= 500`, time.Time{}, time.Time{}, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Execute(context.Background(), q); !errors.Is(err, observe.ErrCapabilityUnsupported) {
		t.Fatalf("want ErrCapabilityUnsupported, got %v", err)
	}
}
