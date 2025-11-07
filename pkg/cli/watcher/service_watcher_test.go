package watcher

import (
	"context"
	"errors"
	"testing"
	"time"

	apiClient "github.com/runestack/rune/pkg/api/client"
	"github.com/runestack/rune/pkg/types"
)

type fakeServiceWatcher struct {
	ch           chan apiClient.WatchEvent
	cancelCalled chan struct{}
	lastArgs     struct {
		namespace     string
		labelSelector string
		fieldSelector string
	}
}

func newFakeServiceWatcher() *fakeServiceWatcher {
	return &fakeServiceWatcher{
		ch:           make(chan apiClient.WatchEvent, 10),
		cancelCalled: make(chan struct{}, 1),
	}
}

func (f *fakeServiceWatcher) WatchServices(namespace, labelSelector, fieldSelector string) (<-chan apiClient.WatchEvent, context.CancelFunc, error) {
	f.lastArgs.namespace = namespace
	f.lastArgs.labelSelector = labelSelector
	f.lastArgs.fieldSelector = fieldSelector
	return f.ch, func() {
		select {
		case f.cancelCalled <- struct{}{}:
		default:
		}
	}, nil
}

func TestServiceWatcherAdapter_ForwardsEvents(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	fw := newFakeServiceWatcher()
	adapter := ServiceWatcherAdapter{ServiceWatcher: fw}

	// Start adapter
	eventCh, err := adapter.Watch(ctx, "default", "", "")
	if err != nil {
		t.Fatalf("adapter.Watch returned error: %v", err)
	}

	// Send one service event
	svc := &types.Service{Name: "api", Namespace: "default"}
	fw.ch <- apiClient.WatchEvent{Service: svc, EventType: "ADDED"}

	// Read adapted event
	select {
	case ev, ok := <-eventCh:
		if !ok {
			t.Fatalf("event channel closed unexpectedly")
		}
		if ev.Error != nil {
			t.Fatalf("unexpected error: %v", ev.Error)
		}
		if ev.EventType != "ADDED" {
			t.Fatalf("unexpected event type: %s", ev.EventType)
		}
		if _, ok := ev.Resource.(*types.Service); !ok {
			t.Fatalf("resource not converted to *types.Service")
		}
	case <-time.After(time.Second):
		t.Fatalf("timed out waiting for event")
	}

	// Close source and ensure cancel is called by adapter
	close(fw.ch)
	select {
	case <-fw.cancelCalled:
		// ok
	case <-time.After(time.Second):
		t.Fatalf("adapter did not call cancel on underlying watcher")
	}
}

func TestServiceWatcherAdapter_ImmediateErrorFastPath(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	fw := newFakeServiceWatcher()
	// Preload an error into the channel to hit fast-path
	fw.ch <- apiClient.WatchEvent{Error: errors.New("boom")}

	adapter := ServiceWatcherAdapter{ServiceWatcher: fw}

	eventCh, err := adapter.Watch(ctx, "default", "", "")
	if err != nil {
		t.Fatalf("adapter.Watch returned error: %v", err)
	}

	// Expect an error surfaced and channel to close afterwards
	ev, ok := <-eventCh
	if !ok {
		t.Fatalf("expected an event, channel closed early")
	}
	if ev.Error == nil {
		t.Fatalf("expected error event, got none")
	}

	// Channel should be closed after delivering the error
	select {
	case _, still := <-eventCh:
		if still {
			t.Fatalf("expected channel to be closed after error event")
		}
	case <-time.After(time.Second):
		t.Fatalf("timeout waiting for channel close")
	}

	// Ensure cancel was invoked
	select {
	case <-fw.cancelCalled:
		// ok
	case <-time.After(time.Second):
		t.Fatalf("adapter did not call cancel on underlying watcher (fast-path)")
	}
}
