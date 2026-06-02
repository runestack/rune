package server

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// UI observability metrics (RUNE-200). Registered once at package
// initialization against the default registerer, which cmd/runed exposes on
// the /metrics endpoint. Package-level vars register exactly once per process
// regardless of how many APIServers are constructed, so there is no
// duplicate-registration risk and no synchronization needed at use sites.
var (
	uiRequestsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "rune_ui_requests_total",
		Help: "Total HTTP requests served by the embedded dashboard layer, labelled by route class and status code.",
	}, []string{"route", "code"})

	uiActiveStreams = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "rune_ui_active_streams",
		Help: "Number of in-flight streaming requests through the dashboard gRPC-Web transcoder.",
	})

	uiHandoffAttempts = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "rune_ui_handoff_attempts_total",
		Help: "CLI token-handoff attempts, labelled by result (created, claimed, expired, notfound).",
	}, []string{"result"})
)

// uiHandoffResult records a handoff outcome.
func uiHandoffResult(result string) {
	uiHandoffAttempts.WithLabelValues(result).Inc()
}
