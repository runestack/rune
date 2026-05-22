package dataplane

import (
	"encoding/json"
	"sort"
	"sync"

	"github.com/runestack/rune/pkg/types"
)

// Cache is the dataplane's in-memory copy of every service's endpoint
// set, keyed by service ID. It is updated by the watch loop in
// Subsystem and read by the proxy on every connection accept.
//
// All methods are safe for concurrent use. Reads return defensive
// copies so callers can iterate without holding the lock.
type Cache struct {
	mu sync.RWMutex
	// endpoints[serviceID] = ordered slice of endpoints for that
	// service. Order is the same as the producer wrote it; selection
	// (locality, weighted-random) lives in selection.go.
	endpoints map[string][]types.Endpoint
}

func newCache() *Cache {
	return NewCache()
}

// NewCache returns an empty endpoint cache.
func NewCache() *Cache {
	return &Cache{endpoints: make(map[string][]types.Endpoint)}
}

// Set replaces the endpoint slice for serviceID. A nil/empty slice is
// kept as an explicit "no endpoints" marker — the proxy fails closed
// instead of falling back to historic data.
func (c *Cache) Set(serviceID string, eps []types.Endpoint) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(eps) == 0 {
		c.endpoints[serviceID] = nil
		return
	}
	cp := make([]types.Endpoint, len(eps))
	copy(cp, eps)
	c.endpoints[serviceID] = cp
}

// Delete removes serviceID's entry. Subsequent Get returns ok=false.
func (c *Cache) Delete(serviceID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.endpoints, serviceID)
}

// Get returns the endpoint slice for serviceID. ok is false if the
// service has never been seen by this cache. A nil slice with ok=true
// means "service known, no endpoints right now".
func (c *Cache) Get(serviceID string) (eps []types.Endpoint, ok bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	v, ok := c.endpoints[serviceID]
	if !ok {
		return nil, false
	}
	if len(v) == 0 {
		return nil, true
	}
	out := make([]types.Endpoint, len(v))
	copy(out, v)
	return out, true
}

// Healthy returns only the healthy endpoints for serviceID.
func (c *Cache) Healthy(serviceID string) ([]types.Endpoint, bool) {
	all, ok := c.Get(serviceID)
	if !ok {
		return nil, false
	}
	out := make([]types.Endpoint, 0, len(all))
	for _, ep := range all {
		if ep.Healthy {
			out = append(out, ep)
		}
	}
	return out, true
}

// Snapshot returns a sorted list of (serviceID, count) pairs for
// diagnostics. Allocation cost is acceptable for the admin surface.
func (c *Cache) Snapshot() []ServiceSummary {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make([]ServiceSummary, 0, len(c.endpoints))
	for id, eps := range c.endpoints {
		healthy := 0
		for _, e := range eps {
			if e.Healthy {
				healthy++
			}
		}
		out = append(out, ServiceSummary{ServiceID: id, Total: len(eps), Healthy: healthy})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ServiceID < out[j].ServiceID })
	return out
}

// ServiceSummary is the public diagnostic view of a single entry.
type ServiceSummary struct {
	ServiceID string
	Total     int
	Healthy   int
}

// decodePayload is a small wrapper used by the snapshot hydrate path
// to decode a raw ServiceEndpoints JSON payload without importing
// the orderedlog package's mutation type.
func decodePayload(raw []byte, out *types.ServiceEndpoints) error {
	return json.Unmarshal(raw, out)
}

// decodeJSON is a generic decode helper for snapshot hydration of
// other op-kind records (local_instances, etc.).
func decodeJSON(raw []byte, out any) error {
	return json.Unmarshal(raw, out)
}
