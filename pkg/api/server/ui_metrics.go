package server

import (
	"sync"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// UI observability metrics (RUNE-200). Registered against the default
// registerer, which cmd/runed exposes on the /metrics endpoint. Registration
// is guarded by a sync.Once so repeated server constructions in tests don't
// panic with duplicate-collector errors.
var (
	uiMetricsOnce sync.Once

	uiRequestsTotal     *prometheus.CounterVec
	uiActiveStreams     prometheus.Gauge
	uiHandoffAttempts   *prometheus.CounterVec
	uiMetricsRegistered bool
)

// initUIMetrics lazily registers the UI metric collectors exactly once.
func initUIMetrics() {
	uiMetricsOnce.Do(func() {
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

		uiMetricsRegistered = true
	})
}

// uiHandoffResult records a handoff outcome if metrics are initialized.
func uiHandoffResult(result string) {
	if uiMetricsRegistered {
		uiHandoffAttempts.WithLabelValues(result).Inc()
	}
}
