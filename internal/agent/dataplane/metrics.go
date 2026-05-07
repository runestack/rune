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
	}
}

// Register attaches every collector to reg. Idempotent failures
// (AlreadyRegistered) are tolerated so the agent can re-register
// after a controlled restart.
func (m *Metrics) Register(reg prometheus.Registerer) error {
	for _, c := range []prometheus.Collector{
		m.ConnectionsActive, m.ConnectionsTotal, m.EndpointHealth,
		m.WatchLag, m.NftablesRules, m.ListenersOpen,
	} {
		if err := reg.Register(c); err != nil {
			if _, ok := err.(prometheus.AlreadyRegisteredError); !ok {
				return err
			}
		}
	}
	return nil
}

func (m *Metrics) incActive(svc, proto string)   { m.ConnectionsActive.WithLabelValues(svc, proto).Inc() }
func (m *Metrics) decActive(svc, proto string)   { m.ConnectionsActive.WithLabelValues(svc, proto).Dec() }
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
func (m *Metrics) setWatchLag(s float64) { m.WatchLag.Set(s) }
func (m *Metrics) setNftablesRules(n int) { m.NftablesRules.Set(float64(n)) }
