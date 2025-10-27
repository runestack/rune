package watcher

import (
	"context"

	"github.com/runestack/rune/pkg/api/client"
)

// ServiceWatcher defines the interface for watching services
type ServiceWatcher interface {
	WatchServices(namespace, labelSelector, fieldSelector string) (<-chan client.WatchEvent, context.CancelFunc, error)
}

// ServiceWatcherAdapter adapts the ServiceWatcher interface to ResourceToWatch
type ServiceWatcherAdapter struct {
	ServiceWatcher
}

// Watch implements the ResourceToWatch interface
func (a ServiceWatcherAdapter) Watch(ctx context.Context, namespace, labelSelector, fieldSelector string) (<-chan WatchEvent, error) {
	svcCh, cancelWatch, err := a.WatchServices(namespace, labelSelector, fieldSelector)
	if err != nil {
		return nil, err
	}

	// Fast-path: if an immediate error is already available, surface it
	select {
	case ev, ok := <-svcCh:
		if ok {
			if ev.Error != nil {
				eventCh := make(chan WatchEvent, 1)
				eventCh <- WatchEvent{Error: ev.Error}
				close(eventCh)
				cancelWatch()
				return eventCh, nil
			}
			// Push back non-error event into a new buffered channel to not lose it
			eventCh := make(chan WatchEvent, 1)
			// Convert service to ServiceResource and forward
			if ev.Service != nil {
				eventCh <- WatchEvent{Resource: ev.Service, EventType: ev.EventType}
			}
			// Start normal forwarding goroutine for subsequent events
			go func(firstHandled bool) {
				defer close(eventCh)
				defer cancelWatch()
				if !firstHandled && ev.Error != nil {
					eventCh <- WatchEvent{Error: ev.Error}
				}
				for {
					select {
					case <-ctx.Done():
						return
					case event, ok := <-svcCh:
						if !ok {
							return
						}
						if event.Error != nil {
							eventCh <- WatchEvent{Error: event.Error}
							continue
						}
						eventCh <- WatchEvent{Resource: event.Service, EventType: event.EventType}
					}
				}
			}(true)
			return eventCh, nil
		}
	default:
		// No immediate event available; proceed normally
	}

	// Create a new channel and adapt the events
	eventCh := make(chan WatchEvent)
	go func() {
		defer close(eventCh)
		defer cancelWatch() // Ensure we clean up the watch when done

		for {
			select {
			case <-ctx.Done():
				return
			case event, ok := <-svcCh:
				if !ok {
					return
				}

				if event.Error != nil {
					eventCh <- WatchEvent{
						Error: event.Error,
					}
					continue
				}

				eventCh <- WatchEvent{
					Resource:  event.Service,
					EventType: event.EventType,
					Error:     nil,
				}
			}
		}
	}()

	return eventCh, nil
}
