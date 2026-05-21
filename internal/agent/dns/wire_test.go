package dns

import (
	"context"
	"net"
	"testing"

	"github.com/runestack/rune/pkg/log"
	"github.com/runestack/rune/pkg/store"
	"github.com/runestack/rune/pkg/types"
)

type fakeVIPSource struct {
	vips map[string]string
}

func (f *fakeVIPSource) VIPForService(_ context.Context, serviceID string) (net.IP, error) {
	if ip, ok := f.vips[serviceID]; ok {
		return net.ParseIP(ip), nil
	}
	return nil, nil
}

func TestStoreZone_LookupA_FallsBackToVIPSource(t *testing.T) {
	st := store.NewTestStore()
	svc := &types.Service{
		ID:        "svc-mongo",
		Name:      "mongo",
		Namespace: "shared",
		Status:    types.ServiceStatusRunning,
	}
	if err := st.Create(context.Background(), types.ResourceTypeService, svc.Namespace, svc.Name, svc); err != nil {
		t.Fatalf("create service: %v", err)
	}

	z := NewStoreZone(st, &fakeVIPSource{vips: map[string]string{
		"svc-mongo": "10.96.0.10",
	}}, log.GetDefaultLogger())

	ips, ok := z.LookupA("shared", "mongo")
	if !ok || len(ips) != 1 || ips[0].String() != "10.96.0.10" {
		t.Fatalf("LookupA = %v, %v; want 10.96.0.10", ips, ok)
	}
}

func TestStoreZone_LookupA_PrefersPersistedVIP(t *testing.T) {
	st := store.NewTestStore()
	svc := &types.Service{
		ID:        "svc-mongo",
		Name:      "mongo",
		Namespace: "shared",
		Discovery: &types.ServiceDiscovery{VIP: "10.96.0.99"},
	}
	if err := st.Create(context.Background(), types.ResourceTypeService, svc.Namespace, svc.Name, svc); err != nil {
		t.Fatalf("create service: %v", err)
	}

	z := NewStoreZone(st, &fakeVIPSource{vips: map[string]string{
		"svc-mongo": "10.96.0.10",
	}}, log.GetDefaultLogger())

	ips, ok := z.LookupA("shared", "mongo")
	if !ok || len(ips) != 1 || ips[0].String() != "10.96.0.99" {
		t.Fatalf("LookupA = %v, %v; want persisted 10.96.0.99", ips, ok)
	}
}
