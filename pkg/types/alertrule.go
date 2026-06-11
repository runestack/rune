package types

import "time"

// AlertRule is a RuneSight log-based alert: a LogQL log selector evaluated as
// a count over a rolling window, compared against a threshold. Rules are
// cluster-scoped, evaluated by the runed alerter loop, and fire through
// channels (see Channel). The Core-tier shape — count thresholds and absence
// (`== 0`) — works identically on every log backend, embedded included.
type AlertRule struct {
	// Unique identifier.
	ID string `json:"id" yaml:"id"`

	// DNS-1123 cluster-unique name.
	Name string `json:"name" yaml:"name"`

	// Description is optional free text shown in the rules table.
	Description string `json:"description,omitempty" yaml:"description,omitempty"`

	// LogQL is the log selector + line filters whose matching lines are
	// counted (e.g. `{service="payments", level="error"} |= "boom"`). It must
	// be a plain log query — the alerter adds the count_over_time aggregation
	// itself. Validated at save time.
	LogQL string `json:"logql" yaml:"logql"`

	// Window is the rolling count window (the `[5m]` part).
	Window time.Duration `json:"window" yaml:"window"`

	// Op compares the windowed count to Threshold: ">", ">=", "<", "<=",
	// "==". Absence/heartbeat alerts are `== 0` (or `< 1`).
	Op string `json:"op" yaml:"op"`

	// Threshold is the count compared against.
	Threshold float64 `json:"threshold" yaml:"threshold"`

	// For is how long the condition must hold before pending becomes firing
	// (the hysteresis that stops one noisy evaluation from paging). Zero
	// fires on the first true evaluation.
	For time.Duration `json:"for,omitempty" yaml:"for,omitempty"`

	// Interval is the evaluation cadence. Zero uses the alerter default (60s).
	Interval time.Duration `json:"interval,omitempty" yaml:"interval,omitempty"`

	// Channels are the Channel names notified on firing and resolved
	// transitions. Alert state changes always also emit a Rune event.
	Channels []string `json:"channels,omitempty" yaml:"channels,omitempty"`

	// Disabled pauses evaluation without deleting the rule. (Inverted so the
	// zero value means enabled.)
	Disabled bool `json:"disabled,omitempty" yaml:"disabled,omitempty"`

	// CreatedBy attributes the rule (auth principal name; informational).
	CreatedBy string `json:"createdBy,omitempty" yaml:"createdBy,omitempty"`

	CreatedAt time.Time `json:"createdAt" yaml:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt" yaml:"updatedAt"`
}

// Alert states (the evaluator's state machine).
const (
	AlertStateOK      = "ok"
	AlertStatePending = "pending"
	AlertStateFiring  = "firing"
)

// Channel is a named notification target referenced by AlertRule.Channels.
// One channel, many rules. The universal type is a templated webhook: URL +
// headers + an optional Go text/template body, which covers Slack, PagerDuty,
// Opsgenie, Discord, and HTTP email APIs (Resend, SendGrid, ...) with no
// provider-specific code. `type: slack` is a preset that defaults the body to
// Slack's incoming-webhook payload.
type Channel struct {
	// Unique identifier.
	ID string `json:"id" yaml:"id"`

	// DNS-1123 cluster-unique name (what rules reference).
	Name string `json:"name" yaml:"name"`

	// Type is "webhook" or "slack" (a webhook preset).
	Type string `json:"type" yaml:"type"`

	// URL is the HTTP(S) endpoint POSTed on alert transitions. May contain
	// ${secret:namespace/name/key} references resolved at send time.
	URL string `json:"url" yaml:"url"`

	// Headers are added to the request. Values may contain
	// ${secret:namespace/name/key} references (e.g. an Authorization bearer
	// token), resolved at send time — never stored resolved.
	Headers map[string]string `json:"headers,omitempty" yaml:"headers,omitempty"`

	// Body is an optional Go text/template for the request payload. Template
	// data: {{.Rule}} {{.State}} {{.PrevState}} {{.Value}} {{.Threshold}}
	// {{.Op}} {{.Window}} {{.LogQL}} {{.Since}} {{.Description}}. Empty uses
	// the default JSON payload (or the Slack preset for type slack).
	Body string `json:"body,omitempty" yaml:"body,omitempty"`

	CreatedAt time.Time `json:"createdAt" yaml:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt" yaml:"updatedAt"`
}

// Channel types.
const (
	ChannelTypeWebhook = "webhook"
	ChannelTypeSlack   = "slack"
)
