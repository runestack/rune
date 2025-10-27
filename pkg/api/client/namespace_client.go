package client

import (
	"fmt"
	"time"

	"github.com/runestack/rune/pkg/api/generated"
	"github.com/runestack/rune/pkg/log"
	"github.com/runestack/rune/pkg/types"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// NamespaceClient provides methods for interacting with namespaces on the Rune API server.
type NamespaceClient struct {
	client *Client
	logger log.Logger
	ns     generated.NamespaceServiceClient
}

// NewNamespaceClient creates a new namespace client.
func NewNamespaceClient(client *Client) *NamespaceClient {
	return &NamespaceClient{
		client: client,
		logger: client.logger.WithComponent("namespace-client"),
		ns:     generated.NewNamespaceServiceClient(client.conn),
	}
}

// GetLogger returns the logger for this client
func (n *NamespaceClient) GetLogger() log.Logger {
	return n.logger
}

// CreateNamespace creates a new namespace on the API server.
func (n *NamespaceClient) CreateNamespace(namespace *types.Namespace) error {
	n.logger.Debug("Creating namespace", log.Str("name", namespace.Name))

	// Create the gRPC request
	req := &generated.CreateNamespaceRequest{
		Namespace: NamespaceToProto(namespace),
	}

	// Send the request to the API server
	ctx, cancel := n.client.Context()
	defer cancel()

	resp, err := n.ns.CreateNamespace(ctx, req)
	if err != nil {
		n.logger.Error("Failed to create namespace", log.Err(err), log.Str("name", namespace.Name))
		return convertGRPCError("create namespace", err)
	}

	// Check if the API returned an error status
	if resp.Status != nil && resp.Status.Code != int32(codes.OK) {
		err := fmt.Errorf("API error: %s", resp.Status.Message)
		n.logger.Error("Failed to create namespace", log.Err(err), log.Str("name", namespace.Name))
		return err
	}

	return nil
}

// GetNamespace retrieves a namespace by name.
func (n *NamespaceClient) GetNamespace(name string) (*types.Namespace, error) {
	n.logger.Debug("Getting namespace", log.Str("name", name))

	// Create the gRPC request
	req := &generated.GetNamespaceRequest{
		Name: name,
	}

	// Send the request to the API server
	ctx, cancel := n.client.Context()
	defer cancel()

	resp, err := n.ns.GetNamespace(ctx, req)
	if err != nil {
		statusErr, ok := status.FromError(err)
		if ok && statusErr.Code() == codes.NotFound {
			return nil, fmt.Errorf("namespace not found: %s", name)
		}
		n.logger.Error("Failed to get namespace", log.Err(err), log.Str("name", name))
		return nil, convertGRPCError("get namespace", err)
	}

	// Check if the API returned an error status
	if resp.Status != nil && resp.Status.Code != int32(codes.OK) {
		err := fmt.Errorf("API error: %s", resp.Status.Message)
		n.logger.Error("Failed to get namespace", log.Err(err), log.Str("name", name))
		return nil, err
	}

	// Convert the proto message to a namespace
	namespace, err := ProtoToNamespace(resp.Namespace)
	if err != nil {
		n.logger.Error("Failed to convert namespace proto", log.Err(err), log.Str("name", name))
		return nil, fmt.Errorf("failed to convert namespace proto: %w", err)
	}

	return namespace, nil
}

// ListNamespaces lists all namespaces with optional filtering.
func (n *NamespaceClient) ListNamespaces(labelSelector, fieldSelector map[string]string) ([]*types.Namespace, error) {
	n.logger.Debug("Listing namespaces")

	// Create the gRPC request
	req := &generated.ListNamespacesRequest{
		LabelSelector: labelSelector,
		FieldSelector: fieldSelector,
	}

	// Send the request to the API server
	ctx, cancel := n.client.Context()
	defer cancel()

	resp, err := n.ns.ListNamespaces(ctx, req)
	if err != nil {
		n.logger.Error("Failed to list namespaces", log.Err(err))
		return nil, convertGRPCError("list namespaces", err)
	}

	// Check if the API returned an error status
	if resp.Status != nil && resp.Status.Code != int32(codes.OK) {
		err := fmt.Errorf("API error: %s", resp.Status.Message)
		n.logger.Error("Failed to list namespaces", log.Err(err))
		return nil, err
	}

	// Convert the proto messages to namespaces
	namespaces := make([]*types.Namespace, 0, len(resp.Namespaces))
	for _, protoNamespace := range resp.Namespaces {
		namespace, err := ProtoToNamespace(protoNamespace)
		if err != nil {
			n.logger.Error("Failed to convert namespace proto", log.Err(err))
			continue
		}
		namespaces = append(namespaces, namespace)
	}

	return namespaces, nil
}

// DeleteNamespace deletes a namespace by name.
func (n *NamespaceClient) DeleteNamespace(name string, ignoreNotFound, force bool) error {
	n.logger.Debug("Deleting namespace", log.Str("name", name))

	// Create the gRPC request
	req := &generated.DeleteNamespaceRequest{
		Name:           name,
		IgnoreNotFound: ignoreNotFound,
		Force:          force,
	}

	// Send the request to the API server
	ctx, cancel := n.client.Context()
	defer cancel()

	resp, err := n.ns.DeleteNamespace(ctx, req)
	if err != nil {
		n.logger.Error("Failed to delete namespace", log.Err(err), log.Str("name", name))
		return convertGRPCError("delete namespace", err)
	}

	// Check if the API returned an error status
	if resp.Status != nil && resp.Status.Code != int32(codes.OK) {
		err := fmt.Errorf("API error: %s", resp.Status.Message)
		n.logger.Error("Failed to delete namespace", log.Err(err), log.Str("name", name))
		return err
	}

	return nil
}

// WatchNamespaces watches for namespace changes.
func (n *NamespaceClient) WatchNamespaces(labelSelector, fieldSelector map[string]string) (<-chan *types.Namespace, error) {
	n.logger.Debug("Watching namespaces")

	// Create the gRPC request
	req := &generated.WatchNamespacesRequest{
		LabelSelector: labelSelector,
		FieldSelector: fieldSelector,
	}

	// Send the request to the API server
	ctx, cancel := n.client.Context()
	defer cancel()

	stream, err := n.ns.WatchNamespaces(ctx, req)
	if err != nil {
		n.logger.Error("Failed to watch namespaces", log.Err(err))
		return nil, convertGRPCError("watch namespaces", err)
	}

	// Create a channel for the watch events
	watchCh := make(chan *types.Namespace, 100)

	// Start a goroutine to handle the stream
	go func() {
		defer close(watchCh)
		defer cancel()

		for {
			resp, err := stream.Recv()
			if err != nil {
				n.logger.Error("Failed to receive watch event", log.Err(err))
				return
			}

			// Convert the proto message to a namespace
			namespace, err := ProtoToNamespace(resp.Namespace)
			if err != nil {
				n.logger.Error("Failed to convert namespace proto", log.Err(err))
				continue
			}

			// Send the namespace to the channel
			select {
			case watchCh <- namespace:
			case <-ctx.Done():
				return
			}
		}
	}()

	return watchCh, nil
}

// NamespaceToProto converts a domain namespace to a protobuf message.
func NamespaceToProto(namespace *types.Namespace) *generated.Namespace {
	if namespace == nil {
		return nil
	}

	proto := &generated.Namespace{
		Id:          namespace.ID,
		Name:        namespace.Name,
		Description: namespace.Description,
		Labels:      namespace.Labels,
		CreatedAt:   namespace.CreatedAt.Unix(),
		UpdatedAt:   namespace.UpdatedAt.Unix(),
	}

	return proto
}

// ProtoToNamespace converts a protobuf message to a domain namespace.
func ProtoToNamespace(proto *generated.Namespace) (*types.Namespace, error) {
	if proto == nil {
		return nil, fmt.Errorf("proto namespace is nil")
	}

	namespace := &types.Namespace{
		ID:          proto.Id,
		Name:        proto.Name,
		Description: proto.Description,
		Labels:      proto.Labels,
		CreatedAt:   time.Unix(proto.CreatedAt, 0),
		UpdatedAt:   time.Unix(proto.UpdatedAt, 0),
	}

	return namespace, nil
}
