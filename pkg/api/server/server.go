package server

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
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
	namespaceService *service.NamespaceService
	serviceService   *service.ServiceService
	instanceService  *service.InstanceService
	logService       *service.LogService
	execService      *service.ExecService
	healthService    *service.HealthService
	secretService    *service.SecretService
	configService    *service.ConfigmapService
	authService      *service.AuthService
	adminService     *service.AdminService

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

		// Use the default orchestrator creation which handles all component setup internally
		s.orchestrator, err = orchestrator.NewDefaultOrchestrator(s.store, s.logger, s.runnerManager)
		if err != nil {
			return fmt.Errorf("failed to create default orchestrator: %w", err)
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
	s.healthService = service.NewHealthService(s.store, s.logger)
	s.secretService = service.NewSecretService(s.store, s.logger)
	s.configService = service.NewConfigmapService(s.store, s.logger)
	s.authService = service.NewAuthService(s.store, s.logger)
	s.adminService = service.NewAdminService(s.store, s.logger)

	// Start gRPC server
	if err := s.startGRPCServer(); err != nil {
		return fmt.Errorf("failed to start gRPC server: %w", err)
	}

	// Handle signals for graceful shutdown
	go s.handleSignals()

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
	generated.RegisterHealthServiceServer(s.grpcServer, s.healthService)
	generated.RegisterSecretServiceServer(s.grpcServer, s.secretService)
	generated.RegisterConfigmapServiceServer(s.grpcServer, s.configService)
	generated.RegisterAuthServiceServer(s.grpcServer, s.authService)
	generated.RegisterAdminServiceServer(s.grpcServer, s.adminService)
	generated.RegisterNamespaceServiceServer(s.grpcServer, s.namespaceService)

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
		return handler(ctx, req)
	}
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

	// Stop gRPC server
	if s.grpcServer != nil {
		s.logger.Info("Stopping gRPC server")
		s.grpcServer.GracefulStop()
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

// handleSignals handles OS signals for graceful shutdown.
func (s *APIServer) handleSignals() {
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	select {
	case sig := <-sigCh:
		s.logger.Info("Received signal", log.Str("signal", sig.String()))
		_ = s.Stop()
	case <-s.shutdownCh:
		return
	}
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
