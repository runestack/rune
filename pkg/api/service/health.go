package service

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/runestack/rune/pkg/api/generated"
	"github.com/runestack/rune/pkg/log"
	"github.com/runestack/rune/pkg/store"
	"github.com/runestack/rune/pkg/types"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// HealthService implements the gRPC HealthService.
type HealthService struct {
	generated.UnimplementedHealthServiceServer

	store  store.Store
	logger log.Logger

	// Optional cached capacity (single-node MVP)
	capacityCPU float64
	capacityMem int64
	hasCapacity bool
}

// NewHealthService creates a new HealthService with the given store and logger.
func NewHealthService(store store.Store, logger log.Logger) *HealthService {
	hs := &HealthService{
		store:  store,
		logger: logger.WithComponent("health-service"),
	}
	// Best-effort capacity detection at construction
	hs.DetectAndSetCapacity()
	return hs
}

// SetCapacity allows the API server to inject cached node capacity for reporting.
func (s *HealthService) SetCapacity(cpuCores float64, memBytes int64) {
	s.capacityCPU = cpuCores
	s.capacityMem = memBytes
	s.hasCapacity = true
}

// DetectAndSetCapacity runs capacity detection and caches the result in the service.
func (s *HealthService) DetectAndSetCapacity() {
	cpu, mem, err := s.detectNodeCapacity()
	if err != nil {
		s.logger.Warn("Capacity detection failed", log.Err(err))
		return
	}
	s.SetCapacity(cpu, mem)
	s.logger.Info("Node capacity detected",
		log.Float64("cpu_cores", cpu),
		log.Str("memory", s.formatMemory(mem)))
}

// detectNodeCapacity computes best-effort CPU cores and memory bytes for the host.
// Prefers cgroup v2 quotas if present; falls back to host capacity.
func (s *HealthService) detectNodeCapacity() (float64, int64, error) {
	cpu := s.detectCPUCores()
	mem := s.detectMemoryBytes()
	if mem == 0 {
		return cpu, 0, fmt.Errorf("unable to determine memory capacity")
	}
	return cpu, mem, nil
}

func (s *HealthService) detectCPUCores() float64 {
	if quota, period, ok := s.readCgroupCPUMax("/sys/fs/cgroup/cpu.max"); ok {
		if quota > 0 && period > 0 {
			eff := float64(quota) / float64(period)
			if eff > 0 {
				return eff
			}
		}
	}
	return float64(runtime.NumCPU())
}

func (s *HealthService) detectMemoryBytes() int64 {
	if v, ok := s.readCgroupMemoryMax("/sys/fs/cgroup/memory.max"); ok && v > 0 && v < (1<<60) {
		return v
	}
	if v, ok := s.readProcMeminfoTotal("/proc/meminfo"); ok {
		return v
	}
	if v, ok := s.readSysctlMemsize(); ok {
		return v
	}
	return 0
}

func (s *HealthService) readCgroupCPUMax(path string) (quota int64, period int64, ok bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, 0, false
	}
	parts := strings.Fields(string(data))
	if len(parts) != 2 {
		return 0, 0, false
	}
	if parts[0] == "max" {
		p, _ := strconv.ParseInt(parts[1], 10, 64)
		return 0, p, true
	}
	q, err1 := strconv.ParseInt(parts[0], 10, 64)
	p, err2 := strconv.ParseInt(parts[1], 10, 64)
	if err1 != nil || err2 != nil {
		return 0, 0, false
	}
	return q, p, true
}

func (s *HealthService) readCgroupMemoryMax(path string) (int64, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	v := strings.TrimSpace(string(data))
	if v == "max" {
		return 0, false
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return 0, false
	}
	return n, true
}

func (s *HealthService) readProcMeminfoTotal(path string) (int64, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "MemTotal:") {
			fields := strings.Fields(line)
			if len(fields) >= 2 {
				if kb, err := strconv.ParseInt(fields[1], 10, 64); err == nil {
					return kb * 1024, true
				}
			}
		}
	}
	return 0, false
}

func (s *HealthService) readSysctlMemsize() (int64, bool) {
	if runtime.GOOS != "darwin" {
		return 0, false
	}
	out, err := exec.Command("sysctl", "-n", "hw.memsize").CombinedOutput()
	if err != nil {
		return 0, false
	}
	v := strings.TrimSpace(string(out))
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return 0, false
	}
	return n, true
}

func (s *HealthService) formatMemory(bytes int64) string {
	const unit = 1024
	if bytes < unit {
		return fmt.Sprintf("%dB", bytes)
	}
	div, exp := int64(unit), 0
	for n := bytes / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	units := []string{"Ki", "Mi", "Gi", "Ti", "Pi", "Ei"}
	return fmt.Sprintf("%.1f%s", float64(bytes)/float64(div), units[exp])
}

// GetHealth retrieves health status of platform components.
func (s *HealthService) GetHealth(ctx context.Context, req *generated.GetHealthRequest) (*generated.GetHealthResponse, error) {
	s.logger.Debug("GetHealth called", log.Str("component", req.ComponentType))

	// If no component type specified, default to API server health
	if req.ComponentType == "" {
		return s.getAPIServerHealth(ctx, req)
	}

	// Handle based on component type
	switch req.ComponentType {
	case "service":
		return s.getServiceHealth(ctx, req)
	case "instance":
		return s.getInstanceHealth(ctx, req)
	case "node":
		return s.getNodeHealth(ctx, req)
	case "api-server":
		return s.getAPIServerHealth(ctx, req)
	default:
		return nil, status.Errorf(codes.InvalidArgument, "unknown component type: %s", req.ComponentType)
	}
}

// getServiceHealth retrieves health for a service or services.
func (s *HealthService) getServiceHealth(ctx context.Context, req *generated.GetHealthRequest) (*generated.GetHealthResponse, error) {
	namespace := req.Namespace
	if namespace == "" {
		namespace = DefaultNamespace
	}

	var components []*generated.ComponentHealth

	if req.Name != "" {
		// Get health for a specific service
		var service types.Service
		if err := s.store.Get(ctx, types.ResourceTypeService, namespace, req.Name, &service); err != nil {
			if IsNotFound(err) {
				return nil, status.Errorf(codes.NotFound, "service not found: %s", req.Name)
			}
			s.logger.Error("Failed to get service", log.Err(err))
			return nil, status.Errorf(codes.Internal, "failed to get service: %v", err)
		}

		// Get instances for this service
		healthStatus, healthMessage := s.computeServiceHealthFromInstances(ctx, service.Name, namespace)

		component := &generated.ComponentHealth{
			ComponentType: "service",
			Id:            service.ID,
			Name:          service.Name,
			Namespace:     namespace,
			Status:        healthStatus,
			Message:       healthMessage,
			Timestamp:     time.Now().Format(time.RFC3339),
		}

		if req.IncludeChecks {
			// Include detailed check results if requested
			// This would extract from service.Health definitions and actual check results
			component.CheckResults = generateMockHealthCheckResults()
		}

		components = append(components, component)
	} else {
		// Get health for all services in the namespace
		var services []types.Service
		err := s.store.List(ctx, types.ResourceTypeService, namespace, &services)
		if err != nil {
			s.logger.Error("Failed to list services", log.Err(err))
			return nil, status.Errorf(codes.Internal, "failed to list services: %v", err)
		}

		for _, service := range services {

			healthStatus, healthMessage := s.computeServiceHealthFromInstances(ctx, service.Name, namespace)

			component := &generated.ComponentHealth{
				ComponentType: "service",
				Id:            service.ID,
				Name:          service.Name,
				Namespace:     namespace,
				Status:        healthStatus,
				Message:       healthMessage,
				Timestamp:     time.Now().Format(time.RFC3339),
			}

			if req.IncludeChecks {
				// Include detailed check results if requested
				component.CheckResults = generateMockHealthCheckResults()
			}

			components = append(components, component)
		}
	}

	return &generated.GetHealthResponse{
		Components: components,
		Status: &generated.Status{
			Code:    int32(codes.OK),
			Message: fmt.Sprintf("Retrieved health for %d services", len(components)),
		},
	}, nil
}

// getInstanceHealth retrieves health for an instance or instances.
func (s *HealthService) getInstanceHealth(ctx context.Context, req *generated.GetHealthRequest) (*generated.GetHealthResponse, error) {
	namespace := req.Namespace
	if namespace == "" {
		namespace = DefaultNamespace
	}

	var components []*generated.ComponentHealth

	// Helper to map instance status to health
	mapStatus := func(st types.InstanceStatus) (generated.HealthStatus, string) {
		switch st {
		case types.InstanceStatusRunning:
			return generated.HealthStatus_HEALTH_STATUS_HEALTHY, "Instance running"
		case types.InstanceStatusPending, types.InstanceStatusCreated, types.InstanceStatusStarting:
			return generated.HealthStatus_HEALTH_STATUS_DEGRADED, string(st)
		case types.InstanceStatusStopped, types.InstanceStatusExited, types.InstanceStatusFailed, types.InstanceStatusDeleted:
			return generated.HealthStatus_HEALTH_STATUS_UNHEALTHY, string(st)
		default:
			return generated.HealthStatus_HEALTH_STATUS_UNKNOWN, string(st)
		}
	}

	if req.Name != "" {
		// Treat name as instance ID for retrieval
		inst, err := s.store.GetInstanceByID(ctx, namespace, req.Name)
		if err != nil {
			s.logger.Error("Failed to get instance", log.Err(err), log.Str("id", req.Name), log.Str("namespace", namespace))
			return nil, status.Errorf(codes.NotFound, "instance not found: %s", req.Name)
		}
		hs, msg := mapStatus(inst.Status)
		comp := &generated.ComponentHealth{
			ComponentType: "instance",
			Id:            inst.ID,
			Name:          inst.Name,
			Namespace:     inst.Namespace,
			Status:        hs,
			Message:       msg,
			Timestamp:     time.Now().Format(time.RFC3339),
		}
		if req.IncludeChecks {
			comp.CheckResults = []*generated.HealthCheckResult{
				{
					Type:                 generated.HealthCheckType_HEALTH_CHECK_TYPE_LIVENESS,
					Status:               hs,
					Message:              fmt.Sprintf("status=%s", inst.Status),
					Timestamp:            time.Now().Format(time.RFC3339),
					ConsecutiveSuccesses: 1,
					ConsecutiveFailures:  0,
				},
			}
		}
		components = append(components, comp)
	} else {
		// List all instances in namespace
		var instances []types.Instance
		if err := s.store.List(ctx, types.ResourceTypeInstance, namespace, &instances); err != nil {
			s.logger.Error("Failed to list instances", log.Err(err))
			return nil, status.Errorf(codes.Internal, "failed to list instances: %v", err)
		}
		for _, inst := range instances {
			hs, msg := mapStatus(inst.Status)
			comp := &generated.ComponentHealth{
				ComponentType: "instance",
				Id:            inst.ID,
				Name:          inst.Name,
				Namespace:     inst.Namespace,
				Status:        hs,
				Message:       msg,
				Timestamp:     time.Now().Format(time.RFC3339),
			}
			if req.IncludeChecks {
				comp.CheckResults = []*generated.HealthCheckResult{
					{
						Type:                 generated.HealthCheckType_HEALTH_CHECK_TYPE_LIVENESS,
						Status:               hs,
						Message:              fmt.Sprintf("status=%s", inst.Status),
						Timestamp:            time.Now().Format(time.RFC3339),
						ConsecutiveSuccesses: 1,
						ConsecutiveFailures:  0,
					},
				}
			}
			components = append(components, comp)
		}
	}

	return &generated.GetHealthResponse{
		Components: components,
		Status: &generated.Status{
			Code:    int32(codes.OK),
			Message: fmt.Sprintf("Retrieved health for %d instances", len(components)),
		},
	}, nil
}

// getNodeHealth retrieves health for a node or nodes.
func (s *HealthService) getNodeHealth(ctx context.Context, req *generated.GetHealthRequest) (*generated.GetHealthResponse, error) {
	var components []*generated.ComponentHealth

	// MVP: Single-node synthetic health based on server capacity detection
	name := req.Name
	if name == "" {
		name = "local"
	}
	status := generated.HealthStatus_HEALTH_STATUS_HEALTHY
	msg := "Single-node"
	comp := &generated.ComponentHealth{
		ComponentType: "node",
		Id:            name,
		Name:          name,
		Status:        status,
		Message:       msg,
		Timestamp:     time.Now().Format(time.RFC3339),
	}
	if req.IncludeChecks && s.hasCapacity {
		comp.CheckResults = []*generated.HealthCheckResult{
			{
				Type:                 generated.HealthCheckType_HEALTH_CHECK_TYPE_READINESS,
				Status:               status,
				Message:              fmt.Sprintf("capacity cpu_cores=%.3f mem_bytes=%d", s.capacityCPU, s.capacityMem),
				Timestamp:            time.Now().Format(time.RFC3339),
				ConsecutiveSuccesses: 1,
				ConsecutiveFailures:  0,
			},
		}
	}
	components = append(components, comp)

	return &generated.GetHealthResponse{
		Components: components,
		Status: &generated.Status{
			Code:    int32(codes.OK),
			Message: fmt.Sprintf("Retrieved health for %d nodes", len(components)),
		},
	}, nil
}

// getAPIServerHealth retrieves health for the API server.
func (s *HealthService) getAPIServerHealth(ctx context.Context, req *generated.GetHealthRequest) (*generated.GetHealthResponse, error) {
	// API server is always healthy if we're here to respond
	component := &generated.ComponentHealth{
		ComponentType: "api-server",
		Id:            "api-server",
		Name:          "api-server",
		Status:        generated.HealthStatus_HEALTH_STATUS_HEALTHY,
		Message:       "API server is operating normally",
		Timestamp:     time.Now().Format(time.RFC3339),
	}

	if req.IncludeChecks {
		checks := []*generated.HealthCheckResult{
			{
				Type:                 generated.HealthCheckType_HEALTH_CHECK_TYPE_LIVENESS,
				Status:               generated.HealthStatus_HEALTH_STATUS_HEALTHY,
				Message:              "API server is responding to requests",
				Timestamp:            time.Now().Format(time.RFC3339),
				ConsecutiveSuccesses: 100,
				ConsecutiveFailures:  0,
			},
			{
				Type:                 generated.HealthCheckType_HEALTH_CHECK_TYPE_READINESS,
				Status:               generated.HealthStatus_HEALTH_STATUS_HEALTHY,
				Message:              "API server is ready to process requests",
				Timestamp:            time.Now().Format(time.RFC3339),
				ConsecutiveSuccesses: 100,
				ConsecutiveFailures:  0,
			},
		}
		// Append capacity info if available, encoded in a structured string
		if s.hasCapacity {
			checks = append(checks, &generated.HealthCheckResult{
				Type:                 generated.HealthCheckType_HEALTH_CHECK_TYPE_STARTUP,
				Status:               generated.HealthStatus_HEALTH_STATUS_HEALTHY,
				Message:              fmt.Sprintf("capacity cpu_cores=%.3f mem_bytes=%d", s.capacityCPU, s.capacityMem),
				Timestamp:            time.Now().Format(time.RFC3339),
				ConsecutiveSuccesses: 1,
				ConsecutiveFailures:  0,
			})
		}
		component.CheckResults = checks
	}

	return &generated.GetHealthResponse{
		Components: []*generated.ComponentHealth{component},
		Status: &generated.Status{
			Code:    int32(codes.OK),
			Message: "API server is healthy",
		},
	}, nil
}

// computeServiceHealthFromInstances computes the health status of a service from its instances.
func (s *HealthService) computeServiceHealthFromInstances(ctx context.Context, serviceName, namespace string) (generated.HealthStatus, string) {
	// Get all instances for the service
	var instances []types.Instance
	err := s.store.List(ctx, types.ResourceTypeInstance, namespace, &instances)
	if err != nil {
		s.logger.Error("Failed to list instances", log.Err(err))
		return generated.HealthStatus_HEALTH_STATUS_UNKNOWN, "Failed to retrieve instance data"
	}

	// Count instances by status
	var totalInstances, runningInstances, failedInstances int
	for _, instance := range instances {
		if instance.ServiceName != serviceName {
			continue
		}

		totalInstances++
		switch instance.Status {
		case types.InstanceStatusRunning:
			runningInstances++
		case types.InstanceStatusFailed:
			failedInstances++
		}
	}

	// Determine overall health
	if totalInstances == 0 {
		return generated.HealthStatus_HEALTH_STATUS_UNKNOWN, "No instances found"
	}

	if runningInstances == totalInstances {
		return generated.HealthStatus_HEALTH_STATUS_HEALTHY, fmt.Sprintf("All %d instances are running", totalInstances)
	}

	if failedInstances == totalInstances {
		return generated.HealthStatus_HEALTH_STATUS_UNHEALTHY, fmt.Sprintf("All %d instances have failed", totalInstances)
	}

	if runningInstances > 0 {
		return generated.HealthStatus_HEALTH_STATUS_DEGRADED,
			fmt.Sprintf("%d/%d instances are running", runningInstances, totalInstances)
	}

	return generated.HealthStatus_HEALTH_STATUS_UNHEALTHY,
		fmt.Sprintf("No instances are running, %d/%d instances have failed", failedInstances, totalInstances)
}

// generateMockHealthCheckResults generates mock health check results for demonstration.
func generateMockHealthCheckResults() []*generated.HealthCheckResult {
	now := time.Now().Format(time.RFC3339)

	return []*generated.HealthCheckResult{
		{
			Type:                 generated.HealthCheckType_HEALTH_CHECK_TYPE_LIVENESS,
			Status:               generated.HealthStatus_HEALTH_STATUS_HEALTHY,
			Message:              "Health check passed",
			Timestamp:            now,
			ConsecutiveSuccesses: 5,
			ConsecutiveFailures:  0,
		},
		{
			Type:                 generated.HealthCheckType_HEALTH_CHECK_TYPE_READINESS,
			Status:               generated.HealthStatus_HEALTH_STATUS_HEALTHY,
			Message:              "Ready to serve traffic",
			Timestamp:            now,
			ConsecutiveSuccesses: 3,
			ConsecutiveFailures:  0,
		},
	}
}
