package outbox

import (
	"testing"

	"github.com/runestack/rune/pkg/log"
)

func TestOutbox_PushDrain(t *testing.T) {
	o := New(8, log.GetDefaultLogger())
	for i := 0; i < 5; i++ {
		o.Push(Entry{Kind: KindLog, Source: "t", Message: "hi"})
	}
	if got := o.Len(); got != 5 {
		t.Fatalf("len=%d, want 5", got)
	}
	got := o.Drain(0)
	if len(got) != 5 {
		t.Fatalf("drain=%d, want 5", len(got))
	}
	if o.Len() != 0 {
		t.Fatal("outbox should be empty after drain")
	}
}

func TestOutbox_DropsOldestOnOverflow(t *testing.T) {
	o := New(3, log.GetDefaultLogger())
	for i := 0; i < 5; i++ {
		o.Push(Entry{Kind: KindLog, Source: "t", Message: string(rune('a' + i))})
	}
	if o.Len() != 3 {
		t.Fatalf("len=%d, want 3 (cap)", o.Len())
	}
	if o.Dropped() != 2 {
		t.Fatalf("dropped=%d, want 2", o.Dropped())
	}
	got := o.Drain(0)
	want := []string{"c", "d", "e"}
	for i, e := range got {
		if e.Message != want[i] {
			t.Errorf("entry[%d]=%q, want %q", i, e.Message, want[i])
		}
	}
}

func TestOutbox_DrainBatch(t *testing.T) {
	o := New(10, log.GetDefaultLogger())
	for i := 0; i < 5; i++ {
		o.Push(Entry{Kind: KindEvent, Source: "t"})
	}
	if got := o.Drain(2); len(got) != 2 {
		t.Fatalf("drain=%d, want 2", len(got))
	}
	if o.Len() != 3 {
		t.Fatalf("remaining=%d, want 3", o.Len())
	}
}

func TestOutbox_CloseDropsPushes(t *testing.T) {
	o := New(4, log.GetDefaultLogger())
	o.Push(Entry{Source: "a"})
	o.Close()
	o.Push(Entry{Source: "b"}) // dropped
	got := o.Drain(0)
	if len(got) != 1 || got[0].Source != "a" {
		t.Fatalf("got %+v, want only 'a'", got)
	}
}
