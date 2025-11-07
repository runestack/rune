package service

import (
	"context"

	"github.com/runestack/rune/pkg/api/generated"
	"github.com/stretchr/testify/mock"
	"google.golang.org/grpc"
)

// MockExecServiceStream is a mock implementation of the generated.ExecService_StreamExecServer interface
type MockExecServiceStream struct {
	mock.Mock
	grpc.ServerStream
	ctx context.Context
}

// NewMockExecServiceStream creates a new MockExecServiceStream with the given context
func NewMockExecServiceStream(ctx context.Context) *MockExecServiceStream {
	return &MockExecServiceStream{
		ctx: ctx,
	}
}

// Send mocks the Send method
func (m *MockExecServiceStream) Send(resp *generated.ExecResponse) error {
	args := m.Called(resp)
	return args.Error(0)
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
