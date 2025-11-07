package helpers

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/runestack/rune/pkg/api/server"
	"github.com/runestack/rune/pkg/log"
	"github.com/runestack/rune/pkg/store"
	"github.com/runestack/rune/pkg/types"
	"github.com/stretchr/testify/require"
)

// StoreType defines the type of store to use in tests
type StoreType string

const (
	StoreTypeMemory StoreType = "memory"
	StoreTypeBadger StoreType = "badger"
)

// TestHelper provides utilities for integration testing
// Default mode uses the mock CLI (rune-test) for fast, deterministic tests.
// The helper-configured store (memory or badger) is used for verification and for any in-process server startup.
type TestHelper struct {
	t          *testing.T
	tempDir    string
	apiServer  *server.APIServer
	runeBinary string
	store      store.Store
	fixtureDir string
	storeType  StoreType
	memStore   *store.MemoryStore // Keep reference for helper methods

	// Real CLI integration mode (optional)
	realCLI  bool
	grpcAddr string
	httpAddr string
}

// NewTestHelper creates a new test helper with configurable store type
func NewTestHelper(t *testing.T) *TestHelper {
	// Check environment variable for store type
	storeType := StoreTypeBadger // Default to BadgerDB store (production-like)
	if envStoreType := os.Getenv("RUNE_TEST_STORE_TYPE"); envStoreType != "" {
		switch envStoreType {
		case "memory", "Memory", "MEMORY":
			storeType = StoreTypeMemory
		case "badger", "Badger", "BADGER":
			storeType = StoreTypeBadger
		default:
			t.Logf("Unknown store type '%s', defaulting to BadgerDB", envStoreType)
			storeType = StoreTypeBadger
		}
	}

	return NewTestHelperWithStore(t, storeType)
}

// NewTestHelperWithStore creates a new test helper with specified store type
func NewTestHelperWithStore(t *testing.T, storeType StoreType) *TestHelper {
	tempDir, err := os.MkdirTemp("", "rune-test-*")
	require.NoError(t, err)

	// Create fixture directory
	fixtureDir := filepath.Join(tempDir, "fixtures")
	err = os.MkdirAll(fixtureDir, 0755)
	require.NoError(t, err)

	// Determine integration mode
	realCLI := os.Getenv("RUNE_IT_MODE") == "real_cli"

	// Initialize store based on type
	var storeInstance store.Store
	var memStore *store.MemoryStore

	switch storeType {
	case StoreTypeMemory:
		memStore = store.NewMemoryStore()
		storeInstance = memStore
	case StoreTypeBadger:
		logger := log.GetDefaultLogger().WithComponent("test-badger-store")
		badgerStore := store.NewBadgerStore(logger)
		// Open store with temporary directory
		err = badgerStore.Open(filepath.Join(tempDir, "badger"))
		require.NoError(t, err)
		storeInstance = badgerStore
	default:
		t.Fatalf("Unknown store type: %s", storeType)
	}

	h := &TestHelper{
		t:          t,
		tempDir:    tempDir,
		fixtureDir: fixtureDir,
		store:      storeInstance,
		storeType:  storeType,
		memStore:   memStore,
		realCLI:    realCLI,
	}

	// Select binary based on mode
	if realCLI {
		// Use real rune CLI
		binPath := filepath.Join(getProjectRootOrCwd(t), "bin", "rune")
		if _, statErr := os.Stat(binPath); statErr != nil {
			cmd := exec.Command("go", "build", "-o", binPath, "github.com/runestack/rune/cmd/rune")
			cmd.Dir = getProjectRootOrCwd(t)
			if out, buildErr := cmd.CombinedOutput(); buildErr != nil {
				t.Fatalf("Failed to build rune CLI: %v\n%s", buildErr, string(out))
			}
		}
		h.runeBinary = binPath

		// Start a real API server on fixed localhost ports
		h.grpcAddr = "127.0.0.1:19080"
		h.httpAddr = "127.0.0.1:19081"
		h.startAPIServerWithAddrs(h.grpcAddr, h.httpAddr)
	} else {
		// Use rune-test mock CLI
		runeBinary := os.Getenv("RUNE_BINARY")
		if runeBinary == "" {
			// Look for rune-test binary in the project's bin directory first
			cwd, _ := os.Getwd()
			projectBin := filepath.Join(cwd, "bin", "rune-test")
			if _, statErr := os.Stat(projectBin); statErr == nil {
				runeBinary = projectBin
			} else {
				// Fall back to PATH lookup or build
				if p, lookErr := exec.LookPath("rune-test"); lookErr == nil {
					runeBinary = p
				} else {
					cmd := exec.Command("go", "build", "-o", filepath.Join(tempDir, "rune-test"), "github.com/runestack/rune/cmd/rune-test")
					if out, buildErr := cmd.CombinedOutput(); buildErr != nil {
						t.Fatalf("Failed to build test binary: %v\n%s", buildErr, string(out))
					}
					runeBinary = filepath.Join(tempDir, "rune-test")
				}
			}
		}
		h.runeBinary = runeBinary
	}

	return h
}

func getProjectRootOrCwd(t *testing.T) string {
	cwd, err := os.Getwd()
	if err != nil {
		return "."
	}
	// Walk up to find go.mod
	d := cwd
	for i := 0; i < 5; i++ {
		if _, statErr := os.Stat(filepath.Join(d, "go.mod")); statErr == nil {
			return d
		}
		d = filepath.Dir(d)
	}
	return cwd
}

// Cleanup removes temporary test resources
func (h *TestHelper) Cleanup() {
	if h.apiServer != nil {
		// Protect against double-closing the server
		server := h.apiServer
		h.apiServer = nil
		server.Stop()
	}
	os.RemoveAll(h.tempDir)
}

// StartAPIServer starts a test API server using the helper-configured store.
// Note: The mock CLI path (rune-test) does not use this server; it is provided
// for tests that want an in-process server for subsystem validation.
func (h *TestHelper) StartAPIServer() *server.APIServer {
	// Set log level to error for tests to suppress repetitive background controller logs
	log.SetDefaultLogger(log.NewLogger(log.WithLevel(log.ErrorLevel)))
	logger := log.GetDefaultLogger().WithComponent("test-api-server")

	// Ensure we have a store; honor the helper's chosen store type
	if h.store == nil {
		if h.storeType == StoreTypeMemory {
			h.memStore = store.NewMemoryStore()
			h.store = h.memStore
		} else {
			// Default to memory if not initialized; integration tests should normally initialize in constructor
			h.memStore = store.NewMemoryStore()
			h.store = h.memStore
		}
	}

	// Create API server options with the provided store
	opts := []server.Option{
		server.WithStore(h.store),
		server.WithLogger(logger),
		server.WithGRPCAddr(":0"), // Random port
		server.WithHTTPAddr(":0"), // Random port
	}

	apiServer, newErr := server.New(opts...)
	require.NoError(h.t, newErr)

	startErr := apiServer.Start()
	require.NoError(h.t, startErr)

	h.apiServer = apiServer

	return apiServer
}

// startAPIServerWithAddrs starts the API server on specific addresses (used for real CLI mode).
func (h *TestHelper) startAPIServerWithAddrs(grpcAddr, httpAddr string) {
	log.SetDefaultLogger(log.NewLogger(log.WithLevel(log.ErrorLevel)))
	logger := log.GetDefaultLogger().WithComponent("test-api-server")
	if h.store == nil {
		h.memStore = store.NewMemoryStore()
		h.store = h.memStore
	}
	opts := []server.Option{
		server.WithStore(h.store),
		server.WithLogger(logger),
		server.WithGRPCAddr(grpcAddr),
		server.WithHTTPAddr(httpAddr),
	}
	apiServer, newErr := server.New(opts...)
	require.NoError(h.t, newErr)
	require.NoError(h.t, apiServer.Start())
	h.apiServer = apiServer
}

// RunCommand executes a rune CLI command
func (h *TestHelper) RunCommand(args ...string) (string, error) {
	// Environment for the command
	env := append(os.Environ(), "RUNE_LOG_LEVEL=error")
	if !h.realCLI {
		// Only used by rune-test mock CLI
		env = append(env, fmt.Sprintf("RUNE_FIXTURE_DIR=%s", h.fixtureDir))
	}

	// Auto-inject API server address for real CLI when needed
	if h.realCLI && len(args) > 0 {
		sub := args[0]
		if sub == "cast" && !contains(args, "--api-server") {
			args = append([]string{sub}, append([]string{"--api-server", h.grpcAddr}, args[1:]...)...)
		}
	}

	cmd := exec.Command(h.runeBinary, args...)
	cmd.Env = env
	out, cmdErr := cmd.CombinedOutput()

	// Only load fixtures for mock CLI cast success
	if !h.realCLI && len(args) > 0 && args[0] == "cast" && cmdErr == nil {
		_ = h.loadFixtures(context.Background())
	}

	return string(out), cmdErr
}

// RunCommandWithTimeout executes a rune CLI command with a timeout
func (h *TestHelper) RunCommandWithTimeout(timeout time.Duration, args ...string) (string, error) {
	env := append(os.Environ(), "RUNE_LOG_LEVEL=error")
	if !h.realCLI {
		env = append(env, fmt.Sprintf("RUNE_FIXTURE_DIR=%s", h.fixtureDir))
	}

	// Auto-inject API server for real CLI
	if h.realCLI && len(args) > 0 {
		sub := args[0]
		if sub == "cast" && !contains(args, "--api-server") {
			args = append([]string{sub}, append([]string{"--api-server", h.grpcAddr}, args[1:]...)...)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, h.runeBinary, args...)
	cmd.Env = env
	out, cmdErr := cmd.CombinedOutput()

	if !h.realCLI && len(args) > 0 && args[0] == "cast" && cmdErr == nil {
		_ = h.loadFixtures(context.Background())
	}
	return string(out), cmdErr
}

func contains(slice []string, target string) bool {
	for _, s := range slice {
		if s == target {
			return true
		}
	}
	return false
}

// loadFixtures loads any fixture files in the fixture directory into the memory store
func (h *TestHelper) loadFixtures(ctx context.Context) error {
	if h.store == nil {
		return fmt.Errorf("store not initialized")
	}

	// Read fixture directory
	files, readDirErr := os.ReadDir(h.fixtureDir)
	if readDirErr != nil {
		return readDirErr
	}

	// Make sure service resource type exists in the store
	if h.memStore != nil {
		h.memStore.EnsureResourceType(types.ResourceTypeService)
	}

	for _, file := range files {
		if file.IsDir() || filepath.Ext(file.Name()) != ".json" {
			continue
		}

		// Read fixture file
		data, readFileErr := os.ReadFile(filepath.Join(h.fixtureDir, file.Name()))
		if readFileErr != nil {
			return readFileErr
		}

		// Parse service
		var serviceMap map[string]interface{}
		if unmarshalErr := json.Unmarshal(data, &serviceMap); unmarshalErr != nil {
			return unmarshalErr
		}

		// Convert to service type
		svc := &types.Service{}

		// Set basic fields
		if name, ok := serviceMap["name"].(string); ok {
			svc.Name = name
		}

		if namespace, ok := serviceMap["namespace"].(string); ok {
			svc.Namespace = namespace
		} else {
			svc.Namespace = "default"
		}

		// Make sure the namespace exists in the store
		if h.memStore != nil {
			h.memStore.EnsureNamespace(types.ResourceTypeService, svc.Namespace)
		}

		if runtime, ok := serviceMap["runtime"].(string); ok {
			svc.Runtime = types.RuntimeType(runtime)
		}

		if image, ok := serviceMap["image"].(string); ok {
			svc.Image = image
		}

		if command, ok := serviceMap["command"].([]interface{}); ok {
			svc.Command = command[0].(string)
			if len(command) > 1 {
				args := make([]string, len(command)-1)
				for i, arg := range command[1:] {
					args[i] = arg.(string)
				}
				svc.Args = args
			}
		}

		if scale, ok := serviceMap["scale"].(float64); ok {
			svc.Scale = int(scale)
		} else if scale, ok := serviceMap["scale"].(string); ok {
			// Try to parse the string
			var s int
			fmt.Sscanf(scale, "%d", &s)
			svc.Scale = s
		}

		// Set status to running by default
		svc.Status = types.ServiceStatusRunning

		// Store the service
		createErr := h.store.Create(ctx, types.ResourceTypeService, svc.Namespace, svc.Name, svc)
		if createErr != nil {
			// If it already exists, that's not a fatal error
			if !strings.Contains(createErr.Error(), "already exists") {
				return createErr
			}
		}
	}

	return nil
}

// GetService directly accesses the memory store to verify a service was created
func (h *TestHelper) GetService(ctx context.Context, namespace, name string) (*types.Service, error) {
	if h.store == nil {
		return nil, fmt.Errorf("store not initialized")
	}

	// First ensure the namespace exists
	if h.memStore != nil {
		h.memStore.EnsureResourceType(types.ResourceTypeService)
	}
	if h.memStore != nil {
		h.memStore.EnsureNamespace(types.ResourceTypeService, namespace)
	}

	var svc types.Service
	getErr := h.store.Get(ctx, types.ResourceTypeService, namespace, name, &svc)
	if getErr != nil {
		return nil, getErr
	}

	return &svc, nil
}

// CreateTestService creates a service directly in the store for testing
func (h *TestHelper) CreateTestService(ctx context.Context, svc *types.Service) error {
	if h.store == nil {
		return fmt.Errorf("store not initialized")
	}

	if svc.Namespace == "" {
		svc.Namespace = "default"
	}

	return h.store.Create(ctx, types.ResourceTypeService, svc.Namespace, svc.Name, svc)
}

// TempDir returns the temporary directory for this test
func (h *TestHelper) TempDir() string {
	return h.tempDir
}
