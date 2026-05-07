package dataplane

import (
	"github.com/prometheus/client_golang/prometheus"
)

// Metrics holds the dataplane's Prometheus collectors. The set is
// allocated per Subsystem so multiple subsystems in tests don't fight
// over the global registry; production wires the set into the
// runed-level registry via Register.
type Metrics struct {
	ConnectionsActive *prometheus.GaugeVec   // {service,protocol}
	ConnectionsTotal  *prometheus.CounterVec // {service,protocol,result}
	EndpointHealth    *prometheus.GaugeVec   // {service} = healthy count
	WatchLag          prometheus.Gauge       // seconds since last watch event
	NftablesRules     prometheus.Gauge       // current rule count (linux only)
	ListenersOpen     *prometheus.GaugeVec   // {service,protocol}
	PolicyDrops       *prometheus.CounterVec // {service,namespace,policy,reason}
	PolicyAllows      *prometheus.CounterVec // {service,namespace,policy}
	PolicyRules       *prometheus.GaugeVec   // {service,namespace} -> ingress+egress rule count
	PolicyLastSeq     *prometheus.GaugeVec   // {service,namespace} -> last refresh timestamp (unix seconds)
	LocalInstances    prometheus.Gauge       // size of LocalInstances table
}

func newMetrics() *Metrics {
	return &Metrics{
		ConnectionsActive: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "rune_proxy_connections_active",
			Help: "Active proxy connections by service and protocol.",
		}, []string{"service", "protocol"}),
		ConnectionsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "rune_proxy_connections_total",
			Help: "Total proxy connection attempts by service, protocol, and outcome.",
		}, []string{"service", "protocol", "result"}),
		EndpointHealth: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "rune_proxy_endpoint_health",
			Help: "Number of endpoints last seen for a service (regardless of health).",
		}, []string{"service"}),
		WatchLag: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "rune_proxy_watch_lag_seconds",
			Help: "Seconds since the last successful OrderedLog watch event.",
		}),
		NftablesRules: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "rune_proxy_nftables_rules",
			Help: "Number of nftables rules currently programmed by the dataplane (Linux only).",
		}),
		ListenersOpen: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "rune_proxy_listeners_open",
			Help: "Open per-VIP proxy listeners by service and protocol.",
		}, []string{"service", "protocol"}),
		PolicyDrops: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "rune_policy_drops_total",
			Help: "Connections rejected by network policy, labelled by destination service and reason.",
		}, []string{"service", "namespace", "policy", "reason"}),
		PolicyAllows: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "rune_policy_allows_total",
			Help: "Connections explicitly allowed by network policy.",
		}, []string{"service", "namespace", "policy"}),
		PolicyRules: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "rune_policy_rules",
			Help: "Number of compiled ingress+egress rules currently active for a service.",
		}, []string{"service", "namespace"}),
		PolicyLastSeq: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "rune_policy_last_seq",
			Help: "Unix timestamp (seconds) of the last successful policy refresh per service. Operators use this to detect stale agents; in v1 the value is the registration time on the agent (no separate policy/ keyspace yet).",
		}, []string{"service", "namespace"}),
		LocalInstances: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "rune_policy_local_instances",
			Help: "Size of the agent's LocalInstances IP -> identity table.",
		}),
	}
}

// Register attaches every collector to reg. Idempotent failures
// (AlreadyRegistered) are tolerated so the agent can re-register
// after a controlled restart.
func (m *Metrics) Register(reg prometheus.Registerer) error {
	for _, c := range []prometheus.Collector{
		m.ConnectionsActive, m.ConnectionsTotal, m.EndpointHealth,
		m.WatchLag, m.NftablesRules, m.ListenersOpen,
		m.PolicyDrops, m.PolicyAllows, m.PolicyRules, m.PolicyLastSeq, m.LocalInstances,
	} {
		if err := reg.Register(c); err != nil {
			if _, ok := err.(prometheus.AlreadyRegisteredError); !ok {
				return err
			}
		}
	}
	return nil
}

func (m *Metrics) incActive(svc, proto string) { m.ConnectionsActive.WithLabelValues(svc, proto).Inc() }
func (m *Metrics) decActive(svc, proto string) { m.ConnectionsActive.WithLabelValues(svc, proto).Dec() }
func (m *Metrics) incTotal(svc, proto, res string) {
	m.ConnectionsTotal.WithLabelValues(svc, proto, res).Inc()
}
func (m *Metrics) observeEndpointSet(svc string, n int) {
	m.EndpointHealth.WithLabelValues(svc).Set(float64(n))
}
func (m *Metrics) observeListenerOpened(svc, proto string) {
	m.ListenersOpen.WithLabelValues(svc, proto).Inc()
}
func (m *Metrics) observeListenerClosed(svc, proto string) {
	m.ListenersOpen.WithLabelValues(svc, proto).Dec()
}
func (m *Metrics) setWatchLag(s float64)  { m.WatchLag.Set(s) }
func (m *Metrics) setNftablesRules(n int) { m.NftablesRules.Set(float64(n)) }

func (m *Metrics) incPolicyDrop(svc, ns, pol, reason string) {
	m.PolicyDrops.WithLabelValues(svc, ns, pol, reason).Inc()
}
func (m *Metrics) incPolicyAllow(svc, ns, pol string) {
	m.PolicyAllows.WithLabelValues(svc, ns, pol).Inc()
}
func (m *Metrics) setPolicyRules(svc, ns string, n int) {
	m.PolicyRules.WithLabelValues(svc, ns).Set(float64(n))
}
func (m *Metrics) setPolicyLastSeq(svc, ns string, ts int64) {
	m.PolicyLastSeq.WithLabelValues(svc, ns).Set(float64(ts))
}
func (m *Metrics) setLocalInstances(n int) { m.LocalInstances.Set(float64(n)) }
