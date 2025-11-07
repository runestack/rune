package watcher

import (
	"context"
	"testing"
	"time"

	apiClient "github.com/runestack/rune/pkg/api/client"
	"github.com/runestack/rune/pkg/cli/utils"
	"github.com/runestack/rune/pkg/types"
)

type fakeInstanceWatcher struct {
	ch       chan apiClient.InstanceWatchEvent
	lastArgs struct {
		namespace     string
		serviceID     string
		labelSelector string
		fieldSelector string
	}
}

func newFakeInstanceWatcher() *fakeInstanceWatcher {
	return &fakeInstanceWatcher{ch: make(chan apiClient.InstanceWatchEvent, 10)}
}

func (f *fakeInstanceWatcher) WatchInstances(namespace, serviceId, labelSelector, fieldSelector string) (<-chan apiClient.InstanceWatchEvent, error) {
	f.lastArgs.namespace = namespace
	f.lastArgs.serviceID = serviceId
	f.lastArgs.labelSelector = labelSelector
	f.lastArgs.fieldSelector = fieldSelector
	return f.ch, nil
}

func TestInstanceWatcherAdapter_ForwardsEvents(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	fw := newFakeInstanceWatcher()
	adapter := InstanceWatcherAdapter{InstanceWatcher: fw}

	eventCh, err := adapter.Watch(ctx, "default", "", "")
	if err != nil {
		t.Fatalf("adapter.Watch returned error: %v", err)
	}

	inst := &types.Instance{ID: "1", Name: "api-1", Namespace: "default"}
	fw.ch <- apiClient.InstanceWatchEvent{Instance: inst, EventType: "ADDED"}

	select {
	case ev := <-eventCh:
		if ev.Error != nil {
			t.Fatalf("unexpected error: %v", ev.Error)
		}
		if ev.EventType != "ADDED" {
			t.Fatalf("unexpected event type: %s", ev.EventType)
		}
		if _, ok := ev.Resource.(*types.Instance); !ok {
			t.Fatalf("resource not converted to *types.Instance")
		}
	case <-time.After(time.Second):
		t.Fatalf("timed out waiting for event")
	}
}

func TestInstanceWatcherAdapter_ServiceFilterParsing(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	fw := newFakeInstanceWatcher()
	adapter := InstanceWatcherAdapter{InstanceWatcher: fw}

	// Include a service filter inside the fieldSelector which should be extracted
	_, err := adapter.Watch(ctx, "prod", "env=prod", "service=payments,other=value")
	if err != nil {
		t.Fatalf("adapter.Watch returned error: %v", err)
	}

	if fw.lastArgs.namespace != "prod" {
		t.Fatalf("namespace not forwarded correctly: %s", fw.lastArgs.namespace)
	}
	if fw.lastArgs.serviceID != "payments" {
		t.Fatalf("serviceID not extracted correctly: %s", fw.lastArgs.serviceID)
	}
	// Remaining field selector should exclude the service key
	if fw.lastArgs.fieldSelector == "" {
		// ok if only key removed
		return
	}
	parsed, err := utils.ParseSelector(fw.lastArgs.fieldSelector)
	if err != nil {
		t.Fatalf("invalid remaining field selector: %v", err)
	}
	if _, exists := parsed["service"]; exists {
		t.Fatalf("service key should have been removed from field selector")
	}
}
