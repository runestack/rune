package server

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	grpc_middleware "github.com/grpc-ecosystem/go-grpc-middleware"
	"github.com/grpc-ecosystem/go-grpc-middleware/v2/interceptors/auth"
	grpc_recovery "github.com/grpc-ecosystem/go-grpc-middleware/v2/interceptors/recovery"
	grpc_validator "github.com/grpc-ecosystem/go-grpc-middleware/v2/interceptors/validator"
	"github.com/runestack/rune/pkg/api/generated"
	"github.com/runestack/rune/pkg/api/service"
	"github.com/runestack/rune/pkg/log"
	"github.com/runestack/rune/pkg/orchestrator"
	"github.com/runestack/rune/pkg/runner/manager"
	"github.com/runestack/rune/pkg/store"
	"github.com/runestack/rune/pkg/store/repos"
	"github.com/runestack/rune/pkg/types"
	"github.com/spf13/viper"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/reflection"
	"google.golang.org/grpc/status"
)

// APIServer represents the gRPC API server for Rune.
type APIServer struct {
	options *Options
	logger  log.Logger

	// Core services
	namespaceService    *service.NamespaceService
	serviceService      *service.ServiceService
	instanceService     *service.InstanceService
	logService          *service.LogService
	execService         *service.ExecService
	portForwardService  *service.PortForwardService
	healthService       *service.HealthService
	secretService       *service.SecretService
	configService       *service.ConfigmapService
	authService         *service.AuthService
	adminService        *service.AdminService
	auditService        *service.AuditService
	storageClassService *service.StorageClassService
	volumeService       *service.VolumeService
	snapshotService     *service.SnapshotService
	describeService     *service.DescribeService

	// gRPC server
	grpcServer *grpc.Server

	// HTTP server for REST gateway
	httpServer *http.Server

	// State store
	store store.Store

	// Orchestrator
	orchestrator orchestrator.Orchestrator

	// Shutdown channel
	shutdownCh chan struct{}

	// Wait group for server goroutines
	wg sync.WaitGroup

	// Ensures Stop is idempotent (signal handler + main may both call it).
	stopOnce sync.Once
	stopErr  error

	// Runner manager
	runnerManager *manager.RunnerManager
}

// New creates a new API server with the given options.
func New(opts ...Option) (*APIServer, error) {
	options := DefaultOptions()
	for _, opt := range opts {
		opt(options)
	}

	logger := options.Logger
	if logger == nil {
		logger = log.GetDefaultLogger().WithComponent("api-server")
	}

	runnerManager := options.RunnerManager
	if runnerManager == nil {
		runnerManager = manager.NewRunnerManager(logger)
	}

	// Initialize the basic server with options
	server := &APIServer{
		options:       options,
		logger:        logger,
		store:         options.Store,
		orchestrator:  options.Orchestrator,
		shutdownCh:    make(chan struct{}),
		runnerManager: runnerManager,
	}

	return server, nil
}

// Start starts the API server.
func (s *APIServer) Start() error {
	// Ensure we have required dependencies
	if s.store == nil {
		return fmt.Errorf("state store is required")
	}

	s.logger.Info("Starting Rune Server")

	// Seed built-in namespaces (idempotent)
	if err := SeedBuiltinNamespaces(context.Background(), s.store); err != nil {
		s.logger.Error("Failed to seed builtin namespaces", log.Err(err))
		return err
	}

	// Seed built-in policies (idempotent)
	if err := SeedBuiltinPolicies(context.Background(), s.store); err != nil {
		s.logger.Error("Failed to seed builtin policies", log.Err(err))
		return err
	}

	// Initialize the runner manager
	if err := s.runnerManager.Initialize(); err != nil {
		s.logger.Warn("Error initializing runners", log.Err(err))
	}

	// Initialize orchestrator if not provided
	if s.orchestrator == nil {
		var err error

		// Honour runefile-supplied storage driver configs when we
		// build the orchestrator internally; fall back to the
		// default constructor when none were supplied so existing
		// callers keep their previous behaviour.
		if len(s.options.StorageDriverConfigs) > 0 ||
			s.options.StorageDefaultStorageClass != nil ||
			s.options.StoragePreserveOnDelete ||
			s.options.StorageSecretLookup != nil ||
			s.options.InitialMountResolver != nil ||
			s.options.EventLog != nil {
			s.orchestrator, err = orchestrator.NewOrchestrator(orchestrator.OrchestratorOptions{
				Store:                   s.store,
				Logger:                  s.logger,
				RunnerManager:           s.runnerManager,
				StorageDriverConfigs:    s.options.StorageDriverConfigs,
				DefaultStorageClass:     s.options.StorageDefaultStorageClass,
				StoragePreserveOnDelete: s.options.StoragePreserveOnDelete,
				StorageSecretLookup:     s.options.StorageSecretLookup,
				InitialMountResolver:    s.options.InitialMountResolver,
				EventLog:                s.options.EventLog,
			})
		} else {
			s.orchestrator, err = orchestrator.NewDefaultOrchestrator(s.store, s.logger, s.runnerManager)
		}
		if err != nil {
			return fmt.Errorf("failed to create orchestrator: %w", err)
		}
	}

	// Start the orchestrator
	if err := s.orchestrator.Start(context.Background()); err != nil {
		return fmt.Errorf("failed to start orchestrator: %w", err)
	}

	// Create service implementations
	s.namespaceService = service.NewNamespaceService(s.store, s.logger)
	s.serviceService = service.NewServiceService(s.store, s.orchestrator, s.runnerManager, s.logger)
	s.instanceService = service.NewInstanceService(s.store, s.runnerManager, s.logger)
	s.logService = service.NewLogService(s.store, s.logger, s.orchestrator)
	s.execService = service.NewExecService(s.logger, s.orchestrator)
	s.portForwardService = service.NewPortForwardService(s.logger, s.orchestrator)
	s.healthService = service.NewHealthService(s.store, s.logger)
	s.secretService = service.NewSecretService(s.store, s.logger)
	s.configService = service.NewConfigmapService(s.store, s.logger)
	s.authService = service.NewAuthService(s.store, s.logger)
	s.adminService = service.NewAdminService(s.store, s.logger)
	s.auditService = service.NewAuditService(s.store, s.logger)
	s.storageClassService = service.NewStorageClassService(s.store, s.logger)
	s.volumeService = service.NewVolumeService(s.store, s.logger,
		service.WithDriverConfigs(s.options.StorageDriverConfigs))
	s.snapshotService = service.NewSnapshotService(s.store, s.logger,
		service.WithSnapshotDriverConfigs(s.options.StorageDriverConfigs))
	s.describeService = service.NewDescribeService(s.store, s.logger)

	if s.options.NetworkStatusProvider != nil {
		s.adminService.SetNetworkStatusProvider(s.options.NetworkStatusProvider)
	}
	if s.options.VIPAllocator != nil {
		s.serviceService.SetVIPAllocator(s.options.VIPAllocator)
	}

	// Start gRPC server
	if err := s.startGRPCServer(); err != nil {
		return fmt.Errorf("failed to start gRPC server: %w", err)
	}

	// SIGINT/SIGTERM are handled by cmd/runed (setupSignalContext) which
	// calls Stop() after ctx cancellation. Do not register a second
	// handler here — duplicate handlers race on GracefulStop and appear
	// to hang on Ctrl+C.

	return nil
}

// startGRPCServer starts the gRPC server.
func (s *APIServer) startGRPCServer() error {
	// Create listener
	lis, err := net.Listen("tcp", s.options.GRPCAddr)
	if err != nil {
		return fmt.Errorf("failed to listen on %s: %w", s.options.GRPCAddr, err)
	}

	// Set up server options
	var opts []grpc.ServerOption

	// Add TLS if enabled
	if s.options.EnableTLS {
		creds, err := credentials.NewServerTLSFromFile(s.options.TLSCertFile, s.options.TLSKeyFile)
		if err != nil {
			return fmt.Errorf("failed to load TLS credentials: %w", err)
		}
		opts = append(opts, grpc.Creds(creds))
	}

	// Add middleware
	opts = append(opts, grpc.UnaryInterceptor(grpc_middleware.ChainUnaryServer(
		s.logUnaryInterceptor(),
		s.authUnaryInterceptor(),
		s.adminUnaryInterceptor(),
		s.rbacUnaryInterceptor(),
		grpc_recovery.UnaryServerInterceptor(),
		grpc_validator.UnaryServerInterceptor(),
	)))

	opts = append(opts, grpc.StreamInterceptor(grpc_middleware.ChainStreamServer(
		s.logStreamInterceptor(),
		s.authStreamInterceptor(),
		s.rbacStreamInterceptor(),
		grpc_recovery.StreamServerInterceptor(),
		grpc_validator.StreamServerInterceptor(),
	)))

	// Create gRPC server
	s.grpcServer = grpc.NewServer(opts...)

	// Register services
	generated.RegisterServiceServiceServer(s.grpcServer, s.serviceService)
	generated.RegisterInstanceServiceServer(s.grpcServer, s.instanceService)
	generated.RegisterLogServiceServer(s.grpcServer, s.logService)
	generated.RegisterExecServiceServer(s.grpcServer, s.execService)
	generated.RegisterPortForwardServiceServer(s.grpcServer, s.portForwardService)
	generated.RegisterHealthServiceServer(s.grpcServer, s.healthService)
	generated.RegisterSecretServiceServer(s.grpcServer, s.secretService)
	generated.RegisterConfigmapServiceServer(s.grpcServer, s.configService)
	generated.RegisterAuthServiceServer(s.grpcServer, s.authService)
	generated.RegisterAdminServiceServer(s.grpcServer, s.adminService)
	generated.RegisterNamespaceServiceServer(s.grpcServer, s.namespaceService)
	generated.RegisterAuditServiceServer(s.grpcServer, s.auditService)
	generated.RegisterStorageClassServiceServer(s.grpcServer, s.storageClassService)
	generated.RegisterVolumeServiceServer(s.grpcServer, s.volumeService)
	generated.RegisterSnapshotServiceServer(s.grpcServer, s.snapshotService)
	generated.RegisterDescribeServiceServer(s.grpcServer, s.describeService)

	// Extra registrars (e.g. WatchService wired by runed for RUNE-028).
	for _, reg := range s.options.ExtraGRPCRegistrars {
		reg(s.grpcServer)
	}

	// Register reflection service for grpcurl/development
	reflection.Register(s.grpcServer)

	// Start server in a goroutine
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		s.logger.Info("Starting gRPC server", log.Str("address", s.options.GRPCAddr))
		if err := s.grpcServer.Serve(lis); err != nil {
			s.logger.Error("gRPC server error", log.Err(err))
		}
	}()

	return nil
}

// authUnaryInterceptor enforces authentication for unary RPCs, with bootstrap exception
func (s *APIServer) authUnaryInterceptor() grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req interface{}, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (interface{}, error) {
		if !s.options.EnableAuth {
			return handler(ctx, req)
		}
		// Allow unauthenticated bootstrap
		if info.FullMethod == "/rune.api.AdminService/AdminBootstrap" {
			return handler(ctx, req)
		}
		// Allow unauthenticated server version probe so `rune version`
		// can report server build info before the user has logged in.
		if info.FullMethod == "/rune.api.HealthService/GetServerVersion" {
			return handler(ctx, req)
		}
		// Otherwise, run normal auth
		ctx2, err := s.authFunc(ctx)
		if err != nil {
			return nil, err
		}
		return handler(ctx2, req)
	}
}

// authStreamInterceptor returns a stream interceptor for authentication.
func (s *APIServer) authStreamInterceptor() grpc.StreamServerInterceptor {
	return auth.StreamServerInterceptor(s.authFunc)
}

// rbac interceptors (policy-based)
func (s *APIServer) rbacUnaryInterceptor() grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req interface{}, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (interface{}, error) {
		if !s.options.EnableAuth {
			return handler(ctx, req)
		}

		// Require authenticated subject, except bootstrap which is already allowed above
		if info.FullMethod == "/rune.api.AdminService/AdminBootstrap" {
			return handler(ctx, req)
		}
		// Server version probe is read-only and intentionally public.
		if info.FullMethod == "/rune.api.HealthService/GetServerVersion" {
			return handler(ctx, req)
		}
		var subjectID string
		if v := ctx.Value(authCtxKey); v != nil {
			if ai, ok := v.(*AuthInfo); ok {
				subjectID = ai.SubjectID
			}
		}
		if subjectID == "" {
			return nil, statusPermissionDenied("unauthorized, subjectID is empty")
		}
		resource, verb := methodToAction(info.FullMethod)
		ns := extractNamespace(req)
		allowed, err := s.evaluatePolicies(ctx, subjectID, resource, verb, ns)
		if err != nil {
			return nil, status.Errorf(codes.Internal, "authorization error: %v", err)
		}
		if !allowed {
			return nil, statusPermissionDenied("access denied for resource: " + resource + " verb: " + verb)
		}
		// Per-request additional RBAC requirements (RUNE-073): some writes
		// carry a privileged side-effect (e.g. flipping a StorageClass to
		// Default:true) that we gate behind a separate verb so operators
		// can grant a `readwrite` token without also granting the
		// privileged action.
		for _, extra := range extraRBACRequirements(info.FullMethod, req) {
			ok, err := s.evaluatePolicies(ctx, subjectID, extra.resource, extra.verb, ns)
			if err != nil {
				return nil, status.Errorf(codes.Internal, "authorization error: %v", err)
			}
			if !ok {
				return nil, statusPermissionDenied("access denied for resource: " + extra.resource + " verb: " + extra.verb)
			}
		}
		return handler(ctx, req)
	}
}

// rbacRequirement is a (resource, verb) tuple representing an additional
// RBAC check the request must pass. See extraRBACRequirements.
type rbacRequirement struct{ resource, verb string }

// extraRBACRequirements returns extra (resource, verb) pairs that the
// authenticated subject must also be authorised for, on top of the
// default verb derived from the method name. Used to gate privileged
// payload-shaped operations (e.g. setting a StorageClass Default:true)
// without inventing a separate gRPC method.
func extraRBACRequirements(fullMethod string, req interface{}) []rbacRequirement {
	switch fullMethod {
	case "/rune.api.StorageClassService/CreateStorageClass":
		if r, ok := req.(*generated.CreateStorageClassRequest); ok && r.GetStorageClass().GetDefault() {
			return []rbacRequirement{{resource: "storageclasses", verb: "set-default"}}
		}
	case "/rune.api.StorageClassService/UpdateStorageClass":
		if r, ok := req.(*generated.UpdateStorageClassRequest); ok && r.GetStorageClass().GetDefault() {
			return []rbacRequirement{{resource: "storageclasses", verb: "set-default"}}
		}
	case "/rune.api.ServiceService/CreateService":
		if r, ok := req.(*generated.CreateServiceRequest); ok && servicePrivilegedRequired(r.GetService()) {
			return []rbacRequirement{{resource: "services", verb: "privileged"}}
		}
	case "/rune.api.ServiceService/UpdateService":
		if r, ok := req.(*generated.UpdateServiceRequest); ok && servicePrivilegedRequired(r.GetService()) {
			return []rbacRequirement{{resource: "services", verb: "privileged"}}
		}
	}
	return nil
}

// servicePrivilegedRequired reports whether the service payload uses
// security knobs that must be gated behind the services.privileged
// policy verb. Mirrors types.SecurityContext.RequiresPrivilegedGate.
func servicePrivilegedRequired(svc *generated.Service) bool {
	if svc == nil {
		return false
	}
	if securityContextNeedsGate(svc.SecurityContext) {
		return true
	}
	for _, step := range svc.InitSteps {
		if step != nil && securityContextNeedsGate(step.SecurityContext) {
			return true
		}
	}
	return false
}

func securityContextNeedsGate(sc *generated.SecurityContext) bool {
	if sc == nil {
		return false
	}
	if sc.Privileged {
		return true
	}
	// Normalize so e.g. k8s-style "Unconfined" gates the same as
	// our lowercase "unconfined". Mirrors types.SeccompProfileType.Canonical().
	if sp := sc.SeccompProfile; sp != nil && strings.EqualFold(sp.Type, "unconfined") {
		return true
	}
	return false
}

// admin interceptors (local-only)
func (s *APIServer) adminUnaryInterceptor() grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req interface{}, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (interface{}, error) {
		resource := methodToResource(info.FullMethod)
		if resource == "admin" && !viper.GetBool("auth.allow_remote_admin") {
			// If remote admin is allowed, skip the local admin check
			if p, ok := peerFromContext(ctx); ok {
				if !isLocalhost(p) {
					return nil, statusPermissionDenied("admin operations are allowed only from localhost unless auth.allow_remote_admin is true")
				}
			}

		}
		return handler(ctx, req)
	}
}

func (s *APIServer) rbacStreamInterceptor() grpc.StreamServerInterceptor {
	return func(srv interface{}, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		if !s.options.EnableAuth {
			return handler(srv, ss)
		}
		var subjectID string
		if v := ss.Context().Value(authCtxKey); v != nil {
			if ai, ok := v.(*AuthInfo); ok {
				subjectID = ai.SubjectID
			}
		}
		if subjectID == "" {
			return statusPermissionDenied("unauthorized")
		}
		resource, verb := methodToAction(info.FullMethod)
		allowed, err := s.evaluatePolicies(ss.Context(), subjectID, resource, verb, "")
		if err != nil {
			return status.Errorf(codes.Internal, "authorization error: %v", err)
		}
		if !allowed {
			return statusPermissionDenied("access denied for resource: " + resource + " verb: " + verb)
		}
		return handler(srv, ss)
	}
}

// evaluatePolicies loads the subject's policies and checks if any rule allows the action
func (s *APIServer) evaluatePolicies(ctx context.Context, subjectID, resource, verb, namespace string) (bool, error) {
	// Load user by ID (list and match) as we don't have GetByID
	var users []types.User
	if err := s.store.List(ctx, types.ResourceTypeUser, "system", &users); err != nil {
		return false, err
	}
	var user *types.User
	for i := range users {
		if users[i].ID == subjectID || users[i].Name == subjectID {
			user = &users[i]
			break
		}
	}
	if user == nil {
		return false, nil
	}
	// If no policies attached, deny by default
	if len(user.Policies) == 0 {
		return false, nil
	}
	pr := repos.NewPolicyRepo(s.store)
	for _, pname := range user.Policies {
		p, err := pr.Get(ctx, pname)
		if err != nil {
			continue
		}
		for _, rule := range p.Rules {
			if rule.Resource != "*" && rule.Resource != resource {
				continue
			}
			verbAllowed := false
			for _, v := range rule.Verbs {
				if v == "*" || v == verb {
					verbAllowed = true
					break
				}
			}
			if !verbAllowed {
				continue
			}
			// Namespace check: if rule.Namespace empty or "*", allow; if set, require match
			if rule.Namespace == "" || rule.Namespace == "*" || rule.Namespace == namespace {
				return true, nil
			}
		}
	}
	return false, nil
}

// Stop stops the API server gracefully.
func (s *APIServer) Stop() error {
	s.stopOnce.Do(func() {
		s.stopErr = s.stop()
	})
	return s.stopErr
}

func (s *APIServer) stop() error {
	s.logger.Info("Stopping Rune Server")

	// Stop the orchestrator first
	if s.orchestrator != nil {
		s.logger.Info("Stopping orchestrator")
		if err := s.orchestrator.Stop(); err != nil {
			s.logger.Error("Error stopping orchestrator", log.Err(err))
		}
	}

	// Ensure we only close the channel once
	select {
	case <-s.shutdownCh:
		// Channel is already closed, nothing to do
	default:
		close(s.shutdownCh)
	}

	// Stop gRPC server (GracefulStop can block forever on open streams).
	if s.grpcServer != nil {
		s.logger.Info("Stopping gRPC server")
		stopped := make(chan struct{})
		go func() {
			s.grpcServer.GracefulStop()
			close(stopped)
		}()
		select {
		case <-stopped:
		case <-time.After(5 * time.Second):
			s.logger.Warn("gRPC graceful stop timed out; forcing stop")
			s.grpcServer.Stop()
		}
	}

	// Stop HTTP server
	if s.httpServer != nil {
		s.logger.Info("Stopping REST gateway")
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := s.httpServer.Shutdown(ctx); err != nil {
			s.logger.Error("Error shutting down REST gateway", log.Err(err))
		}
	}

	// Wait for all goroutines to finish
	s.wg.Wait()
	s.logger.Info("Rune Server stopped")

	return nil
}

// logUnaryInterceptor returns a unary interceptor for logging.
func (s *APIServer) logUnaryInterceptor() grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req interface{}, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (interface{}, error) {
		start := time.Now()
		s.logger.Debug("gRPC request", log.Str("method", info.FullMethod))

		resp, err := handler(ctx, req)

		duration := time.Since(start)
		if err != nil {
			s.logger.Error("gRPC error",
				log.Str("method", info.FullMethod),
				log.Err(err),
				log.Duration("duration", duration))
		} else {
			s.logger.Debug("gRPC response",
				log.Str("method", info.FullMethod),
				log.Duration("duration", duration))
		}

		return resp, err
	}
}

// GetStore returns the store instance.
func (s *APIServer) GetStore() store.Store {
	return s.store
}

// GetOrchestrator returns the orchestrator instance. Used by runed
// to wire post-construction collaborators (e.g. RUNE-063 networking
// data plane endpoint publisher).
func (s *APIServer) GetOrchestrator() orchestrator.Orchestrator {
	return s.orchestrator
}

// GetRunnerManager returns the runner manager. Used by runed to
// wire post-construction collaborators that need to talk to the
// container runtime — notably DNS injection (RUNE-063): once the
// agent's embedded DNS subsystem is up, runed calls
// `runnerManager.SetDNSInjection([]string{"127.0.0.123"}, ...)`
// so every subsequently-created container is told to ask Rune's
// resolver for `<service>.<namespace>.rune` names.
func (s *APIServer) GetRunnerManager() *manager.RunnerManager {
	return s.runnerManager
}

// logStreamInterceptor returns a stream interceptor for logging.
func (s *APIServer) logStreamInterceptor() grpc.StreamServerInterceptor {
	return func(srv interface{}, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		start := time.Now()
		s.logger.Debug("gRPC stream request", log.Str("method", info.FullMethod))

		err := handler(srv, ss)

		duration := time.Since(start)
		if err != nil {
			s.logger.Error("gRPC stream error",
				log.Str("method", info.FullMethod),
				log.Err(err),
				log.Duration("duration", duration))
		} else {
			s.logger.Debug("gRPC stream complete",
				log.Str("method", info.FullMethod),
				log.Duration("duration", duration))
		}

		return err
	}
}
