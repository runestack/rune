package service

import (
	"context"
	"testing"

	"github.com/runestack/rune/pkg/api/generated"
	"github.com/runestack/rune/pkg/log"
	"github.com/runestack/rune/pkg/store"
	"github.com/runestack/rune/pkg/store/repos"
	"github.com/runestack/rune/pkg/types"
)

// TestReleaseHistoryRoundTrip guards against the regression in #91: against the
// real BadgerStore, each version's Resource comes back from the JSON envelope as
// a map[string]interface{} (never a typed *types.Release), so a History handler
// that relied on a type assertion silently dropped every revision and returned
// an empty list with status OK. The fix is the JSON round-trip in
// historicalToRelease; this test exercises it through the real store, not a
// typed in-memory fake (which would store the typed value and never reproduce
// the bug).
func TestReleaseHistoryRoundTrip(t *testing.T) {
	ctx := context.Background()

	st := store.NewBadgerStore(log.NewTestLogger())
	if err := st.Open(t.TempDir()); err != nil {
		t.Fatalf("open badger: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	repo := repos.NewReleaseRepo(st)
	svc := &ReleaseService{repo: repo, logger: log.GetDefaultLogger()}

	const ns, name = "default", "atomic-demo"

	// Revision 1: create, then a sequence of updates that mirror the
	// deployed → failed → failed → deployed history from the issue. Each write
	// records a version row.
	rel := &types.Release{Name: name, Namespace: ns, Revision: 1, Status: types.ReleaseStatusDeployed}
	if err := repo.Create(ctx, rel); err != nil {
		t.Fatalf("create: %v", err)
	}
	for _, s := range []types.ReleaseStatus{
		types.ReleaseStatusFailed,
		types.ReleaseStatusFailed,
		types.ReleaseStatusDeployed,
	} {
		rel.Status = s
		if err := repo.Update(ctx, rel); err != nil {
			t.Fatalf("update -> %s: %v", s, err)
		}
	}

	resp, err := svc.History(ctx, &generated.HistoryRequest{Name: name, Namespace: ns})
	if err != nil {
		t.Fatalf("history: %v", err)
	}

	// The pre-fix bug: type assertion never matched the map[string]interface{}
	// the store returns, so this list was empty.
	if len(resp.Revisions) != 4 {
		t.Fatalf("expected 4 revisions, got %d", len(resp.Revisions))
	}

	// The round-trip must recover the typed fields, not just a non-nil slice.
	for _, rv := range resp.Revisions {
		if rv.Name != name {
			t.Errorf("revision name = %q, want %q", rv.Name, name)
		}
		if rv.Namespace != ns {
			t.Errorf("revision namespace = %q, want %q", rv.Namespace, ns)
		}
		if rv.Revision != 1 {
			t.Errorf("revision number = %d, want 1", rv.Revision)
		}
	}
}
