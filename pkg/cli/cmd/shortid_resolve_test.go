package cmd

import (
	"testing"
	"time"

	"github.com/runestack/rune/pkg/types"
)

// These cover the #167 fix: the CLI-side resolver (used by `rune exec` and
// `rune port-forward`) must accept the abbreviated 8-hex instance id that
// `rune get instances` prints — the same targets `rune logs` already resolved
// server-side — and must never silently pick one of several matches.

func inst(id, name string, status types.InstanceStatus) *types.Instance {
	return &types.Instance{ID: id, Name: name, Status: status}
}

func TestMatchInstanceByIDPrefix(t *testing.T) {
	// a47107da… and a47199bb… share the 6-hex prefix "a471"→"a4710"/"a4719";
	// they diverge at char 6, so "a47107" is unique and "a471" is too short.
	insts := []*types.Instance{
		inst("a47107da-1111-2222-3333-444444444444", "marketing-xa3s2", types.InstanceStatusRunning),
		inst("a47107bb-9999-8888-7777-666666666666", "marketing-zzzzz", types.InstanceStatusRunning),
		inst("138a0cda-5555-6666-7777-888888888888", "control-plane-ao3lt", types.InstanceStatusRunning),
	}

	t.Run("unique 8-hex prefix resolves", func(t *testing.T) {
		got, err := matchInstanceByIDPrefix(insts, "138a0cda", "tomoul")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got == nil || got.Name != "control-plane-ao3lt" {
			t.Fatalf("resolved wrong instance: %+v", got)
		}
	})

	t.Run("ambiguous prefix errors instead of guessing", func(t *testing.T) {
		// "a47107" matches both a47107da and a47107bb.
		got, err := matchInstanceByIDPrefix(insts, "a47107", "tomoul")
		if err == nil {
			t.Fatalf("expected ambiguity error, got instance %+v", got)
		}
	})

	t.Run("longer prefix disambiguates", func(t *testing.T) {
		got, err := matchInstanceByIDPrefix(insts, "a47107da", "tomoul")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got == nil || got.Name != "marketing-xa3s2" {
			t.Fatalf("resolved wrong instance: %+v", got)
		}
	})

	t.Run("non-hex and too-short targets are not prefix-matched", func(t *testing.T) {
		for _, target := range []string{"marketing-xa3s2", "a471", "a4710z", ""} {
			got, err := matchInstanceByIDPrefix(insts, target, "tomoul")
			if err != nil || got != nil {
				t.Errorf("target %q: expected no prefix match, got (%v, %v)", target, got, err)
			}
		}
	})

	t.Run("hex prefix matching nothing is a miss, not an error", func(t *testing.T) {
		got, err := matchInstanceByIDPrefix(insts, "ffffff", "tomoul")
		if err != nil || got != nil {
			t.Fatalf("expected clean miss, got (%v, %v)", got, err)
		}
	})
}

// A name shared by a live instance and a Failed tombstone must resolve to the
// LIVE record — the short-id leg must not disturb that precedence.
func TestMatchPrefixDoesNotOverrideNamePrecedence(t *testing.T) {
	failedAt := time.Now().Add(-time.Minute)
	tomb := inst("aaaaaaaa-1111-2222-3333-444444444444", "web-0", types.InstanceStatusFailed)
	tomb.FailedAt = &failedAt
	live := inst("bbbbbbbb-1111-2222-3333-444444444444", "web-0", types.InstanceStatusRunning)

	// Sanity: prefix matching is keyed on ID, so a NAME never reaches it.
	got, err := matchInstanceByIDPrefix([]*types.Instance{tomb, live}, "web-0", "default")
	if err != nil || got != nil {
		t.Fatalf("a name must not be prefix-matched: (%v, %v)", got, err)
	}
	// And each full-ish prefix still selects its own record.
	if g, _ := matchInstanceByIDPrefix([]*types.Instance{tomb, live}, "bbbbbbbb", "default"); g == nil || g.ID != live.ID {
		t.Fatalf("expected live record for its own prefix, got %+v", g)
	}
	if g, _ := matchInstanceByIDPrefix([]*types.Instance{tomb, live}, "aaaaaaaa", "default"); g == nil || g.ID != tomb.ID {
		t.Fatalf("expected tombstone for its own prefix, got %+v", g)
	}
}
