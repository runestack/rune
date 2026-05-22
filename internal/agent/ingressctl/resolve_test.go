package ingressctl

import (
	"context"
	"testing"

	"github.com/runestack/rune/internal/agent/dataplane"
	"github.com/runestack/rune/pkg/store"
	"github.com/runestack/rune/pkg/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func seedService(t *testing.T, st *store.TestStore, svc types.Service) {
	t.Helper()
	require.NoError(t, st.CreateService(context.Background(), &svc))
}

func TestResolve_PrefersVIP(t *testing.T) {
	st := store.NewTestStore()
	seedService(t, st, types.Service{
		ID:        "svc-1",
		Namespace: "prod",
		Name:      "docs",
		Discovery: &types.ServiceDiscovery{VIP: "10.96.0.14"},
	})
	c := New(Config{Store: st, Cache: dataplane.NewCache()})
	target, ok := c.Resolve("prod", "docs", 3000)
	require.True(t, ok)
	assert.Equal(t, "10.96.0.14:3000", target)
}

func TestResolve_FallsBackToCacheByServiceID(t *testing.T) {
	st := store.NewTestStore()
	seedService(t, st, types.Service{
		ID:        "svc-1",
		Namespace: "prod",
		Name:      "docs",
	})
	cache := dataplane.NewCache()
	cache.Set("svc-1", []types.Endpoint{{IP: "172.17.0.3", Port: 3000, Healthy: true}})
	c := New(Config{Store: st, Cache: cache})
	target, ok := c.Resolve("prod", "docs", 3000)
	require.True(t, ok)
	assert.Equal(t, "172.17.0.3:3000", target)
}

func TestResolve_ReservedPort80UsesCacheNotVIP(t *testing.T) {
	st := store.NewTestStore()
	seedService(t, st, types.Service{
		ID:        "svc-landing",
		Namespace: "prod",
		Name:      "landing",
		Discovery: &types.ServiceDiscovery{VIP: "10.96.0.13"},
	})
	cache := dataplane.NewCache()
	cache.Set("svc-landing", []types.Endpoint{{IP: "172.17.0.2", Port: 80, Healthy: true}})
	c := New(Config{
		Store:             st,
		Cache:             cache,
		ReservedHostPorts: []int{80, 443},
	})
	target, ok := c.Resolve("prod", "landing", 80)
	require.True(t, ok)
	assert.Equal(t, "172.17.0.2:80", target)
}

func TestResolve_IgnoresStaleNameKeyedCache(t *testing.T) {
	st := store.NewTestStore()
	seedService(t, st, types.Service{
		ID:        "svc-1",
		Namespace: "prod",
		Name:      "docs",
		Discovery: &types.ServiceDiscovery{VIP: "10.96.0.14"},
	})
	cache := dataplane.NewCache()
	cache.Set("docs", []types.Endpoint{{IP: "172.17.0.7", Port: 3000, Healthy: true}})
	c := New(Config{Store: st, Cache: cache})
	target, ok := c.Resolve("prod", "docs", 3000)
	require.True(t, ok)
	assert.Equal(t, "10.96.0.14:3000", target)
}
