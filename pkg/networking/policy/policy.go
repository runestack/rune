// Package policy implements compilation and evaluation of
// ServiceNetworkPolicy for the per-node agent (RUNE-064).
//
// The agent embeds an evaluator inside the userspace proxy. When a
// connection lands on a service VIP listener the evaluator answers
// "is this peer allowed to talk to this service on this port?" using
// a pre-compiled rule set built from the destination service's spec.
//
// Default-deny semantics (locked):
//
//   - A service with **any** ingress rule defined → traffic that does
//     not match an allow rule is denied (Kubernetes NetworkPolicy
//     parity).
//   - A service with no policy → all traffic allowed.
//
// Source identity:
//
//   - Same-node peers are resolved via the LocalInstances table
//     (containerIP → service+namespace) maintained by RUNE-063.
//   - Cross-node peers cannot be resolved to a service identity in
//     v1 (no mTLS yet); only CIDR / namespace selectors match.
//
// Egress:
//
//   - Evaluated when a local instance dials a service VIP. The
//     destination service+namespace is known from the dial target.
//
// All policy state is derived from the destination Service spec. No
// dedicated `policy/` orderedlog keyspace is used in v1 — the spec
// travels with the Service. The `policy/` prefix remains reserved
// for a future denormalized compiled-policy index.
package policy

import (
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"

	"github.com/runestack/rune/pkg/types"
)

// Decision is the outcome of evaluating a connection against a
// compiled policy.
type Decision int

const (
	// DecisionAllow means the connection is permitted.
	DecisionAllow Decision = iota
	// DecisionDeny means the connection is rejected by an explicit
	// or default-deny rule.
	DecisionDeny
	// DecisionNoPolicy means the destination service has no policy
	// at all (open service). Callers treat this as Allow but may
	// account for it separately in metrics.
	DecisionNoPolicy
)

func (d Decision) String() string {
	switch d {
	case DecisionAllow:
		return "allow"
	case DecisionDeny:
		return "deny"
	case DecisionNoPolicy:
		return "no_policy"
	}
	return "unknown"
}

// Reason is a short, machine-friendly tag summarising why a Decision
// was reached. It feeds the Prometheus drop counter and the per-drop
// log line.
type Reason string

const (
	ReasonNoPolicy             Reason = "no_policy"
	ReasonAllowedBySvc         Reason = "allow_service"
	ReasonAllowedByNamespace   Reason = "allow_namespace"
	ReasonAllowedBySelector    Reason = "allow_selector"
	ReasonAllowedByCIDR        Reason = "allow_cidr"
	ReasonDeniedNoMatch        Reason = "no_matching_rule"
	ReasonDeniedPort           Reason = "port_not_allowed"
	ReasonDeniedCrossNodeIdent Reason = "cross_node_identity_unresolved"
)

// PeerInfo describes the source of an inbound connection. Same-node
// peers carry an Identity; cross-node peers only have an IP.
type PeerInfo struct {
	// IP is the source IP of the connection. Always present.
	IP net.IP
	// Identity is the (service, namespace) of the source when the
	// peer is a same-node container. Nil for cross-node peers or
	// when the agent has no local-instances entry for IP.
	Identity *types.InstanceIdentity
	// SameNode indicates whether IP belongs to a container on this
	// agent's node. Used to gate service-name selectors that cannot
	// match cross-node peers in v1.
	SameNode bool
}

// EgressTarget describes the destination of an outbound connection
// initiated by a local instance.
type EgressTarget struct {
	Service   string
	Namespace string
	Port      int
}

// Compiled is the evaluator-friendly form of a ServiceNetworkPolicy.
// It is small (one per service), immutable, and safe for concurrent
// reads. Build it once with Compile and replace atomically when the
// underlying spec changes.
type Compiled struct {
	// ServiceID identifies the destination service this policy
	// belongs to. Used in logs and metrics.
	ServiceID string
	// Namespace is the destination service's namespace.
	Namespace string
	// PolicyName is a stable label for log/metric attribution. We
	// don't have a separate Policy resource yet, so this is just
	// the service ID.
	PolicyName string

	// hasIngress controls default-deny activation for ingress.
	hasIngress bool
	// hasEgress controls default-deny activation for egress.
	hasEgress bool

	ingress []ingressRule
	egress  []egressRule
}

type ingressRule struct {
	peers []peerMatcher
	ports portSet
}

type egressRule struct {
	peers []peerMatcher
	ports portSet
}

// peerMatcher is the compiled form of a NetworkPolicyPeer. Exactly
// one of the fields is set (Service+Namespace count as a single
// matcher kind).
type peerMatcher struct {
	// kind reflects which selector applies for explanation/logging.
	kind matcherKind

	service   string
	namespace string
	selector  map[string]string
	cidr      *net.IPNet
}

type matcherKind int

const (
	matcherService matcherKind = iota
	matcherNamespace
	matcherSelector
	matcherCIDR
)

// portSet represents an allow-list of (port, protocol) pairs. An
// empty set means "all ports allowed" within the rule (matching
// Kubernetes NetworkPolicy semantics).
type portSet struct {
	all   bool // empty Ports → wildcard
	ports []portRule
}

type portRule struct {
	// proto is "" (any), "tcp", or "udp".
	proto string
	port  int
}

// HasIngressRules reports whether default-deny ingress is in effect.
func (c *Compiled) HasIngressRules() bool { return c != nil && c.hasIngress }

// HasEgressRules reports whether default-deny egress is in effect.
func (c *Compiled) HasEgressRules() bool { return c != nil && c.hasEgress }

// Compile builds an evaluator for svc. svc may be nil (returns nil).
// Spec validation errors are surfaced via Validate; Compile is a
// pure transform and never returns an error — invalid peers are
// dropped from the rule set so a misconfigured spec fails closed.
func Compile(svc *types.Service) *Compiled {
	if svc == nil || svc.NetworkPolicy == nil {
		return nil
	}
	pol := svc.NetworkPolicy
	if len(pol.Ingress) == 0 && len(pol.Egress) == 0 {
		return nil
	}
	c := &Compiled{
		ServiceID:  svc.ID,
		Namespace:  svc.Namespace,
		PolicyName: svc.ID,
		hasIngress: len(pol.Ingress) > 0,
		hasEgress:  len(pol.Egress) > 0,
	}
	for _, r := range pol.Ingress {
		c.ingress = append(c.ingress, ingressRule{
			peers: compilePeers(r.From),
			ports: compilePortSet(r.Ports),
		})
	}
	for _, r := range pol.Egress {
		c.egress = append(c.egress, egressRule{
			peers: compilePeers(r.To),
			ports: compilePortSet(r.Ports),
		})
	}
	return c
}

func compilePeers(peers []types.NetworkPolicyPeer) []peerMatcher {
	out := make([]peerMatcher, 0, len(peers))
	for _, p := range peers {
		switch {
		case p.CIDR != "":
			_, n, err := net.ParseCIDR(p.CIDR)
			if err != nil {
				continue
			}
			out = append(out, peerMatcher{kind: matcherCIDR, cidr: n})
		case len(p.ServiceSelector) > 0:
			cp := make(map[string]string, len(p.ServiceSelector))
			for k, v := range p.ServiceSelector {
				cp[k] = v
			}
			out = append(out, peerMatcher{kind: matcherSelector, selector: cp})
		case p.Service != "":
			out = append(out, peerMatcher{
				kind:      matcherService,
				service:   p.Service,
				namespace: p.Namespace,
			})
		case p.Namespace != "":
			out = append(out, peerMatcher{kind: matcherNamespace, namespace: p.Namespace})
		}
	}
	return out
}

func compilePortSet(raw []string) portSet {
	if len(raw) == 0 {
		return portSet{all: true}
	}
	ps := portSet{}
	for _, s := range raw {
		pr, ok := parsePort(s)
		if !ok {
			continue
		}
		ps.ports = append(ps.ports, pr)
	}
	if len(ps.ports) == 0 {
		// All entries were unparseable → fail closed by allowing
		// nothing in this rule (the rule itself becomes a no-op).
		return ps
	}
	return ps
}

// parsePort accepts "8080", "80/tcp", "53/udp" (case-insensitive).
func parsePort(s string) (portRule, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return portRule{}, false
	}
	proto := ""
	if i := strings.IndexByte(s, '/'); i > 0 {
		proto = strings.ToLower(strings.TrimSpace(s[i+1:]))
		s = s[:i]
		if proto != "tcp" && proto != "udp" {
			return portRule{}, false
		}
	}
	p, err := strconv.Atoi(s)
	if err != nil || p <= 0 || p > 65535 {
		return portRule{}, false
	}
	return portRule{proto: proto, port: p}, true
}

func (ps portSet) matches(port int, proto string) bool {
	if ps.all {
		return true
	}
	for _, p := range ps.ports {
		if p.port != port {
			continue
		}
		if p.proto == "" || p.proto == proto {
			return true
		}
	}
	return false
}

// Result captures both the binary Decision and a Reason for logs +
// metrics + `rune policy explain` output.
type Result struct {
	Decision Decision
	Reason   Reason
	// MatchedRule is the index into the ingress/egress slice that
	// produced an Allow decision. -1 when not applicable.
	MatchedRule int
}

// EvaluateIngress decides whether peer is allowed to reach the
// service this Compiled belongs to on (port, proto). proto is "tcp"
// or "udp".
//
// A nil Compiled means "no policy" → DecisionNoPolicy / Allow.
func (c *Compiled) EvaluateIngress(peer PeerInfo, port int, proto string) Result {
	if c == nil || !c.hasIngress {
		return Result{Decision: DecisionNoPolicy, Reason: ReasonNoPolicy, MatchedRule: -1}
	}
	for i, r := range c.ingress {
		if !r.ports.matches(port, proto) {
			continue
		}
		for _, m := range r.peers {
			if reason, ok := matchPeer(m, peer); ok {
				return Result{Decision: DecisionAllow, Reason: reason, MatchedRule: i}
			}
		}
	}
	// Default-deny: did any rule match the port at all?
	portReachable := false
	for _, r := range c.ingress {
		if r.ports.matches(port, proto) {
			portReachable = true
			break
		}
	}
	reason := ReasonDeniedNoMatch
	if !portReachable {
		reason = ReasonDeniedPort
	}
	if !peer.SameNode && peer.Identity == nil {
		// Cross-node peer with no identity tried to match a
		// service-name rule somewhere — give the operator a hint.
		for _, r := range c.ingress {
			for _, m := range r.peers {
				if m.kind == matcherService || m.kind == matcherSelector {
					reason = ReasonDeniedCrossNodeIdent
					break
				}
			}
		}
	}
	return Result{Decision: DecisionDeny, Reason: reason, MatchedRule: -1}
}

// EvaluateEgress decides whether the service this Compiled belongs
// to may dial target on (port, proto).
func (c *Compiled) EvaluateEgress(target EgressTarget, proto string) Result {
	if c == nil || !c.hasEgress {
		return Result{Decision: DecisionNoPolicy, Reason: ReasonNoPolicy, MatchedRule: -1}
	}
	for i, r := range c.egress {
		if !r.ports.matches(target.Port, proto) {
			continue
		}
		for _, m := range r.peers {
			if reason, ok := matchEgressPeer(m, target); ok {
				return Result{Decision: DecisionAllow, Reason: reason, MatchedRule: i}
			}
		}
	}
	return Result{Decision: DecisionDeny, Reason: ReasonDeniedNoMatch, MatchedRule: -1}
}

func matchPeer(m peerMatcher, peer PeerInfo) (Reason, bool) {
	switch m.kind {
	case matcherCIDR:
		if peer.IP != nil && m.cidr.Contains(peer.IP) {
			return ReasonAllowedByCIDR, true
		}
	case matcherService:
		// Service-name rules never match cross-node peers in v1.
		if !peer.SameNode || peer.Identity == nil {
			return "", false
		}
		if peer.Identity.Service != m.service {
			return "", false
		}
		if m.namespace != "" && peer.Identity.Namespace != m.namespace {
			return "", false
		}
		return ReasonAllowedBySvc, true
	case matcherNamespace:
		// Namespace match: same-node uses the resolved identity;
		// cross-node has no identity so namespace selectors only
		// match same-node peers in v1.
		if peer.Identity != nil && peer.Identity.Namespace == m.namespace {
			return ReasonAllowedByNamespace, true
		}
	case matcherSelector:
		// ServiceSelector requires identity → same-node only in v1.
		// Selector evaluation against actual service labels is
		// deferred; matchers compile but never match. Documented
		// as a v1 limitation.
		return "", false
	}
	return "", false
}

func matchEgressPeer(m peerMatcher, t EgressTarget) (Reason, bool) {
	switch m.kind {
	case matcherCIDR:
		// CIDR egress matches only when target carries an IP via
		// the dial target. Since we only know svc/ns at egress
		// time, CIDR egress is a no-op in v1.
		return "", false
	case matcherService:
		if t.Service != m.service {
			return "", false
		}
		if m.namespace != "" && t.Namespace != m.namespace {
			return "", false
		}
		return ReasonAllowedBySvc, true
	case matcherNamespace:
		if t.Namespace == m.namespace {
			return ReasonAllowedByNamespace, true
		}
	case matcherSelector:
		// As per ingress, selector matching against the destination
		// service spec is deferred.
		return "", false
	}
	return "", false
}

// IngressRuleCount returns the number of compiled ingress rules.
// Used by metrics (rune_policy_rules gauge).
func (c *Compiled) IngressRuleCount() int {
	if c == nil {
		return 0
	}
	return len(c.ingress)
}

// EgressRuleCount returns the number of compiled egress rules.
func (c *Compiled) EgressRuleCount() int {
	if c == nil {
		return 0
	}
	return len(c.egress)
}

// Explain returns a human-readable summary of the compiled policy
// suitable for `rune policy explain <service>`. The output is
// deterministic given the same input policy.
func (c *Compiled) Explain() ExplainOutput {
	if c == nil {
		return ExplainOutput{Open: true}
	}
	out := ExplainOutput{
		ServiceID:  c.ServiceID,
		Namespace:  c.Namespace,
		PolicyName: c.PolicyName,
	}
	for _, r := range c.ingress {
		out.Ingress = append(out.Ingress, explainRule(r.peers, r.ports))
	}
	for _, r := range c.egress {
		out.Egress = append(out.Egress, explainRule(r.peers, r.ports))
	}
	out.DefaultDenyIngress = c.hasIngress
	out.DefaultDenyEgress = c.hasEgress
	return out
}

func explainRule(peers []peerMatcher, ports portSet) ExplainRule {
	er := ExplainRule{}
	for _, p := range peers {
		switch p.kind {
		case matcherService:
			ns := p.namespace
			if ns == "" {
				ns = "*"
			}
			er.Peers = append(er.Peers, fmt.Sprintf("service=%s ns=%s scope=same-node", p.service, ns))
		case matcherNamespace:
			er.Peers = append(er.Peers, fmt.Sprintf("namespace=%s scope=same-node", p.namespace))
		case matcherSelector:
			er.Peers = append(er.Peers, fmt.Sprintf("serviceSelector=%v scope=same-node (v1: never matches)", p.selector))
		case matcherCIDR:
			er.Peers = append(er.Peers, fmt.Sprintf("cidr=%s scope=any", p.cidr.String()))
		}
	}
	if ports.all {
		er.Ports = []string{"*"}
	} else {
		for _, p := range ports.ports {
			if p.proto == "" {
				er.Ports = append(er.Ports, strconv.Itoa(p.port))
			} else {
				er.Ports = append(er.Ports, fmt.Sprintf("%d/%s", p.port, p.proto))
			}
		}
	}
	return er
}

// ExplainOutput is the JSON-friendly form rendered by the CLI.
type ExplainOutput struct {
	ServiceID          string        `json:"serviceId,omitempty"`
	Namespace          string        `json:"namespace,omitempty"`
	PolicyName         string        `json:"policy,omitempty"`
	Open               bool          `json:"open,omitempty"`
	DefaultDenyIngress bool          `json:"defaultDenyIngress,omitempty"`
	DefaultDenyEgress  bool          `json:"defaultDenyEgress,omitempty"`
	Ingress            []ExplainRule `json:"ingress,omitempty"`
	Egress             []ExplainRule `json:"egress,omitempty"`
}

// ExplainRule is a single human-readable rule entry.
type ExplainRule struct {
	Peers []string `json:"peers"`
	Ports []string `json:"ports"`
}

// Validate runs the public ServiceNetworkPolicy validator and
// surfaces any compilation-time issues (currently just CIDR parse
// errors that Compile silently drops). The validator is the formal
// gate; this is a softer secondary check used by the CLI dry-run
// path so operators see CIDR typos.
func Validate(pol *types.ServiceNetworkPolicy) error {
	if pol == nil {
		return nil
	}
	if err := pol.Validate(); err != nil {
		return err
	}
	for i, r := range pol.Ingress {
		for j, p := range r.From {
			if p.CIDR != "" {
				if _, _, err := net.ParseCIDR(p.CIDR); err != nil {
					return fmt.Errorf("ingress rule %d peer %d: invalid CIDR %q: %w", i, j, p.CIDR, err)
				}
			}
		}
	}
	for i, r := range pol.Egress {
		for j, p := range r.To {
			if p.CIDR != "" {
				if _, _, err := net.ParseCIDR(p.CIDR); err != nil {
					return fmt.Errorf("egress rule %d peer %d: invalid CIDR %q: %w", i, j, p.CIDR, err)
				}
			}
		}
	}
	return nil
}

// ErrInvalidPolicy wraps validator errors.
var ErrInvalidPolicy = errors.New("invalid network policy")
