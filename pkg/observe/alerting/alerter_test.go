package alerting

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/runestack/rune/pkg/log"
	"github.com/runestack/rune/pkg/observe"
	"github.com/runestack/rune/pkg/observe/embedded"
	"github.com/runestack/rune/pkg/types"
)

// --- fakes ---

type fakeRules struct{ rules []*types.AlertRule }

func (f *fakeRules) List(ctx context.Context) ([]*types.AlertRule, error) { return f.rules, nil }

type fakeChannels struct{ chans map[string]*types.Channel }

func (f *fakeChannels) Get(ctx context.Context, name string) (*types.Channel, error) {
	if c, ok := f.chans[name]; ok {
		return c, nil
	}
	return nil, fmt.Errorf("channel %s not found", name)
}

type fakeEvents struct{ events []types.Event }

func (f *fakeEvents) Emit(ctx context.Context, e types.Event) error {
	f.events = append(f.events, e)
	return nil
}

// --- state machine ---

func TestNextState(t *testing.T) {
	base := time.Date(2026, 6, 11, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name string
		prev string
		cond bool
		held time.Duration // time since `since`
		forD time.Duration
		want string
	}{
		{"ok stays ok", types.AlertStateOK, false, 0, time.Minute, types.AlertStateOK},
		{"ok to pending with for", types.AlertStateOK, true, 0, time.Minute, types.AlertStatePending},
		{"ok fires immediately without for", types.AlertStateOK, true, 0, 0, types.AlertStateFiring},
		{"pending holds below for", types.AlertStatePending, true, 30 * time.Second, time.Minute, types.AlertStatePending},
		{"pending fires at for", types.AlertStatePending, true, time.Minute, time.Minute, types.AlertStateFiring},
		{"pending resolves", types.AlertStatePending, false, 30 * time.Second, time.Minute, types.AlertStateOK},
		{"firing stays firing", types.AlertStateFiring, true, time.Hour, time.Minute, types.AlertStateFiring},
		{"firing resolves", types.AlertStateFiring, false, time.Hour, time.Minute, types.AlertStateOK},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := nextState(c.prev, c.cond, base, c.forD, base.Add(c.held))
			if got != c.want {
				t.Fatalf("nextState(%s, cond=%v, held=%s, for=%s) = %s, want %s",
					c.prev, c.cond, c.held, c.forD, got, c.want)
			}
		})
	}
}

// --- end-to-end evaluation against the embedded store ---

func seedStore(t *testing.T, base time.Time, errorLines int) observe.LogStore {
	t.Helper()
	st := embedded.New(embedded.Config{Retention: -1})
	t.Cleanup(func() { _ = st.Close() })
	recs := make([]observe.LogRecord, 0, errorLines)
	for i := 0; i < errorLines; i++ {
		// Strictly before base: the query window's End is exclusive.
		recs = append(recs, observe.LogRecord{
			Timestamp: base.Add(-time.Duration(i+1) * time.Second),
			Service:   "payments", Level: "error", Line: "boom",
		})
	}
	if len(recs) > 0 {
		if err := st.Write(context.Background(), recs); err != nil {
			t.Fatal(err)
		}
	}
	return st
}

func TestAlerter_FiresAndResolves(t *testing.T) {
	now := time.Date(2026, 6, 11, 12, 0, 0, 0, time.UTC)
	clock := &now

	var delivered []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var m map[string]any
		_ = json.Unmarshal(b, &m)
		delivered = append(delivered, m)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	store := seedStore(t, now, 20) // 20 error lines in the window
	events := &fakeEvents{}
	rule := &types.AlertRule{
		Name: "payment-errors", LogQL: `{service="payments", level="error"}`,
		Window: 5 * time.Minute, Op: ">", Threshold: 10,
		For: 90 * time.Second, Interval: 60 * time.Second,
		Channels: []string{"hook"},
	}
	a := New(Options{
		Store: store,
		Rules: &fakeRules{rules: []*types.AlertRule{rule}},
		Channels: &fakeChannels{chans: map[string]*types.Channel{
			"hook": {Name: "hook", Type: types.ChannelTypeWebhook, URL: srv.URL},
		}},
		Events: events,
		Now:    func() time.Time { return *clock },
	})

	ctx := context.Background()

	// Eval 1: condition true, for=90s => pending. No channel delivery.
	a.Tick(ctx)
	if st := a.Statuses()[0]; st.State != types.AlertStatePending || st.Value != 20 {
		t.Fatalf("after eval1: %+v", st)
	}
	if len(delivered) != 0 {
		t.Fatalf("pending must not notify channels, got %d deliveries", len(delivered))
	}

	// Eval 2 (+2m): still true, held >= for => firing. Channel notified.
	*clock = now.Add(2 * time.Minute)
	a.Tick(ctx)
	if st := a.Statuses()[0]; st.State != types.AlertStateFiring {
		t.Fatalf("after eval2: %+v", st)
	}
	if len(delivered) != 1 || delivered[0]["state"] != "firing" || delivered[0]["rule"] != "payment-errors" {
		t.Fatalf("want 1 firing delivery, got %+v", delivered)
	}

	// Eval 3 (+4m): records have aged out of the 5m window? They were written
	// at ~now; at now+4m the 5m window still includes them. Move to +10m so
	// the window is empty => resolves. Channel notified with state ok.
	*clock = now.Add(10 * time.Minute)
	a.Tick(ctx)
	if st := a.Statuses()[0]; st.State != types.AlertStateOK {
		t.Fatalf("after eval3: %+v", st)
	}
	if len(delivered) != 2 || delivered[1]["state"] != "ok" || delivered[1]["prevState"] != "firing" {
		t.Fatalf("want resolved delivery, got %+v", delivered)
	}

	// Events were emitted for every transition (pending, firing, resolved).
	if len(events.events) != 3 {
		t.Fatalf("want 3 transition events, got %d: %+v", len(events.events), events.events)
	}
	if events.events[1].Reason != "AlertFiring" || events.events[1].Level != types.EventLevelWarn {
		t.Fatalf("firing event wrong: %+v", events.events[1])
	}
}

func TestAlerter_AbsenceAlert(t *testing.T) {
	now := time.Date(2026, 6, 11, 12, 0, 0, 0, time.UTC)
	store := seedStore(t, now, 0) // no logs at all
	a := New(Options{
		Store: store,
		Rules: &fakeRules{rules: []*types.AlertRule{{
			Name: "worker-absent", LogQL: `{service="worker"}`,
			Window: 5 * time.Minute, Op: "==", Threshold: 0,
		}}},
		Channels: &fakeChannels{chans: map[string]*types.Channel{}},
		Now:      func() time.Time { return now },
	})
	a.Tick(context.Background())
	if st := a.Statuses()[0]; st.State != types.AlertStateFiring || st.Value != 0 {
		t.Fatalf("absence alert should fire on zero count: %+v", st)
	}
}

func TestAlerter_EvalErrorPreservesState(t *testing.T) {
	now := time.Date(2026, 6, 11, 12, 0, 0, 0, time.UTC)
	store := seedStore(t, now, 5)
	rules := &fakeRules{rules: []*types.AlertRule{{
		Name: "r", LogQL: `{service="payments"}`, Window: time.Minute, Op: ">", Threshold: 1,
	}}}
	clock := &now
	a := New(Options{
		Store: store, Rules: rules,
		Channels: &fakeChannels{chans: map[string]*types.Channel{}},
		Now:      func() time.Time { return *clock },
	})
	ctx := context.Background()
	a.Tick(ctx)
	if st := a.Statuses()[0]; st.State != types.AlertStateFiring {
		t.Fatalf("setup: want firing, got %+v", st)
	}

	// Break the rule's query (simulates a backend/parse failure on reload):
	// state must NOT change, and the error must surface.
	rules.rules[0].LogQL = "not valid logql"
	*clock = clock.Add(2 * time.Minute)
	a.Tick(ctx)
	st := a.Statuses()[0]
	if st.State != types.AlertStateFiring {
		t.Fatalf("evaluation error must not change state, got %+v", st)
	}
	if st.LastError == "" {
		t.Fatal("evaluation error must surface on the status")
	}
}

func TestAlerter_DisabledAndDeletedRules(t *testing.T) {
	now := time.Date(2026, 6, 11, 12, 0, 0, 0, time.UTC)
	store := seedStore(t, now, 5)
	rules := &fakeRules{rules: []*types.AlertRule{
		{Name: "on", LogQL: `{service="payments"}`, Window: time.Minute, Op: ">", Threshold: 1},
		{Name: "off", LogQL: `{service="payments"}`, Window: time.Minute, Op: ">", Threshold: 1, Disabled: true},
	}}
	a := New(Options{
		Store: store, Rules: rules,
		Channels: &fakeChannels{chans: map[string]*types.Channel{}},
		Now:      func() time.Time { return now },
	})
	ctx := context.Background()
	a.Tick(ctx)
	sts := a.Statuses()
	if len(sts) != 1 || sts[0].Rule != "on" {
		t.Fatalf("disabled rules must not be tracked: %+v", sts)
	}

	// Deleting the rule drops its status on the next tick.
	rules.rules = rules.rules[:0]
	a.Tick(ctx)
	if sts := a.Statuses(); len(sts) != 0 {
		t.Fatalf("deleted rules must drop status: %+v", sts)
	}
}

// --- notifier ---

func TestNotifier_SecretInterpolationAndTemplate(t *testing.T) {
	var gotAuth, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	secrets := func(ctx context.Context, ns, name, key string) (string, error) {
		if ns == "default" && name == "resend" && key == "api-key" {
			return "rs_live_123", nil
		}
		return "", fmt.Errorf("unknown secret %s/%s/%s", ns, name, key)
	}
	n := newNotifier(secrets, testLogger())

	ch := &types.Channel{
		Name: "email-oncall", Type: types.ChannelTypeWebhook, URL: srv.URL,
		Headers: map[string]string{"Authorization": "Bearer ${secret:resend/api-key}"},
		Body:    `{"subject":"[{{.State}}] {{.Rule}}","text":"count {{.Value}} {{.Op}} {{.Threshold}}"}`,
	}
	rule := &types.AlertRule{Name: "payment-errors", Op: ">", Threshold: 10, Window: 5 * time.Minute}
	st := Status{Rule: "payment-errors", State: types.AlertStateFiring, Value: 17, Since: time.Now()}

	if err := n.send(context.Background(), ch, rule, types.AlertStatePending, st); err != nil {
		t.Fatalf("send: %v", err)
	}
	if gotAuth != "Bearer rs_live_123" {
		t.Errorf("secret not interpolated: %q", gotAuth)
	}
	if gotBody != `{"subject":"[firing] payment-errors","text":"count 17 > 10"}` {
		t.Errorf("template body wrong: %s", gotBody)
	}
}

func TestNotifier_SlackPresetAndFailures(t *testing.T) {
	var gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	n := newNotifier(nil, testLogger())
	rule := &types.AlertRule{Name: "r", Op: ">", Threshold: 5, Window: time.Minute}
	st := Status{State: types.AlertStateFiring, Value: 9, Since: time.Now()}

	slack := &types.Channel{Name: "sre", Type: types.ChannelTypeSlack, URL: srv.URL}
	if err := n.send(context.Background(), slack, rule, "pending", st); err != nil {
		t.Fatalf("slack send: %v", err)
	}
	var m map[string]string
	if err := json.Unmarshal([]byte(gotBody), &m); err != nil || m["text"] == "" {
		t.Fatalf("slack preset must produce {\"text\": ...}: %s", gotBody)
	}

	// Secret ref without a lookup fails the send (never leaks the raw ref).
	bad := &types.Channel{Name: "x", Type: types.ChannelTypeWebhook, URL: srv.URL,
		Headers: map[string]string{"Authorization": "${secret:a/b/c}"}}
	if err := n.send(context.Background(), bad, rule, "pending", st); err == nil {
		t.Fatal("want error when secret lookup is unavailable")
	}

	// Non-2xx surfaces as an error.
	srv500 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv500.Close()
	wh := &types.Channel{Name: "w", Type: types.ChannelTypeWebhook, URL: srv500.URL}
	if err := n.send(context.Background(), wh, rule, "pending", st); err == nil {
		t.Fatal("want error on 502 from channel endpoint")
	}
}

func testLogger() log.Logger { return log.GetDefaultLogger() }
