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

// SecretClient provides methods for interacting with secrets on the Rune API server.
type SecretClient struct {
	client *Client
	logger log.Logger
	svc    generated.SecretServiceClient
}

// NewSecretClient creates a new secret client.
func NewSecretClient(client *Client) *SecretClient {
	return &SecretClient{
		client: client,
		logger: client.logger.WithComponent("secret-client"),
		svc:    generated.NewSecretServiceClient(client.conn),
	}
}

// GetLogger returns the logger for this client
func (s *SecretClient) GetLogger() log.Logger {
	return s.logger
}

// CreateSecret creates a new secret on the API server.
func (s *SecretClient) CreateSecret(secret *types.Secret, ensureNamespace bool) error {
	s.logger.Debug("Creating secret", log.Str("name", secret.Name), log.Str("namespace", secret.Namespace))

	// Create the gRPC request
	req := &generated.CreateSecretRequest{
		Secret:          s.secretToProto(secret),
		EnsureNamespace: ensureNamespace,
	}

	// Send the request to the API server
	ctx, cancel := s.client.Context()
	defer cancel()

	resp, err := s.svc.CreateSecret(ctx, req)
	if err != nil {
		s.logger.Error("Failed to create secret", log.Err(err), log.Str("name", secret.Name))
		return convertGRPCError("create secret", err)
	}

	// Check if the API returned an error status
	if resp.Status != nil && resp.Status.Code != int32(codes.OK) {
		err := fmt.Errorf("API error: %s", resp.Status.Message)
		s.logger.Error("Failed to create secret", log.Err(err), log.Str("name", secret.Name))
		return err
	}

	return nil
}

// GetSecret retrieves a secret by name.
func (s *SecretClient) GetSecret(namespace, name string) (*types.Secret, error) {
	s.logger.Debug("Getting secret", log.Str("name", name), log.Str("namespace", namespace))

	// Create the gRPC request
	req := &generated.GetSecretRequest{
		Name:      name,
		Namespace: namespace,
	}

	// Send the request to the API server
	ctx, cancel := s.client.Context()
	defer cancel()

	resp, err := s.svc.GetSecret(ctx, req)
	if err != nil {
		statusErr, ok := status.FromError(err)
		if ok && statusErr.Code() == codes.NotFound {
			return nil, fmt.Errorf("secret not found: %s/%s", namespace, name)
		}
		s.logger.Error("Failed to get secret", log.Err(err), log.Str("name", name))
		return nil, convertGRPCError("get secret", err)
	}

	// Check if the API returned an error status
	if resp.Status != nil && resp.Status.Code != int32(codes.OK) {
		err := fmt.Errorf("API error: %s", resp.Status.Message)
		s.logger.Error("Failed to get secret", log.Err(err), log.Str("name", name))
		return nil, err
	}

	// Convert the proto message to a secret
	secret, err := s.protoToSecret(resp.Secret)
	if err != nil {
		return nil, fmt.Errorf("failed to convert secret: %w", err)
	}

	return secret, nil
}

// UpdateSecret updates an existing secret.
func (s *SecretClient) UpdateSecret(secret *types.Secret, force bool) error {
	s.logger.Debug("Updating secret", log.Str("name", secret.Name), log.Str("namespace", secret.Namespace))

	// Create the gRPC request
	req := &generated.UpdateSecretRequest{
		Secret: s.secretToProto(secret),
	}

	// Send the request to the API server
	ctx, cancel := s.client.Context()
	defer cancel()

	resp, err := s.svc.UpdateSecret(ctx, req)
	if err != nil {
		s.logger.Error("Failed to update secret", log.Err(err), log.Str("name", secret.Name))
		return convertGRPCError("update secret", err)
	}

	// Check if the API returned an error status
	if resp.Status != nil && resp.Status.Code != int32(codes.OK) {
		err := fmt.Errorf("API error: %s", resp.Status.Message)
		s.logger.Error("Failed to update secret", log.Err(err), log.Str("name", secret.Name))
		return err
	}

	return nil
}

// DeleteSecret deletes a secret.
func (s *SecretClient) DeleteSecret(namespace, name string) error {
	s.logger.Debug("Deleting secret", log.Str("name", name), log.Str("namespace", namespace))

	// Create the gRPC request
	req := &generated.DeleteSecretRequest{
		Name:      name,
		Namespace: namespace,
	}

	// Send the request to the API server
	ctx, cancel := s.client.Context()
	defer cancel()

	resp, err := s.svc.DeleteSecret(ctx, req)
	if err != nil {
		s.logger.Error("Failed to delete secret", log.Err(err), log.Str("name", name))
		return convertGRPCError("delete secret", err)
	}

	// Check if the API returned an error status
	if resp.Code != int32(codes.OK) {
		err := fmt.Errorf("API error: %s", resp.Message)
		s.logger.Error("Failed to delete secret", log.Err(err), log.Str("name", name))
		return err
	}

	return nil
}

// ListSecrets lists secrets in a namespace.
func (s *SecretClient) ListSecrets(namespace string, labelSelector string, fieldSelector string) ([]*types.Secret, error) {
	s.logger.Debug("Listing secrets", log.Str("namespace", namespace))

	// Create the gRPC request
	req := &generated.ListSecretsRequest{Namespace: namespace}

	// Send the request to the API server
	ctx, cancel := s.client.Context()
	defer cancel()

	resp, err := s.svc.ListSecrets(ctx, req)
	if err != nil {
		s.logger.Error("Failed to list secrets", log.Err(err), log.Str("namespace", namespace))
		return nil, convertGRPCError("list secrets", err)
	}

	// Check if the API returned an error status
	if resp.Status != nil && resp.Status.Code != int32(codes.OK) {
		err := fmt.Errorf("API error: %s", resp.Status.Message)
		s.logger.Error("Failed to list secrets", log.Err(err), log.Str("namespace", namespace))
		return nil, err
	}

	// Convert the proto messages to secrets
	secrets := make([]*types.Secret, 0, len(resp.Secrets))
	for _, protoSecret := range resp.Secrets {
		secret, err := s.protoToSecret(protoSecret)
		if err != nil {
			s.logger.Warn("Failed to convert secret", log.Err(err))
			continue
		}
		secrets = append(secrets, secret)
	}

	// Apply client-side filtering
	filtered, err := s.filterSecretsBySelectors(secrets, labelSelector, fieldSelector)
	if err != nil {
		return nil, err
	}
	return filtered, nil
}

// secretToProto converts a types.Secret to a generated.Secret
func (s *SecretClient) secretToProto(secret *types.Secret) *generated.Secret {
	if secret == nil {
		return nil
	}

	proto := &generated.Secret{
		Name:      secret.Name,
		Namespace: secret.Namespace,
		Type:      secret.Type,
		Data:      secret.Data,
	}

	return proto
}

// protoToSecret converts a generated.Secret to a types.Secret
func (s *SecretClient) protoToSecret(proto *generated.Secret) (*types.Secret, error) {
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

	secret := &types.Secret{
		Name:      proto.Name,
		Namespace: proto.Namespace,
		Type:      proto.Type,
		Data:      proto.Data,
		Version:   int(proto.Version),
		CreatedAt: createdAt,
		UpdatedAt: updatedAt,
	}

	return secret, nil
}

// filterSecretsBySelectors applies label and field selector filtering client-side.
// Supported field selectors: name. Label selectors are not supported for secrets in this build.
func (s *SecretClient) filterSecretsBySelectors(secrets []*types.Secret, labelSelector, fieldSelector string) ([]*types.Secret, error) {
	// Label selectors are unsupported (no labels on types.Secret in this build)
	if labelSelector != "" {
		return nil, fmt.Errorf("label selector is not supported for secrets")
	}

	// Parse field selector
	fields, err := parseSelector(fieldSelector)
	if err != nil {
		return nil, fmt.Errorf("invalid field selector: %w", err)
	}

	var nameFilter string
	if v, ok := fields["name"]; ok {
		nameFilter = v
		delete(fields, "name")
	}
	// Any remaining fields are unsupported
	if len(fields) > 0 {
		return nil, fmt.Errorf("unsupported field selector keys for secrets: %v", fields)
	}

	if nameFilter == "" {
		return secrets, nil
	}
	result := make([]*types.Secret, 0, len(secrets))
	for _, sec := range secrets {
		if sec != nil && sec.Name == nameFilter {
			result = append(result, sec)
		}
	}
	return result, nil
}
