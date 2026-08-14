package service

import (
	"context"
	"sync"

	"github.com/runestack/rune/pkg/api/generated"
	"github.com/stretchr/testify/mock"
	"google.golang.org/grpc"
)

// MockExecServiceStream is a mock implementation of the generated.ExecService_StreamExecServer interface
type MockExecServiceStream struct {
	mock.Mock
	grpc.ServerStream
	ctx context.Context

	// sendMu guards sends. StreamExec pumps stdout/stderr from separate
	// goroutines, so tests that want to wait for the stream to flush before
	// asserting must not read mock.Mock's call slice directly (that races the
	// pumps). SendCount exposes the tally safely instead.
	sendMu    sync.Mutex
	sendCount int
	sawStderr bool
	sawStdout bool
	sawExit   bool
}

// NewMockExecServiceStream creates a new MockExecServiceStream with the given context
func NewMockExecServiceStream(ctx context.Context) *MockExecServiceStream {
	return &MockExecServiceStream{
		ctx: ctx,
	}
}

// Send mocks the Send method
func (m *MockExecServiceStream) Send(resp *generated.ExecResponse) error {
	// Record the frame only AFTER m.Called has registered it with testify.
	// Marking it beforehand let a waiting test observe "stderr was sent"
	// while m.Called was still mid-flight, so AssertExpectations could run
	// against bookkeeping that had not caught up yet and report
	// "5 out of 6 expectation(s) were met".
	args := m.Called(resp)
	m.sendMu.Lock()
	m.sendCount++
	switch resp.GetResponse().(type) {
	case *generated.ExecResponse_Stdout:
		m.sawStdout = true
	case *generated.ExecResponse_Stderr:
		m.sawStderr = true
	case *generated.ExecResponse_Exit:
		m.sawExit = true
	}
	m.sendMu.Unlock()
	return args.Error(0)
}

// SendCount returns how many times Send has been called, safe to call from a
// different goroutine than the stream's pumps.
func (m *MockExecServiceStream) SendCount() int {
	m.sendMu.Lock()
	defer m.sendMu.Unlock()
	return m.sendCount
}

// SawStreams reports which response kinds have been sent so far. Tests use it
// to wait for the specific frames they assert on — StreamExec's stderr pump is
// best-effort (see ExecService.handleStderr) and can still be in flight when
// StreamExec returns, so waiting on a bare Send *count* is not enough.
func (m *MockExecServiceStream) SawStreams() (stdout, stderr, exit bool) {
	m.sendMu.Lock()
	defer m.sendMu.Unlock()
	return m.sawStdout, m.sawStderr, m.sawExit
}

// Recv mocks the Recv method
func (m *MockExecServiceStream) Recv() (*generated.ExecRequest, error) {
	args := m.Called()
	return args.Get(0).(*generated.ExecRequest), args.Error(1)
}

// Context returns the context
func (m *MockExecServiceStream) Context() context.Context {
	if m.ctx != nil {
		return m.ctx
	}
	return context.Background()
}
