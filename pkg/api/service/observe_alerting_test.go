package service

import (
	"context"
	"testing"
	"time"

	"github.com/runestack/rune/pkg/api/generated"
	"github.com/runestack/rune/pkg/log"
	"github.com/runestack/rune/pkg/observe/alerting"
	"github.com/runestack/rune/pkg/observe/embedded"
	"github.com/runestack/rune/pkg/store"
	"github.com/runestack/rune/pkg/store/repos"
)

func newAlertingService(t *testing.T) *ObserveService {
	t.Helper()
	st := embedded.New(embedded.Config{Retention: -1})
	t.Cleanup(func() { _ = st.Close() })
	ts := store.NewTestStore()
	rules := repos.NewAlertRuleRepo(ts)
	chans := repos.NewChannelRepo(ts)
	alerter := alerting.New(alerting.Options{
		Store: st, Rules: rules, Channels: chans,
		Now: func() time.Time { return time.Date(2026, 6, 11, 12, 0, 0, 0, time.UTC) },
	})
	svc := NewObserveService(st, nil, log.GetDefaultLogger())
	svc.SetAlerting(rules, chans, alerter)
	return svc
}

func TestObserveService_AlertRuleCRUDAndStatus(t *testing.T) {
	svc := newAlertingService(t)
	ctx := context.Background()

	// Metric-query LogQL is rejected (the alerter adds the aggregation itself).
	if _, err := svc.SaveAlertRule(ctx, &generated.SaveAlertRuleRequest{
		Rule: &generated.AlertRule{Name: "bad", Logql: `count_over_time({service="x"}[5m])`, Window: "5m", Op: ">"},
	}); err == nil {
		t.Fatal("want rejection of metric-query logql")
	}
	// Bad duration rejected.
	if _, err := svc.SaveAlertRule(ctx, &generated.SaveAlertRuleRequest{
		Rule: &generated.AlertRule{Name: "bad2", Logql: `{service="x"}`, Window: "five minutes", Op: ">"},
	}); err == nil {
		t.Fatal("want rejection of invalid window duration")
	}

	saved, err := svc.SaveAlertRule(ctx, &generated.SaveAlertRuleRequest{
		Rule: &generated.AlertRule{
			Name: "payment-errors", Logql: `{service="payments", level="error"}`,
			Window: "5m", Op: ">", Threshold: 10, For: "90s", Channels: []string{"hook"},
		},
	})
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	if saved.GetRule().GetWindow() != "5m0s" || saved.GetRule().GetFor() != "1m30s" {
		t.Fatalf("durations not round-tripped: %+v", saved.GetRule())
	}

	// Statuses appear after the alerter evaluates.
	svcAlerter(svc).Tick(ctx)
	list, err := svc.ListAlertRules(ctx, &generated.ListAlertRulesRequest{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list.GetRules()) != 1 || len(list.GetStatuses()) != 1 {
		t.Fatalf("want 1 rule + 1 status, got %d/%d", len(list.GetRules()), len(list.GetStatuses()))
	}
	if list.GetStatuses()[0].GetState() != "ok" {
		t.Fatalf("empty store should evaluate ok: %+v", list.GetStatuses()[0])
	}

	if _, err := svc.DeleteAlertRule(ctx, &generated.DeleteAlertRuleRequest{Name: "payment-errors"}); err != nil {
		t.Fatalf("delete: %v", err)
	}
}

func TestObserveService_ChannelCRUD(t *testing.T) {
	svc := newAlertingService(t)
	ctx := context.Background()

	if _, err := svc.SaveChannel(ctx, &generated.SaveChannelRequest{
		Channel: &generated.Channel{Name: "bad", Type: "smtp", Url: "https://x"},
	}); err == nil {
		t.Fatal("want type validation error")
	}

	saved, err := svc.SaveChannel(ctx, &generated.SaveChannelRequest{
		Channel: &generated.Channel{
			Name: "email-oncall", Type: "webhook", Url: "https://api.resend.com/emails",
			Headers: map[string]string{"Authorization": "Bearer ${secret:resend/api-key}"},
		},
	})
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	if saved.GetChannel().GetHeaders()["Authorization"] == "" {
		t.Fatal("headers lost in round-trip")
	}

	list, err := svc.ListChannels(ctx, &generated.ListChannelsRequest{})
	if err != nil || len(list.GetChannels()) != 1 {
		t.Fatalf("list: %v / %d", err, len(list.GetChannels()))
	}
	if _, err := svc.DeleteChannel(ctx, &generated.DeleteChannelRequest{Name: "email-oncall"}); err != nil {
		t.Fatalf("delete: %v", err)
	}
}

func TestObserveService_AlertingUnavailable(t *testing.T) {
	svc := NewObserveService(nil, nil, log.GetDefaultLogger())
	if _, err := svc.ListAlertRules(context.Background(), &generated.ListAlertRulesRequest{}); err == nil {
		t.Fatal("want FailedPrecondition without alerting wired")
	}
}

// svcAlerter exposes the alerter for test ticks.
func svcAlerter(s *ObserveService) *alerting.Alerter { return s.alerter }
