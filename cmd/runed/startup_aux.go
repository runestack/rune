package main

import (
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/runestack/rune/pkg/log"
)

// startAuxiliarySurfaces runs startup phase 9 (RUNE-313): the optional
// Prometheus metrics server and the SIGHUP DNS-resolver reload. Both are
// optional by construction — metricsAddr may be empty and node.dns may be nil
// (dev mode with nothing bindable) — so this phase can legitimately do
// nothing. It returns the metrics server so shutdown can stop it first.
func startAuxiliarySurfaces(b *boot, n *node) *http.Server {
	ctx := b.ctx
	logger := b.logger
	dnsSub := n.dns

	// Optional: serve Prometheus metrics on a private address so
	// scrapers can collect dataplane + future subsystem metrics.
	var metricsServer *http.Server
	if *metricsAddr != "" {
		mux := http.NewServeMux()
		mux.Handle("/metrics", promhttp.Handler())
		metricsServer = &http.Server{
			Addr:              *metricsAddr,
			Handler:           mux,
			ReadHeaderTimeout: 5 * time.Second,
		}
		go func() {
			logger.Info("Metrics server listening", log.Str("addr", *metricsAddr))
			if err := metricsServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				logger.Warn("Metrics server stopped", log.Err(err))
			}
		}()
	}

	// SIGHUP triggers a re-read of upstream DNS resolvers (RUNE-063).
	// Useful when /etc/resolv.conf changes (DHCP renewal, NetworkManager
	// reload, etc.) without restarting runed.
	if dnsSub != nil {
		hupCh := make(chan os.Signal, 1)
		signal.Notify(hupCh, syscall.SIGHUP)
		go func() {
			for {
				select {
				case <-ctx.Done():
					return
				case <-hupCh:
					if err := dnsSub.Refresh(); err != nil {
						logger.Warn("DNS refresh failed", log.Err(err))
					} else {
						logger.Info("DNS upstreams refreshed (SIGHUP)")
					}
				}
			}
		}()
	}

	return metricsServer
}
