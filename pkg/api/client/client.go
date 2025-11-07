package client

import (
	"context"
	"fmt"
	"time"

	"github.com/runestack/rune/internal/config"
	"github.com/runestack/rune/pkg/log"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
)

// ClientOptions holds configuration options for the API client.
type ClientOptions struct {
	// Address of the API server
	Address string

	// TLS configuration
	UseTLS      bool
	TLSCertFile string

	// Authentication (token-only)
	Token string

	// Timeouts
	DialTimeout time.Duration
	CallTimeout time.Duration

	// Logger
	Logger log.Logger
}

// DefaultClientOptions returns the default client options.
func DefaultClientOptions() *ClientOptions {
	return &ClientOptions{
		Address:     fmt.Sprintf("localhost:%d", config.DefaultGRPCPort),
		UseTLS:      false,
		DialTimeout: 30 * time.Second,
		CallTimeout: 30 * time.Second,
		Logger:      log.GetDefaultLogger().WithComponent("api-client"),
	}
}

// Client provides a client for interacting with the Rune API server.
type Client struct {
	options *ClientOptions
	conn    *grpc.ClientConn
	logger  log.Logger
}

// NewClient creates a new API client with the given options.
func NewClient(options *ClientOptions) (*Client, error) {
	if options == nil {
		options = DefaultClientOptions()
	}

	// Set up logging
	logger := options.Logger
	if logger == nil {
		logger = log.GetDefaultLogger().WithComponent("api-client")
	}

	// Configure connection options
	dialOpts := []grpc.DialOption{
		grpc.WithBlock(),
	}

	// Configure TLS
	if options.UseTLS {
		if options.TLSCertFile != "" {
			creds, err := credentials.NewClientTLSFromFile(options.TLSCertFile, "")
			if err != nil {
				return nil, fmt.Errorf("failed to load TLS certificate: %w", err)
			}
			dialOpts = append(dialOpts, grpc.WithTransportCredentials(creds))
		} else {
			// Use default TLS credentials
			dialOpts = append(dialOpts, grpc.WithTransportCredentials(credentials.NewTLS(nil)))
		}
	} else {
		// No TLS
		dialOpts = append(dialOpts, grpc.WithTransportCredentials(insecure.NewCredentials()))
	}

	// Add bearer auth if provided
	if options.Token != "" {
		dialOpts = append(dialOpts, grpc.WithPerRPCCredentials(&bearerCredentials{token: options.Token}))
	}

	// Connect to the API server
	ctx, cancel := context.WithTimeout(context.Background(), options.DialTimeout)
	defer cancel()

	conn, err := grpc.DialContext(ctx, options.Address, dialOpts...)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to API server at %s: %w", options.Address, err)
	}

	return &Client{
		options: options,
		conn:    conn,
		logger:  logger,
	}, nil
}

// Close closes the client connection.
func (c *Client) Close() error {
	if c.conn != nil {
		return c.conn.Close()
	}
	return nil
}

// Conn exposes the underlying gRPC connection for generated clients
func (c *Client) Conn() *grpc.ClientConn { return c.conn }

// Context returns a context with the configured call timeout.
func (c *Client) Context() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), c.options.CallTimeout)
}

// apiKeyCredentials implements the grpc.PerRPCCredentials interface for API key authentication.
// removed apiKeyCredentials; tokens only

// bearerCredentials implements Authorization: Bearer <token>
type bearerCredentials struct{ token string }

func (b *bearerCredentials) GetRequestMetadata(ctx context.Context, uri ...string) (map[string]string, error) {
	return map[string]string{
		"authorization": "Bearer " + b.token,
	}, nil
}

// For local dev we allow sending token over insecure; production should use TLS
func (b *bearerCredentials) RequireTransportSecurity() bool { return false }

// parseTimestamp parses a timestamp string into a time.Time.
func parseTimestamp(timestampStr string) (*time.Time, error) {
	// Parse created_at timestamp
	if timestampStr != "" {
		timestamp, err := time.Parse(time.RFC3339, timestampStr)
		if err != nil {
			return nil, fmt.Errorf("failed to parse timestamp: %w", err)
		}
		return &timestamp, nil
	}
	return nil, nil
}
