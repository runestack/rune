package service

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/runestack/rune/pkg/api/client"
	"github.com/runestack/rune/pkg/api/generated"
	"github.com/runestack/rune/pkg/log"
	"github.com/runestack/rune/pkg/store"
	"github.com/runestack/rune/pkg/store/repos"
	"github.com/runestack/rune/pkg/types"
	"github.com/runestack/rune/pkg/utils"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// NamespaceService implements the gRPC NamespaceService.
type NamespaceService struct {
	generated.UnimplementedNamespaceServiceServer

	repo   *repos.NamespaceRepo
	logger log.Logger
}

// NewNamespaceService creates a new NamespaceService with the given store and logger.
func NewNamespaceService(store store.Store, logger log.Logger) *NamespaceService {
	return &NamespaceService{
		repo:   repos.NewNamespaceRepo(store),
		logger: logger,
	}
}

// CreateNamespace creates a new namespace.
func (s *NamespaceService) CreateNamespace(ctx context.Context, req *generated.CreateNamespaceRequest) (*generated.CreateNamespaceResponse, error) {
	s.logger.Debug("CreateNamespace called")

	if req.Namespace == nil {
		return nil, status.Error(codes.InvalidArgument, "namespace is required")
	}

	// Convert protobuf message to domain model
	namespace, err := client.ProtoToNamespace(req.Namespace)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid namespace: %v", err)
	}

	// Use repo to create the namespace
	if err := s.repo.Create(ctx, namespace); err != nil {
		s.logger.Error("Failed to create namespace", log.Err(err))
		return nil, status.Errorf(codes.Internal, "failed to create namespace: %v", err)
	}

	// Convert back to protobuf message
	return &generated.CreateNamespaceResponse{
		Namespace: client.NamespaceToProto(namespace),
		Status: &generated.Status{
			Code:    int32(codes.OK),
			Message: "Namespace created successfully",
		},
	}, nil
}

// GetNamespace retrieves a namespace by name.
func (s *NamespaceService) GetNamespace(ctx context.Context, req *generated.GetNamespaceRequest) (*generated.GetNamespaceResponse, error) {
	s.logger.Debug("GetNamespace called", log.Str("name", req.Name))

	if req.Name == "" {
		return nil, status.Error(codes.InvalidArgument, "namespace name is required")
	}

	// Get the namespace from repo
	namespace, err := s.repo.Get(ctx, req.Name)
	if err != nil {
		if store.IsNotFoundError(err) {
			return nil, status.Errorf(codes.NotFound, "namespace not found: %s", req.Name)
		}
		s.logger.Error("Failed to get namespace", log.Err(err))
		return nil, status.Errorf(codes.Internal, "failed to get namespace: %v", err)
	}

	// Convert to protobuf message
	return &generated.GetNamespaceResponse{
		Namespace: client.NamespaceToProto(namespace),
		Status: &generated.Status{
			Code:    int32(codes.OK),
			Message: "Namespace retrieved successfully",
		},
	}, nil
}

// ListNamespaces lists namespaces with optional filtering.
func (s *NamespaceService) ListNamespaces(ctx context.Context, req *generated.ListNamespacesRequest) (*generated.ListNamespacesResponse, error) {
	s.logger.Debug("ListNamespaces called")

	// Get namespaces from repo
	namespaces, err := s.repo.List(ctx)
	if err != nil {
		s.logger.Error("Failed to list namespaces", log.Err(err))
		return nil, status.Errorf(codes.Internal, "failed to list namespaces: %v", err)
	}

	s.logger.Debug("Found namespaces", log.Int("count", len(namespaces)))

	// Convert to protobuf messages and apply filtering
	protoNamespaces := make([]*generated.Namespace, 0, len(namespaces))
	for _, namespace := range namespaces {
		// Apply selector filtering (both labels and fields)
		if !matchNamespaceSelectors(namespace, req.LabelSelector, req.FieldSelector) {
			continue
		}

		protoNamespace := client.NamespaceToProto(namespace)
		protoNamespaces = append(protoNamespaces, protoNamespace)
	}

	s.logger.Debug("Found proto namespaces", log.Int("count", len(protoNamespaces)))

	return &generated.ListNamespacesResponse{
		Namespaces: protoNamespaces,
		Status: &generated.Status{
			Code:    int32(codes.OK),
			Message: fmt.Sprintf("Found %d namespaces", len(protoNamespaces)),
		},
		Paging: &generated.PagingParams{
			Limit:  utils.ToInt32NonNegative(len(protoNamespaces)),
			Offset: 0,
		},
	}, nil
}

// DeleteNamespace removes a namespace.
func (s *NamespaceService) DeleteNamespace(ctx context.Context, req *generated.DeleteNamespaceRequest) (*generated.DeleteNamespaceResponse, error) {
	s.logger.Debug("DeleteNamespace called", log.Str("name", req.Name))

	if req.Name == "" {
		return nil, status.Error(codes.InvalidArgument, "namespace name is required")
	}

	// Check if the namespace exists (unless ignore_not_found is set)
	if !req.IgnoreNotFound {
		_, err := s.repo.Get(ctx, req.Name)
		if err != nil {
			if store.IsNotFoundError(err) {
				return nil, status.Errorf(codes.NotFound, "namespace not found: %s", req.Name)
			}
			s.logger.Error("Failed to get namespace", log.Err(err))
			return nil, status.Errorf(codes.Internal, "failed to get namespace: %v", err)
		}
	}

	// Use repo to delete the namespace
	if err := s.repo.Delete(ctx, req.Name); err != nil {
		s.logger.Error("Failed to delete namespace", log.Err(err))
		return nil, status.Errorf(codes.Internal, "failed to delete namespace: %v", err)
	}

	return &generated.DeleteNamespaceResponse{
		Status: &generated.Status{
			Code:    int32(codes.OK),
			Message: "Namespace deleted successfully",
		},
	}, nil
}

// WatchNamespaces watches namespaces for changes.
func (s *NamespaceService) WatchNamespaces(req *generated.WatchNamespacesRequest, stream generated.NamespaceService_WatchNamespacesServer) error {
	s.logger.Debug("WatchNamespaces called")

	ctx := stream.Context()

	// Start watching for namespace changes from repo
	watchCh, err := s.repo.Watch(ctx)
	if err != nil {
		s.logger.Error("Failed to watch namespaces", log.Err(err))
		return status.Errorf(codes.Internal, "failed to watch namespaces: %v", err)
	}

	// Initialize with current namespaces (simulating ADDED events for all existing namespaces)
	namespaces, err := s.repo.List(ctx)
	if err != nil {
		s.logger.Error("Failed to list namespaces", log.Err(err))
		return status.Errorf(codes.Internal, "failed to list initial namespaces: %v", err)
	}

	// Send all existing namespaces as ADDED events
	for _, namespace := range namespaces {
		// Apply selector filtering
		if !matchNamespaceSelectors(namespace, req.LabelSelector, req.FieldSelector) {
			continue
		}

		// Send to client
		err = stream.Send(&generated.WatchNamespacesResponse{
			Namespace: client.NamespaceToProto(namespace),
			EventType: generated.EventType_EVENT_TYPE_ADDED,
			Status:    &generated.Status{Code: int32(codes.OK)},
		})
		if err != nil {
			s.logger.Error("Failed to send initial namespace", log.Err(err))
			return status.Errorf(codes.Internal, "failed to send initial namespace: %v", err)
		}
	}

	// Watch loop - continue until client disconnects or context is cancelled
	for {
		select {
		case <-ctx.Done():
			s.logger.Debug("Watch context cancelled")
			return nil

		case event, ok := <-watchCh:
			if !ok {
				s.logger.Debug("Watch channel closed")
				return nil
			}

			// Only handle events for namespaces
			if event.ResourceType != types.ResourceTypeNamespace {
				continue
			}

			// Convert to typed namespace
			var namespace types.Namespace
			if typedNamespace, ok := event.Resource.(*types.Namespace); ok {
				namespace = *typedNamespace
			} else {
				// Use JSON marshaling/unmarshaling for conversion
				rawData, err := json.Marshal(event.Resource)
				if err != nil {
					s.logger.Warn("Failed to marshal namespace data", log.Err(err))
					continue
				}

				if err := json.Unmarshal(rawData, &namespace); err != nil {
					s.logger.Warn("Failed to unmarshal namespace data", log.Err(err))
					continue
				}
			}

			// Apply selector filtering
			if !matchNamespaceSelectors(&namespace, req.LabelSelector, req.FieldSelector) {
				continue
			}

			// Map store event type to proto event type
			var eventType generated.EventType
			switch event.Type {
			case store.WatchEventCreated:
				eventType = generated.EventType_EVENT_TYPE_ADDED
			case store.WatchEventUpdated:
				eventType = generated.EventType_EVENT_TYPE_MODIFIED
			case store.WatchEventDeleted:
				eventType = generated.EventType_EVENT_TYPE_DELETED
			default:
				eventType = generated.EventType_EVENT_TYPE_UNSPECIFIED
			}

			// Send to client
			err = stream.Send(&generated.WatchNamespacesResponse{
				Namespace: client.NamespaceToProto(&namespace),
				EventType: eventType,
				Status:    &generated.Status{Code: int32(codes.OK)},
			})
			if err != nil {
				s.logger.Error("Failed to send watch event", log.Err(err))
				return status.Errorf(codes.Internal, "failed to send watch event: %v", err)
			}
		}
	}
}

// matchNamespaceSelectors checks if namespace matches all the labels and fields in the selectors
func matchNamespaceSelectors(namespace *types.Namespace, labels map[string]string, fields map[string]string) bool {
	// Check label selectors
	if len(labels) > 0 {
		// Check if the namespace has all the requested labels
		for key, value := range labels {
			// If namespace has no labels or the specific label is not found
			if namespace.Labels == nil {
				return false
			}

			namespaceValue, exists := namespace.Labels[key]
			if !exists || namespaceValue != value {
				return false
			}
		}
	}

	// Check field selectors
	for k, v := range fields {
		switch k {
		case "name":
			if namespace.Name != v {
				return false
			}
		default:
			// Unknown field, consider it a non-match
			return false
		}
	}

	// Match if we passed all checks
	return true
}
