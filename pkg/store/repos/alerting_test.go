package repos

import (
	"context"
	"testing"
	"time"

	"github.com/runestack/rune/pkg/store"
	"github.com/runestack/rune/pkg/types"
)

func TestAlertRuleRepo_CRUDAndValidation(t *testing.T) {
	r := NewAlertRuleRepo(store.NewTestStore())
	ctx := context.Background()

	rule := &types.AlertRule{
		Name: "payment-errors", LogQL: `{service="payments", level="error"}`,
		Window: 5 * time.Minute, Op: ">", Threshold: 10,
		For: 2 * time.Minute, Channels: []string{"sre-slack"},
	}
	saved, err := r.Save(ctx, rule)
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	if saved.ID == "" || saved.CreatedAt.IsZero() {
		t.Fatalf("identity not stamped: %+v", saved)
	}

	// Upsert preserves identity, updates fields.
	saved2, err := r.Save(ctx, &types.AlertRule{
		Name: "payment-errors", LogQL: `{service="payments"}`,
		Window: 10 * time.Minute, Op: ">=", Threshold: 5,
	})
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if saved2.ID != saved.ID || !saved2.CreatedAt.Equal(saved.CreatedAt) {
		t.Errorf("upsert lost identity")
	}

	rules, err := r.List(ctx)
	if err != nil || len(rules) != 1 {
		t.Fatalf("list: %v / %d", err, len(rules))
	}
	if err := r.Delete(ctx, "payment-errors"); err != nil {
		t.Fatalf("delete: %v", err)
	}

	// Validation.
	bad := []*types.AlertRule{
		{Name: "Bad Name", LogQL: "{}", Window: time.Minute, Op: ">"},
		{Name: "no-query", LogQL: " ", Window: time.Minute, Op: ">"},
		{Name: "no-window", LogQL: "{}", Op: ">"},
		{Name: "bad-op", LogQL: "{}", Window: time.Minute, Op: "!="},
	}
	for _, b := range bad {
		if _, err := r.Save(ctx, b); err == nil {
			t.Errorf("want validation error for %q", b.Name)
		}
	}
}

func TestChannelRepo_CRUDAndValidation(t *testing.T) {
	r := NewChannelRepo(store.NewTestStore())
	ctx := context.Background()

	c := &types.Channel{
		Name: "email-oncall", Type: types.ChannelTypeWebhook,
		URL:     "https://api.resend.com/emails",
		Headers: map[string]string{"Authorization": "Bearer ${secret:default/resend/api-key}"},
		Body:    `{"from":"alerts@x.io","to":["sre@x.io"],"subject":"[{{.State}}] {{.Rule}}"}`,
	}
	if _, err := r.Save(ctx, c); err != nil {
		t.Fatalf("save: %v", err)
	}
	got, err := r.Get(ctx, "email-oncall")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Headers["Authorization"] != "Bearer ${secret:default/resend/api-key}" {
		t.Errorf("headers not round-tripped: %+v", got.Headers)
	}

	// Validation.
	if _, err := r.Save(ctx, &types.Channel{Name: "bad-type", Type: "smtp", URL: "https://x"}); err == nil {
		t.Error("want type validation error")
	}
	if _, err := r.Save(ctx, &types.Channel{Name: "bad-url", Type: "webhook", URL: "ftp://x"}); err == nil {
		t.Error("want url validation error")
	}
}
