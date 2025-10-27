package watcher

import (
	"context"
	"fmt"
	"strings"

	"github.com/runestack/rune/pkg/api/client"
	"github.com/runestack/rune/pkg/cli/utils"
)

// InstanceWatcher defines the interface for watching instances
type InstanceWatcher interface {
	WatchInstances(namespace, serviceId, labelSelector, fieldSelector string) (<-chan client.InstanceWatchEvent, error)
}

// InstanceWatcherAdapter adapts the InstanceWatcher interface to ResourceToWatch
type InstanceWatcherAdapter struct {
	InstanceWatcher
}

// Watch implements the ResourceToWatch interface
func (a InstanceWatcherAdapter) Watch(ctx context.Context, namespace, labelSelector, fieldSelector string) (<-chan WatchEvent, error) {
	// Parse any service filter from fieldSelector
	serviceID := ""
	fieldSelectorMap, err := utils.ParseSelector(fieldSelector)
	if err == nil {
		if svc, exists := fieldSelectorMap["service"]; exists {
			serviceID = svc
			// Remove from field selector to avoid double filtering
			delete(fieldSelectorMap, "service")
			// Rebuild field selector string
			var pairs []string
			for k, v := range fieldSelectorMap {
				pairs = append(pairs, fmt.Sprintf("%s=%s", k, v))
			}
			fieldSelector = strings.Join(pairs, ",")
		}
	}

	instCh, err := a.WatchInstances(namespace, serviceID, labelSelector, fieldSelector)
	if err != nil {
		return nil, err
	}

	// Create a new channel and adapt the events
	eventCh := make(chan WatchEvent)
	go func() {
		defer close(eventCh)

		for event := range instCh {
			if event.Error != nil {
				eventCh <- WatchEvent{
					Error: event.Error,
				}
				continue
			}

			eventCh <- WatchEvent{
				Resource:  event.Instance,
				EventType: event.EventType,
				Error:     nil,
			}
		}
	}()

	return eventCh, nil
}
