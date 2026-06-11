package alerting

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"text/template"
	"time"

	"github.com/runestack/rune/pkg/log"
	"github.com/runestack/rune/pkg/types"
)

// The notifier delivers alert transitions to channels. The universal channel
// is a templated webhook — URL + headers + an optional body template — which
// covers Slack, PagerDuty, Opsgenie, Discord, and HTTP email APIs (Resend,
// SendGrid, ...) with no provider-specific code. `type: slack` only changes
// the default body. ${secret:namespace/name/key} references in the URL and
// header values resolve at send time and are never persisted resolved.

const sendTimeout = 10 * time.Second

// secretRefRe matches ${secret:namespace/name/key} (namespace optional —
// two-part refs default to the "default" namespace).
var secretRefRe = regexp.MustCompile(`\$\{secret:([a-zA-Z0-9._/-]+)\}`)

// payload is the template data for channel bodies and the default JSON shape.
type payload struct {
	Rule        string  `json:"rule"`
	Description string  `json:"description,omitempty"`
	State       string  `json:"state"`     // firing | ok
	PrevState   string  `json:"prevState"` // what it transitioned from
	Value       float64 `json:"value"`
	Op          string  `json:"op"`
	Threshold   float64 `json:"threshold"`
	Window      string  `json:"window"`
	LogQL       string  `json:"logql"`
	Since       string  `json:"since"` // RFC3339
}

type notifier struct {
	http    *http.Client
	secrets SecretLookup
	logger  log.Logger
}

func newNotifier(secrets SecretLookup, logger log.Logger) *notifier {
	return &notifier{
		http:    &http.Client{Timeout: sendTimeout},
		secrets: secrets,
		logger:  logger,
	}
}

func (n *notifier) send(ctx context.Context, ch *types.Channel, rule *types.AlertRule, prev string, st Status) error {
	p := payload{
		Rule:        rule.Name,
		Description: rule.Description,
		State:       st.State,
		PrevState:   prev,
		Value:       st.Value,
		Op:          rule.Op,
		Threshold:   rule.Threshold,
		Window:      rule.Window.String(),
		LogQL:       rule.LogQL,
		Since:       st.Since.UTC().Format(time.RFC3339),
	}

	body, contentType, err := n.renderBody(ch, p)
	if err != nil {
		return fmt.Errorf("render body: %w", err)
	}
	url, err := n.interpolate(ctx, ch.URL)
	if err != nil {
		return fmt.Errorf("resolve url secrets: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", contentType)
	for k, v := range ch.Headers {
		rv, err := n.interpolate(ctx, v)
		if err != nil {
			return fmt.Errorf("resolve header %s secrets: %w", k, err)
		}
		req.Header.Set(k, rv)
	}

	resp, err := n.http.Do(req)
	if err != nil {
		return err
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<14))
		_ = resp.Body.Close()
	}()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return fmt.Errorf("channel %s returned %d: %s", ch.Name, resp.StatusCode, strings.TrimSpace(string(b)))
	}
	return nil
}

// renderBody produces the request payload: the channel's Body template when
// set, the Slack preset for type slack, or the default JSON payload.
func (n *notifier) renderBody(ch *types.Channel, p payload) ([]byte, string, error) {
	tmplText := ch.Body
	if tmplText == "" {
		if ch.Type == types.ChannelTypeSlack {
			tmplText = slackPreset
		} else {
			b, err := json.Marshal(p)
			return b, "application/json", err
		}
	}
	tmpl, err := template.New("body").Parse(tmplText)
	if err != nil {
		return nil, "", fmt.Errorf("parse template: %w", err)
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, p); err != nil {
		return nil, "", fmt.Errorf("execute template: %w", err)
	}
	return buf.Bytes(), "application/json", nil
}

// slackPreset is the incoming-webhook payload for type slack.
const slackPreset = `{"text":"{{if eq .State "firing"}}:rotating_light:{{else}}:white_check_mark:{{end}} *{{.Rule}}* is {{.State}} — count {{.Value}} {{.Op}} {{.Threshold}} over {{.Window}}"}`

// interpolate resolves ${secret:namespace/name/key} references. Two-part refs
// (name/key) default to the "default" namespace. Errors fail the send —
// better an undelivered notification than one leaking the raw reference.
func (n *notifier) interpolate(ctx context.Context, s string) (string, error) {
	matches := secretRefRe.FindAllStringSubmatchIndex(s, -1)
	if len(matches) == 0 {
		return s, nil
	}
	if n.secrets == nil {
		return "", fmt.Errorf("secret references present but no secret lookup configured")
	}
	var b strings.Builder
	last := 0
	for _, m := range matches {
		b.WriteString(s[last:m[0]])
		ref := s[m[2]:m[3]]
		parts := strings.Split(ref, "/")
		var ns, name, key string
		switch len(parts) {
		case 3:
			ns, name, key = parts[0], parts[1], parts[2]
		case 2:
			ns, name, key = "default", parts[0], parts[1]
		default:
			return "", fmt.Errorf("invalid secret reference %q (want namespace/name/key or name/key)", ref)
		}
		val, err := n.secrets(ctx, ns, name, key)
		if err != nil {
			return "", fmt.Errorf("resolve secret %s: %w", ref, err)
		}
		b.WriteString(val)
		last = m[1]
	}
	b.WriteString(s[last:])
	return b.String(), nil
}
