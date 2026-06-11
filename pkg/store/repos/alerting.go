package repos

import (
	"context"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/runestack/rune/pkg/store"
	"github.com/runestack/rune/pkg/types"
	"github.com/runestack/rune/pkg/utils"
)

// Alert rules and channels are cluster-scoped (one shared set): rows live
// under an empty namespace key, like SavedView and StorageClass.
const alertingNamespaceKey = ""

// AlertRuleRepo persists RuneSight alert rules.
type AlertRuleRepo struct {
	base *BaseRepo[types.AlertRule]
}

func NewAlertRuleRepo(core store.Store) *AlertRuleRepo {
	return &AlertRuleRepo{base: NewBaseRepo[types.AlertRule](core, types.ResourceTypeAlertRule)}
}

// List returns every rule, sorted by name for stable display.
func (r *AlertRuleRepo) List(ctx context.Context) ([]*types.AlertRule, error) {
	rules, err := r.base.List(ctx, alertingNamespaceKey)
	if err != nil {
		return nil, err
	}
	sort.Slice(rules, func(i, j int) bool { return rules[i].Name < rules[j].Name })
	return rules, nil
}

func (r *AlertRuleRepo) Get(ctx context.Context, name string) (*types.AlertRule, error) {
	return r.base.Get(ctx, alertingNamespaceKey, name)
}

// Save upserts a rule by name, preserving creation identity on update.
func (r *AlertRuleRepo) Save(ctx context.Context, rule *types.AlertRule) (*types.AlertRule, error) {
	if err := r.validate(rule); err != nil {
		return nil, err
	}
	now := time.Now()
	cur, err := r.base.Get(ctx, alertingNamespaceKey, rule.Name)
	if err == nil && cur != nil {
		rule.ID = cur.ID
		rule.CreatedAt = cur.CreatedAt
		if rule.CreatedBy == "" {
			rule.CreatedBy = cur.CreatedBy
		}
		rule.UpdatedAt = now
		if err := r.base.Update(ctx, alertingNamespaceKey, rule.Name, rule); err != nil {
			return nil, err
		}
		return rule, nil
	}
	if rule.ID == "" {
		rule.ID = uuid.NewString()
	}
	rule.CreatedAt = now
	rule.UpdatedAt = now
	if err := r.base.Create(ctx, alertingNamespaceKey, rule.Name, rule); err != nil {
		return nil, err
	}
	return rule, nil
}

func (r *AlertRuleRepo) Delete(ctx context.Context, name string) error {
	return r.base.Delete(ctx, alertingNamespaceKey, name)
}

func (r *AlertRuleRepo) validate(rule *types.AlertRule) error {
	if err := utils.ValidateDNS1123Name(rule.Name); err != nil {
		return fmt.Errorf("alert rule name validation failed: %w", err)
	}
	if strings.TrimSpace(rule.LogQL) == "" {
		return fmt.Errorf("alert rule %q: logql is required", rule.Name)
	}
	if rule.Window <= 0 {
		return fmt.Errorf("alert rule %q: window must be > 0", rule.Name)
	}
	switch rule.Op {
	case ">", ">=", "<", "<=", "==":
	default:
		return fmt.Errorf("alert rule %q: invalid op %q (want > >= < <= ==)", rule.Name, rule.Op)
	}
	if rule.For < 0 || rule.Interval < 0 {
		return fmt.Errorf("alert rule %q: for/interval must be >= 0", rule.Name)
	}
	return nil
}

// ChannelRepo persists notification channels.
type ChannelRepo struct {
	base *BaseRepo[types.Channel]
}

func NewChannelRepo(core store.Store) *ChannelRepo {
	return &ChannelRepo{base: NewBaseRepo[types.Channel](core, types.ResourceTypeChannel)}
}

// List returns every channel, sorted by name.
func (r *ChannelRepo) List(ctx context.Context) ([]*types.Channel, error) {
	chans, err := r.base.List(ctx, alertingNamespaceKey)
	if err != nil {
		return nil, err
	}
	sort.Slice(chans, func(i, j int) bool { return chans[i].Name < chans[j].Name })
	return chans, nil
}

func (r *ChannelRepo) Get(ctx context.Context, name string) (*types.Channel, error) {
	return r.base.Get(ctx, alertingNamespaceKey, name)
}

// Save upserts a channel by name.
func (r *ChannelRepo) Save(ctx context.Context, c *types.Channel) (*types.Channel, error) {
	if err := r.validate(c); err != nil {
		return nil, err
	}
	now := time.Now()
	cur, err := r.base.Get(ctx, alertingNamespaceKey, c.Name)
	if err == nil && cur != nil {
		c.ID = cur.ID
		c.CreatedAt = cur.CreatedAt
		c.UpdatedAt = now
		if err := r.base.Update(ctx, alertingNamespaceKey, c.Name, c); err != nil {
			return nil, err
		}
		return c, nil
	}
	if c.ID == "" {
		c.ID = uuid.NewString()
	}
	c.CreatedAt = now
	c.UpdatedAt = now
	if err := r.base.Create(ctx, alertingNamespaceKey, c.Name, c); err != nil {
		return nil, err
	}
	return c, nil
}

func (r *ChannelRepo) Delete(ctx context.Context, name string) error {
	return r.base.Delete(ctx, alertingNamespaceKey, name)
}

func (r *ChannelRepo) validate(c *types.Channel) error {
	if err := utils.ValidateDNS1123Name(c.Name); err != nil {
		return fmt.Errorf("channel name validation failed: %w", err)
	}
	switch c.Type {
	case types.ChannelTypeWebhook, types.ChannelTypeSlack:
	default:
		return fmt.Errorf("channel %q: invalid type %q (want webhook|slack)", c.Name, c.Type)
	}
	// URL may contain ${secret:...} refs; validate scheme on the raw string.
	u, err := url.Parse(c.URL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return fmt.Errorf("channel %q: url must be http(s)", c.Name)
	}
	return nil
}
