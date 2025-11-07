package client

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/runestack/rune/pkg/api/generated"
	"github.com/runestack/rune/pkg/log"
	"google.golang.org/grpc/codes"
)

// HealthClient provides methods to query health info from the API server.
type HealthClient struct {
	client *Client
	logger log.Logger
	hc     generated.HealthServiceClient
}

// NewHealthClient creates a new health client.
func NewHealthClient(client *Client) *HealthClient {
	return &HealthClient{
		client: client,
		logger: client.logger.WithComponent("health-client"),
		hc:     generated.NewHealthServiceClient(client.conn),
	}
}

// GetHealth queries health for a component type
func (h *HealthClient) GetHealth(componentType, name, namespace string, includeChecks bool) (*generated.GetHealthResponse, error) {
	ctx, cancel := h.client.Context()
	defer cancel()

	req := &generated.GetHealthRequest{
		ComponentType: componentType,
		Name:          name,
		Namespace:     namespace,
		IncludeChecks: includeChecks,
	}
	resp, err := h.hc.GetHealth(ctx, req)
	if err != nil {
		return nil, convertGRPCError("get health", err)
	}
	if resp.Status != nil && resp.Status.Code != int32(codes.OK) {
		return nil, fmt.Errorf("API error: %s", resp.Status.Message)
	}
	return resp, nil
}

// GetAPIServerCapacity returns server-cached single-node capacity (CPU cores, memory bytes).
// It queries the API server health with IncludeChecks and parses the capacity check.
func (h *HealthClient) GetAPIServerCapacity() (float64, int64, error) {
	ctx, cancel := h.client.Context()
	defer cancel()

	resp, err := h.hc.GetHealth(ctx, &generated.GetHealthRequest{
		ComponentType: "api-server",
		IncludeChecks: true,
	})
	if err != nil {
		return 0, 0, convertGRPCError("get health", err)
	}
	if resp.Status != nil && resp.Status.Code != int32(codes.OK) {
		return 0, 0, fmt.Errorf("API error: %s", resp.Status.Message)
	}
	for _, c := range resp.Components {
		for _, chk := range c.CheckResults {
			if strings.HasPrefix(chk.Message, "capacity ") {
				// Format: "capacity cpu_cores=<float> mem_bytes=<int>"
				parts := strings.Fields(chk.Message)
				var cpu float64
				var mem int64
				for _, p := range parts[1:] { // skip "capacity"
					if strings.HasPrefix(p, "cpu_cores=") {
						v := strings.TrimPrefix(p, "cpu_cores=")
						if fv, err := strconv.ParseFloat(v, 64); err == nil {
							cpu = fv
						}
					} else if strings.HasPrefix(p, "mem_bytes=") {
						v := strings.TrimPrefix(p, "mem_bytes=")
						if iv, err := strconv.ParseInt(v, 10, 64); err == nil {
							mem = iv
						}
					}
				}
				if cpu > 0 && mem > 0 {
					return cpu, mem, nil
				}
			}
		}
	}
	return 0, 0, fmt.Errorf("capacity not available from server")
}
