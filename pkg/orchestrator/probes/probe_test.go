package probes

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/runestack/rune/pkg/log"
	"github.com/runestack/rune/pkg/runner"
	"github.com/runestack/rune/pkg/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
)

// MockRunnerProvider implements runner.RunnerProvider for testing
type MockRunnerProvider struct {
	mock.Mock
}

func (m *MockRunnerProvider) GetInstanceRunner(instance *types.Instance) (runner.Runner, error) {
	args := m.Called(instance)
	return args.Get(0).(runner.Runner), args.Error(1)
}

// MockReadCloser implements the ReadCloser interface
type MockReadCloser struct {
	Reader io.ReadCloser
}

func (m *MockReadCloser) Read(p []byte) (n int, err error) {
	return m.Reader.Read(p)
}

func (m *MockReadCloser) Close() error {
	return m.Reader.Close()
}

func TestNewProber(t *testing.T) {
	tests := []struct {
		name      string
		probeType string
		wantErr   bool
		wantType  interface{}
	}{
		{"HTTP Probe", "http", false, &HTTPProber{}},
		{"TCP Probe", "tcp", false, &TCPProber{}},
		{"Exec Probe", "exec", false, &ExecProber{}},
		{"Unknown Probe", "unknown", true, nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			prober, err := NewProber(tt.probeType)

			if tt.wantErr {
				assert.Error(t, err)
				assert.Nil(t, prober)
			} else {
				assert.NoError(t, err)
				assert.IsType(t, tt.wantType, prober)
			}
		})
	}
}

func TestHTTPProber_Execute(t *testing.T) {
	// Create a test server
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			w.WriteHeader(http.StatusOK)
		} else if r.URL.Path == "/fail" {
			w.WriteHeader(http.StatusInternalServerError)
		} else {
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	tests := []struct {
		name        string
		path        string
		wantSuccess bool
	}{
		{"Successful HTTP Check", "/health", true},
		{"Error HTTP Check", "/fail", false},
		{"Not Found HTTP Check", "/notfound", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			prober := &HTTPProber{}
			ctx := &ProbeContext{
				Ctx:      context.Background(),
				Logger:   log.NewLogger(),
				Instance: &types.Instance{},
				ProbeConfig: &types.Probe{
					Type: "http",
					Path: tt.path,
					Port: 8080, // Will be overridden by the test server port
				},
				HTTPClient: &http.Client{
					Transport: &http.Transport{
						Proxy: func(req *http.Request) (*url.URL, error) {
							// Redirect all localhost requests to the test server
							return url.Parse(server.URL)
						},
					},
				},
			}

			result := prober.Execute(ctx)
			assert.Equal(t, tt.wantSuccess, result.Success)
		})
	}
}

// TestTCPProber_Execute tests the TCP prober implementation
func TestTCPProber_Execute(t *testing.T) {
	// Skip detailed test implementation for TCP
	// In a real test, we'd set up an actual TCP server to test against
	t.Skip("TCP probe tests would require setting up a real TCP server")
}

func TestExecProber_Execute(t *testing.T) {
	tests := []struct {
		name        string
		command     []string
		exitCode    int
		wantSuccess bool
		wantErrMsg  string
	}{
		{
			name:        "No Command Specified",
			command:     []string{},
			exitCode:    0, // Not used in this case
			wantSuccess: false,
			wantErrMsg:  "no command specified",
		},
		{
			name:        "Success Exit Code 0",
			command:     []string{"test"},
			exitCode:    0,
			wantSuccess: true,
			wantErrMsg:  "",
		},
		{
			name:        "Failure Exit Code 1",
			command:     []string{"test"},
			exitCode:    1,
			wantSuccess: false,
			wantErrMsg:  "exit code 1",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create a mock provider for this test
			mockRunnerProvider := new(MockRunnerProvider)

			// Create a TestRunner with our desired exit code and empty output
			// Empty output ensures ExitCode() won't error because we didn't read all the output
			testRunner := runner.NewTestRunner()
			testRunner.ExitCodeVal = tt.exitCode
			testRunner.ExecOutput = []byte{} // Empty output so ExitCode() won't complain

			// Configure the provider to return our test runner
			mockRunnerProvider.On("GetInstanceRunner", mock.Anything).Return(testRunner, nil).Maybe()

			// Create a simplified mock ProbeContext
			probeCtx := &ProbeContext{
				Ctx:      context.Background(),
				Logger:   log.NewLogger(),
				Instance: &types.Instance{},
				ProbeConfig: &types.Probe{
					Type:    "exec",
					Command: tt.command,
				},
				RunnerProvider: mockRunnerProvider,
			}

			// Execute the probe
			prober := &ExecProber{}
			result := prober.Execute(probeCtx)

			assert.Equal(t, tt.wantSuccess, result.Success)

			if tt.wantErrMsg != "" {
				assert.Contains(t, result.Message, tt.wantErrMsg)
			}

			// Verify mocks were called as expected
			if len(tt.command) > 0 {
				mockRunnerProvider.AssertExpectations(t)

				// Verify that Exec was called on the test runner
				assert.Greater(t, len(testRunner.ExecCalls), 0, "TestRunner.Exec should have been called")
			}
		})
	}
}
