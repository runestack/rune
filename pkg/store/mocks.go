package store

import (
	"context"

	"github.com/runestack/rune/pkg/types"
	"github.com/stretchr/testify/mock"
)

type MockStore struct {
	mock.Mock
}

func (m *MockStore) Open(dbPath string) error {
	args := m.Called(dbPath)
	return args.Error(0)
}

func (m *MockStore) Close() error {
	args := m.Called()
	return args.Error(0)
}

func (m *MockStore) Get(ctx context.Context, resourceType, namespace, name string, value interface{}) error {
	args := m.Called(ctx, resourceType, namespace, name, value)
	// This is tricky because we need to update the value parameter
	if args.Get(0) != nil {
		// Assuming we're dealing with a pointer to a type that can be type asserted
		if instance, ok := args.Get(0).(*types.Instance); ok && value != nil {
			if instancePtr, ok := value.(*types.Instance); ok {
				*instancePtr = *instance
			}
		}
		if service, ok := args.Get(0).(*types.Service); ok && value != nil {
			if servicePtr, ok := value.(*types.Service); ok {
				*servicePtr = *service
			}
		}
	}
	return args.Error(1)
}

func (m *MockStore) List(ctx context.Context, resourceType, namespace string) ([]interface{}, error) {
	args := m.Called(ctx, resourceType, namespace)
	return args.Get(0).([]interface{}), args.Error(1)
}

func (m *MockStore) Create(ctx context.Context, resourceType, namespace, name string, value interface{}) error {
	args := m.Called(ctx, resourceType, namespace, name, value)
	return args.Error(0)
}

func (m *MockStore) Update(ctx context.Context, resourceType, namespace, name string, value interface{}) error {
	args := m.Called(ctx, resourceType, namespace, name, value)
	return args.Error(0)
}

func (m *MockStore) Delete(ctx context.Context, resourceType, namespace, name string) error {
	args := m.Called(ctx, resourceType, namespace, name)
	return args.Error(0)
}

func (m *MockStore) GetHistory(ctx context.Context, resourceType, namespace, name string) ([]HistoricalVersion, error) {
	args := m.Called(ctx, resourceType, namespace, name)
	return args.Get(0).([]HistoricalVersion), args.Error(1)
}

func (m *MockStore) GetVersion(ctx context.Context, resourceType, namespace, name, version string) (interface{}, error) {
	args := m.Called(ctx, resourceType, namespace, name, version)
	return args.Get(0), args.Error(1)
}

func (m *MockStore) Transaction(ctx context.Context, fn func(ctx Transaction) error) error {
	args := m.Called(ctx, fn)
	return args.Error(0)
}

func (m *MockStore) Watch(ctx context.Context, resourceType, namespace string) (<-chan WatchEvent, error) {
	args := m.Called(ctx, resourceType, namespace)
	return args.Get(0).(<-chan WatchEvent), args.Error(1)
}
