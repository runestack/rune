package dataplane

import (
	"context"
	"net"
	"testing"

	"github.com/runestack/rune/pkg/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEnrichServiceVIP_fromResolver(t *testing.T) {
	svc := &types.Service{ID: "svc-1", Name: "mongo", Namespace: "shared"}
	resolver := FuncVIPResolver{Fn: func(_ context.Context, id string) (net.IP, error) {
		assert.Equal(t, "svc-1", id)
		return net.ParseIP("10.96.0.10"), nil
	}}
	require.NoError(t, enrichServiceVIP(context.Background(), svc, resolver))
	require.NotNil(t, svc.Discovery)
	assert.Equal(t, "10.96.0.10", svc.Discovery.VIP)
}

func TestEnrichServiceVIP_preservesExisting(t *testing.T) {
	svc := &types.Service{
		ID:        "svc-1",
		Discovery: &types.ServiceDiscovery{VIP: "10.96.0.99"},
	}
	require.NoError(t, enrichServiceVIP(context.Background(), svc, FuncVIPResolver{Fn: func(context.Context, string) (net.IP, error) {
		t.Fatal("resolver should not be called")
		return nil, nil
	}}))
	assert.Equal(t, "10.96.0.99", svc.Discovery.VIP)
}
