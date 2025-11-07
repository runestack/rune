package probes

import (
	"fmt"
	"net/http"
	"time"

	"github.com/runestack/rune/pkg/log"
)

// HTTPProber implements the HTTP health check probe
type HTTPProber struct{}

// Execute implements the Prober interface for HTTP probes
func (p *HTTPProber) Execute(ctx *ProbeContext) ProbeResult {
	ctx.Logger.Info("Executing HTTP probe", log.Str("instance", ctx.Instance.Name), log.Str("probe_type", "http"))
	start := time.Now()

	// Determine endpoint for the check
	endpoint := fmt.Sprintf("http://localhost:%d", ctx.ProbeConfig.Port)
	url := fmt.Sprintf("%s%s", endpoint, ctx.ProbeConfig.Path)

	// Create HTTP request
	req, err := http.NewRequestWithContext(ctx.Ctx, "GET", url, nil)
	if err != nil {
		return ProbeResult{
			Success:  false,
			Message:  fmt.Sprintf("Failed to create HTTP request: %v", err),
			Duration: time.Since(start),
		}
	}

	// Execute request
	resp, err := ctx.HTTPClient.Do(req)
	if err != nil {
		return ProbeResult{
			Success:  false,
			Message:  fmt.Sprintf("HTTP health check failed: %v", err),
			Duration: time.Since(start),
		}
	}
	defer resp.Body.Close()

	// Check for successful status code (200-399)
	if resp.StatusCode >= 200 && resp.StatusCode < 400 {
		return ProbeResult{
			Success:  true,
			Message:  fmt.Sprintf("HTTP health check succeeded with status %d", resp.StatusCode),
			Duration: time.Since(start),
		}
	}

	return ProbeResult{
		Success:  false,
		Message:  fmt.Sprintf("HTTP health check returned non-success status %d", resp.StatusCode),
		Duration: time.Since(start),
	}
}
