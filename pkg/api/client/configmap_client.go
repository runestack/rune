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

// ConfigmapClient provides methods for interacting with configs on the Rune API server.
type ConfigmapClient struct {
	client *Client
	logger log.Logger
	svc    generated.ConfigmapServiceClient
}

// NewConfigmapClient creates a new configmap client.
func NewConfigmapClient(client *Client) *ConfigmapClient {
	return &ConfigmapClient{
		client: client,
		logger: client.logger.WithComponent("configmap-client"),
		svc:    generated.NewConfigmapServiceClient(client.conn),
	}
}

// GetLogger returns the logger for this client
func (c *ConfigmapClient) GetLogger() log.Logger { return c.logger }

// CreateConfig creates a new configmap on the API server.
func (c *ConfigmapClient) CreateConfigmap(configmap *types.Configmap, ensureNamespace bool) error {
	c.logger.Debug("Creating configmap", log.Str("name", configmap.Name), log.Str("namespace", configmap.Namespace))

	req := &generated.CreateConfigmapRequest{
		Configmap:       c.configToProto(configmap),
		EnsureNamespace: ensureNamespace,
	}

	ctx, cancel := c.client.Context()
	defer cancel()

	resp, err := c.svc.CreateConfigmap(ctx, req)
	if err != nil {
		c.logger.Error("Failed to create configmap", log.Err(err), log.Str("name", configmap.Name))
		return convertGRPCError("create configmap", err)
	}
	if resp.Status != nil && resp.Status.Code != int32(codes.OK) {
		err := fmt.Errorf("API error: %s", resp.Status.Message)
		c.logger.Error("Failed to create configmap", log.Err(err), log.Str("name", configmap.Name))
		return err
	}
	return nil
}

// GetConfigmap retrieves a configmap by name.
func (c *ConfigmapClient) GetConfigmap(namespace, name string) (*types.Configmap, error) {
	c.logger.Debug("Getting configmap", log.Str("name", name), log.Str("namespace", namespace))

	req := &generated.GetConfigmapRequest{Name: name, Namespace: namespace}

	ctx, cancel := c.client.Context()
	defer cancel()

	resp, err := c.svc.GetConfigmap(ctx, req)
	if err != nil {
		statusErr, ok := status.FromError(err)
		if ok && statusErr.Code() == codes.NotFound {
			return nil, fmt.Errorf("configmap not found: %s/%s", namespace, name)
		}
		c.logger.Error("Failed to get configmap", log.Err(err), log.Str("name", name))
		return nil, convertGRPCError("get configmap", err)
	}
	if resp.Status != nil && resp.Status.Code != int32(codes.OK) {
		err := fmt.Errorf("API error: %s", resp.Status.Message)
		c.logger.Error("Failed to get configmap", log.Err(err), log.Str("name", name))
		return nil, err
	}

	cfg, err := c.protoToConfigmap(resp.Configmap)
	if err != nil {
		return nil, fmt.Errorf("failed to convert configmap: %w", err)
	}
	return cfg, nil
}

// UpdateConfigmap updates an existing configmap.
func (c *ConfigmapClient) UpdateConfigmap(configmap *types.Configmap) error {
	c.logger.Debug("Updating configmap", log.Str("name", configmap.Name), log.Str("namespace", configmap.Namespace))

	req := &generated.UpdateConfigmapRequest{Configmap: c.configToProto(configmap)}

	ctx, cancel := c.client.Context()
	defer cancel()

	resp, err := c.svc.UpdateConfigmap(ctx, req)
	if err != nil {
		c.logger.Error("Failed to update configmap", log.Err(err), log.Str("name", configmap.Name))
		return convertGRPCError("update configmap", err)
	}
	if resp.Status != nil && resp.Status.Code != int32(codes.OK) {
		err := fmt.Errorf("API error: %s", resp.Status.Message)
		c.logger.Error("Failed to update configmap", log.Err(err), log.Str("name", configmap.Name))
		return err
	}
	return nil
}

// DeleteConfigmap deletes a configmap.
func (c *ConfigmapClient) DeleteConfigmap(namespace, name string) error {
	c.logger.Debug("Deleting configmap", log.Str("name", name), log.Str("namespace", namespace))

	req := &generated.DeleteConfigmapRequest{Name: name, Namespace: namespace}

	ctx, cancel := c.client.Context()
	defer cancel()

	resp, err := c.svc.DeleteConfigmap(ctx, req)
	if err != nil {
		c.logger.Error("Failed to delete configmap", log.Err(err), log.Str("name", name))
		return convertGRPCError("delete configmap", err)
	}
	if resp.Code != int32(codes.OK) {
		return fmt.Errorf("API error: %s", resp.Message)
	}
	return nil
}

// ListConfigmaps lists configmaps in a namespace.
func (c *ConfigmapClient) ListConfigmaps(namespace string, labelSelector string, fieldSelector string) ([]*types.Configmap, error) {
	c.logger.Debug("Listing configmaps", log.Str("namespace", namespace))

	req := &generated.ListConfigmapsRequest{Namespace: namespace}

	ctx, cancel := c.client.Context()
	defer cancel()

	resp, err := c.svc.ListConfigmaps(ctx, req)
	if err != nil {
		c.logger.Error("Failed to list configs", log.Err(err), log.Str("namespace", namespace))
		return nil, convertGRPCError("list configs", err)
	}
	if resp.Status != nil && resp.Status.Code != int32(codes.OK) {
		err := fmt.Errorf("API error: %s", resp.Status.Message)
		c.logger.Error("Failed to list configs", log.Err(err), log.Str("namespace", namespace))
		return nil, err
	}

	configs := make([]*types.Configmap, 0, len(resp.Configmaps))
	for _, pc := range resp.Configmaps {
		cfg, err := c.protoToConfigmap(pc)
		if err != nil {
			c.logger.Warn("Failed to convert configmap", log.Err(err))
			continue
		}
		configs = append(configs, cfg)
	}
	// Apply client-side filtering
	filtered, err := c.filterConfigsBySelectors(configs, labelSelector, fieldSelector)
	if err != nil {
		return nil, err
	}
	return filtered, nil
}

// Converters
func (c *ConfigmapClient) configToProto(cfg *types.Configmap) *generated.Configmap {
	if cfg == nil {
		return nil
	}
	return &generated.Configmap{
		Name:      cfg.Name,
		Namespace: cfg.Namespace,
		Data:      cfg.Data,
	}
}

func (c *ConfigmapClient) protoToConfigmap(proto *generated.Configmap) (*types.Configmap, error) {
	if proto == nil {
		return nil, nil
	}
	createdAt, err := time.Parse(time.RFC3339, proto.CreatedAt)
	if err != nil {
		return nil, fmt.Errorf("failed to parse createdAt: %w", err)
	}
	updatedAt, err := time.Parse(time.RFC3339, proto.UpdatedAt)
	if err != nil {
		return nil, fmt.Errorf("failed to parse updatedAt: %w", err)
	}

	return &types.Configmap{
		Name:      proto.Name,
		Namespace: proto.Namespace,
		Data:      proto.Data,
		Version:   int(proto.Version),
		CreatedAt: createdAt,
		UpdatedAt: updatedAt,
	}, nil
}

// filterConfigsBySelectors applies client-side filtering for configs.
// Supported field selectors: name. Label selectors are not supported for configs in this build.
func (c *ConfigmapClient) filterConfigsBySelectors(configs []*types.Configmap, labelSelector, fieldSelector string) ([]*types.Configmap, error) {
	if labelSelector != "" {
		return nil, fmt.Errorf("label selector is not supported for configs")
	}
	fields, err := parseSelector(fieldSelector)
	if err != nil {
		return nil, fmt.Errorf("invalid field selector: %w", err)
	}
	var nameFilter string
	if v, ok := fields["name"]; ok {
		nameFilter = v
		delete(fields, "name")
	}
	if len(fields) > 0 {
		return nil, fmt.Errorf("unsupported field selector keys for configs: %v", fields)
	}
	if nameFilter == "" {
		return configs, nil
	}
	result := make([]*types.Configmap, 0, len(configs))
	for _, cfg := range configs {
		if cfg != nil && cfg.Name == nameFilter {
			result = append(result, cfg)
		}
	}
	return result, nil
}
