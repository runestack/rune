package session

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/runestack/rune/pkg/log"
	"github.com/runestack/rune/pkg/store"
	"github.com/runestack/rune/pkg/store/repos"
)

func newTestManager(t *testing.T) (*Manager, store.Store) {
	t.Helper()
	st := store.NewTestStore()
	if err := st.Open(""); err != nil {
		t.Fatalf("open store: %v", err)
	}
	return New(st, log.GetDefaultLogger()), st
}

// Concurrent refreshes presenting the SAME secret must converge on one
// successor and never revoke the grant (the multi-tab / retry race). This is
// the production behaviour the spike validated, now against the real manager.
func TestRotate_ConcurrentSameSecret(t *testing.T) {
	ctx := context.Background()
	m, st := newTestManager(t)
	_, secret, err := repos.NewTokenRepo(st).IssueRefreshGrant(ctx, "laptop", "alice", "user", time.Hour)
	if err != nil {
		t.Fatalf("issue grant: %v", err)
	}

	const N = 50
	var wg sync.WaitGroup
	outs := make([]Out, N)
	results := make([]Result, N)
	start := make(chan struct{})
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			outs[i], results[i] = m.Rotate(ctx, secret)
		}(i)
	}
	close(start)
	wg.Wait()

	successors := map[string]struct{}{}
	for i := 0; i < N; i++ {
		if results[i] != ResultOK {
			t.Fatalf("racer %d got non-OK result %d (grant likely revoked)", i, results[i])
		}
		successors[outs[i].Refresh] = struct{}{}
		if outs[i].Access == "" {
			t.Fatalf("racer %d got empty access token", i)
		}
	}
	if len(successors) != 1 {
		t.Fatalf("expected all racers to converge on 1 successor refresh token, got %d distinct", len(successors))
	}
}

// A genuinely stale reuse of a rotated secret, past the grace window, is theft:
// it must revoke the grant.
func TestRotate_StaleReusePastGraceRevokes(t *testing.T) {
	ctx := context.Background()
	m, st := newTestManager(t)
	m.GraceWindow = 5 * time.Millisecond

	grant, secret, err := repos.NewTokenRepo(st).IssueRefreshGrant(ctx, "laptop", "alice", "user", time.Hour)
	if err != nil {
		t.Fatalf("issue grant: %v", err)
	}

	if _, res := m.Rotate(ctx, secret); res != ResultOK {
		t.Fatalf("first rotation should succeed, got %d", res)
	}
	time.Sleep(10 * time.Millisecond) // let grace expire

	if _, res := m.Rotate(ctx, secret); res != ResultBreach {
		t.Fatalf("stale reuse past grace should breach, got %d", res)
	}
	got, err := repos.NewTokenRepo(st).Get(ctx, grant.ID)
	if err != nil {
		t.Fatalf("get grant: %v", err)
	}
	if !got.Revoked {
		t.Fatal("grant should be revoked after stale-reuse breach")
	}
}

// The minted access token is a real bearer; the rotated refresh token is not.
func TestRotate_AccessIsBearerRefreshIsNot(t *testing.T) {
	ctx := context.Background()
	m, st := newTestManager(t)
	repo := repos.NewTokenRepo(st)
	_, secret, err := repo.IssueRefreshGrant(ctx, "laptop", "alice", "user", time.Hour)
	if err != nil {
		t.Fatalf("issue grant: %v", err)
	}
	out, res := m.Rotate(ctx, secret)
	if res != ResultOK {
		t.Fatalf("rotate: %d", res)
	}
	if _, err := repo.FindRequestBearer(ctx, out.Access); err != nil {
		t.Fatalf("access token should be a valid bearer: %v", err)
	}
	if _, err := repo.FindRequestBearer(ctx, out.Refresh); err == nil {
		t.Fatal("SECURITY: rotated refresh token accepted as a bearer")
	}
}
