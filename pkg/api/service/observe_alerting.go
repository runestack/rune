package service

import (
	"context"
	"fmt"
	"time"

	"github.com/runestack/rune/pkg/api/generated"
	"github.com/runestack/rune/pkg/observe"
	"github.com/runestack/rune/pkg/observe/alerting"
	"github.com/runestack/rune/pkg/store/repos"
	"github.com/runestack/rune/pkg/types"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// SetAlerting wires the alerting backends: rule + channel persistence and the
// live alerter (for statuses). Any nil disables the corresponding RPCs with
// FailedPrecondition.
func (s *ObserveService) SetAlerting(rules *repos.AlertRuleRepo, channels *repos.ChannelRepo, alerter *alerting.Alerter) {
	s.alertRules = rules
	s.channels = channels
	s.alerter = alerter
}

// ListAlertRules returns every rule plus the alerter's live statuses.
func (s *ObserveService) ListAlertRules(ctx context.Context, _ *generated.ListAlertRulesRequest) (*generated.ListAlertRulesResponse, error) {
	if s.alertRules == nil {
		return nil, status.Error(codes.FailedPrecondition, "alerting is unavailable")
	}
	rules, err := s.alertRules.List(ctx)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "list alert rules: %v", err)
	}
	out := &generated.ListAlertRulesResponse{}
	for _, r := range rules {
		out.Rules = append(out.Rules, alertRuleToProto(r))
	}
	if s.alerter != nil {
		for _, st := range s.alerter.Statuses() {
			out.Statuses = append(out.Statuses, &generated.AlertStatus{
				Rule:      st.Rule,
				State:     st.State,
				Value:     st.Value,
				Since:     formatObserveTime(st.Since),
				LastEval:  formatObserveTime(st.LastEval),
				LastError: st.LastError,
			})
		}
	}
	return out, nil
}

// SaveAlertRule upserts a rule by name. The LogQL is parsed (and must be a
// plain log query — the alerter adds the aggregation) before persisting.
func (s *ObserveService) SaveAlertRule(ctx context.Context, req *generated.SaveAlertRuleRequest) (*generated.SaveAlertRuleResponse, error) {
	if s.alertRules == nil {
		return nil, status.Error(codes.FailedPrecondition, "alerting is unavailable")
	}
	pr := req.GetRule()
	if pr == nil {
		return nil, status.Error(codes.InvalidArgument, "rule is required")
	}
	q, err := observe.ParseLogQL(pr.GetLogql(), time.Time{}, time.Time{}, 0, false)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid logql: %v", err)
	}
	if q.IsMetricQuery() {
		return nil, status.Error(codes.InvalidArgument, "rule logql must be a plain log query (the alerter adds count_over_time)")
	}
	rule, err := protoToAlertRule(pr)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "%v", err)
	}
	saved, err := s.alertRules.Save(ctx, rule)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "save alert rule: %v", err)
	}
	return &generated.SaveAlertRuleResponse{
		Rule:   alertRuleToProto(saved),
		Status: &generated.Status{Code: int32(codes.OK)},
	}, nil
}

// DeleteAlertRule removes a rule by name.
func (s *ObserveService) DeleteAlertRule(ctx context.Context, req *generated.DeleteAlertRuleRequest) (*generated.DeleteAlertRuleResponse, error) {
	if s.alertRules == nil {
		return nil, status.Error(codes.FailedPrecondition, "alerting is unavailable")
	}
	if req.GetName() == "" {
		return nil, status.Error(codes.InvalidArgument, "name is required")
	}
	if err := s.alertRules.Delete(ctx, req.GetName()); err != nil {
		return nil, status.Errorf(codes.NotFound, "delete alert rule: %v", err)
	}
	return &generated.DeleteAlertRuleResponse{Status: &generated.Status{Code: int32(codes.OK)}}, nil
}

// ListChannels returns every notification channel.
func (s *ObserveService) ListChannels(ctx context.Context, _ *generated.ListChannelsRequest) (*generated.ListChannelsResponse, error) {
	if s.channels == nil {
		return nil, status.Error(codes.FailedPrecondition, "alerting is unavailable")
	}
	chans, err := s.channels.List(ctx)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "list channels: %v", err)
	}
	out := &generated.ListChannelsResponse{}
	for _, c := range chans {
		out.Channels = append(out.Channels, channelToProto(c))
	}
	return out, nil
}

// SaveChannel upserts a channel by name.
func (s *ObserveService) SaveChannel(ctx context.Context, req *generated.SaveChannelRequest) (*generated.SaveChannelResponse, error) {
	if s.channels == nil {
		return nil, status.Error(codes.FailedPrecondition, "alerting is unavailable")
	}
	pc := req.GetChannel()
	if pc == nil {
		return nil, status.Error(codes.InvalidArgument, "channel is required")
	}
	saved, err := s.channels.Save(ctx, &types.Channel{
		Name:    pc.GetName(),
		Type:    pc.GetType(),
		URL:     pc.GetUrl(),
		Headers: pc.GetHeaders(),
		Body:    pc.GetBody(),
	})
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "save channel: %v", err)
	}
	return &generated.SaveChannelResponse{
		Channel: channelToProto(saved),
		Status:  &generated.Status{Code: int32(codes.OK)},
	}, nil
}

// DeleteChannel removes a channel by name.
func (s *ObserveService) DeleteChannel(ctx context.Context, req *generated.DeleteChannelRequest) (*generated.DeleteChannelResponse, error) {
	if s.channels == nil {
		return nil, status.Error(codes.FailedPrecondition, "alerting is unavailable")
	}
	if req.GetName() == "" {
		return nil, status.Error(codes.InvalidArgument, "name is required")
	}
	if err := s.channels.Delete(ctx, req.GetName()); err != nil {
		return nil, status.Errorf(codes.NotFound, "delete channel: %v", err)
	}
	return &generated.DeleteChannelResponse{Status: &generated.Status{Code: int32(codes.OK)}}, nil
}

// --- conversions ---

func alertRuleToProto(r *types.AlertRule) *generated.AlertRule {
	return &generated.AlertRule{
		Id:          r.ID,
		Name:        r.Name,
		Description: r.Description,
		Logql:       r.LogQL,
		Window:      r.Window.String(),
		Op:          r.Op,
		Threshold:   r.Threshold,
		For:         durStr(r.For),
		Interval:    durStr(r.Interval),
		Channels:    r.Channels,
		Disabled:    r.Disabled,
		CreatedBy:   r.CreatedBy,
		CreatedAt:   formatObserveTime(r.CreatedAt),
		UpdatedAt:   formatObserveTime(r.UpdatedAt),
	}
}

func protoToAlertRule(pr *generated.AlertRule) (*types.AlertRule, error) {
	window, err := parseDur("window", pr.GetWindow(), true)
	if err != nil {
		return nil, err
	}
	forDur, err := parseDur("for", pr.GetFor(), false)
	if err != nil {
		return nil, err
	}
	interval, err := parseDur("interval", pr.GetInterval(), false)
	if err != nil {
		return nil, err
	}
	return &types.AlertRule{
		Name:        pr.GetName(),
		Description: pr.GetDescription(),
		LogQL:       pr.GetLogql(),
		Window:      window,
		Op:          pr.GetOp(),
		Threshold:   pr.GetThreshold(),
		For:         forDur,
		Interval:    interval,
		Channels:    pr.GetChannels(),
		Disabled:    pr.GetDisabled(),
		CreatedBy:   pr.GetCreatedBy(),
	}, nil
}

func channelToProto(c *types.Channel) *generated.Channel {
	return &generated.Channel{
		Id:        c.ID,
		Name:      c.Name,
		Type:      c.Type,
		Url:       c.URL,
		Headers:   c.Headers,
		Body:      c.Body,
		CreatedAt: formatObserveTime(c.CreatedAt),
		UpdatedAt: formatObserveTime(c.UpdatedAt),
	}
}

func durStr(d time.Duration) string {
	if d == 0 {
		return ""
	}
	return d.String()
}

func parseDur(field, s string, required bool) (time.Duration, error) {
	if s == "" {
		if required {
			return 0, fmt.Errorf("%s is required", field)
		}
		return 0, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("invalid %s %q: %v", field, s, err)
	}
	return d, nil
}
