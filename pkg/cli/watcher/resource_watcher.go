package watcher

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/pterm/pterm"
	"github.com/runestack/rune/pkg/cli/format"
	"github.com/runestack/rune/pkg/types"
)

// ResourceWatcher is a generic interface for resource watching
type ResourceWatcher struct {
	// Configuration
	Namespace         string
	AllNamespaces     bool
	LabelSelector     string
	FieldSelector     string
	ResourceName      string
	ShowHeaders       bool
	RefreshInterval   time.Duration
	InitialBufferTime time.Duration
	WatchTimeout      time.Duration

	// Internal state
	resources              map[string]types.Resource
	events                 []Event
	lastUpdateTime         time.Time
	initialSyncComplete    bool
	reconnectDelay         time.Duration
	maxReconnectDelay      time.Duration
	tableRenderer          *pterm.TablePrinter
	termWidth, termHeight  int
	maxEvents              int
	headerRenderer         func() string
	resourceToRowsRenderer func(resources []types.Resource) [][]string
	eventRenderer          func(events []Event) []string
}

// Event represents a resource change event for display
type Event struct {
	EventType string         // "ADDED", "MODIFIED", "DELETED"
	Resource  types.Resource // The resource that changed
	Timestamp time.Time      // When the event occurred
}

// ResourceToWatch is a generic interface for resource watchers
type ResourceToWatch interface {
	// Watch starts watching the resource and returns a channel of events
	Watch(ctx context.Context, namespace, labelSelector, fieldSelector string) (<-chan WatchEvent, error)
}

// WatchEvent represents a resource change event from the API
type WatchEvent struct {
	Resource  types.Resource
	EventType string // "ADDED", "MODIFIED", "DELETED"
	Error     error
}

// NewResourceWatcher creates a new watcher with default configuration
func NewResourceWatcher() *ResourceWatcher {
	w := &ResourceWatcher{
		Namespace:         "default",
		AllNamespaces:     false,
		RefreshInterval:   2 * time.Second,
		InitialBufferTime: 500 * time.Millisecond,
		resources:         make(map[string]types.Resource),
		events:            make([]Event, 0, 10),
		maxEvents:         10,
		reconnectDelay:    1 * time.Second,
		maxReconnectDelay: 30 * time.Second,
		lastUpdateTime:    time.Now(),
	}

	// Initialize terminal size
	w.updateTerminalSize()

	return w
}

// SetHeaderRenderer sets a custom function to render the header
func (w *ResourceWatcher) SetHeaderRenderer(renderer func() string) {
	w.headerRenderer = renderer
}

// SetResourceToRowsRenderer sets a custom function to convert resources to table rows
func (w *ResourceWatcher) SetResourceToRowsRenderer(renderer func(resources []types.Resource) [][]string) {
	w.resourceToRowsRenderer = renderer
}

// SetEventRenderer sets a custom function to render events
func (w *ResourceWatcher) SetEventRenderer(renderer func(events []Event) []string) {
	w.eventRenderer = renderer
}

// SetTimeout sets the watch timeout duration
func (w *ResourceWatcher) SetTimeout(timeout string) error {
	if timeout == "" {
		w.WatchTimeout = 0
		return nil
	}

	duration, err := time.ParseDuration(timeout)
	if err != nil {
		return fmt.Errorf("invalid timeout value: %w", err)
	}
	w.WatchTimeout = duration
	return nil
}

// Watch starts watching resources using the provided watcher
func (w *ResourceWatcher) Watch(ctx context.Context, watcher ResourceToWatch) error {
	// Update the namespace if using all namespaces
	namespace := w.Namespace
	if w.AllNamespaces {
		namespace = "*" // Use wildcard to watch all namespaces
	}

	// Set up a channel to handle OS signals for graceful termination
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)

	// Create context with timeout if specified
	var cancel context.CancelFunc
	if w.WatchTimeout > 0 {
		ctx, cancel = context.WithTimeout(ctx, w.WatchTimeout)
	} else {
		// No timeout specified, create a cancellable context
		ctx, cancel = context.WithCancel(ctx)
	}
	defer cancel()

	// Create a filter for a specific resource if provided
	var fieldSelectorStr string
	if w.ResourceName != "" {
		if w.FieldSelector != "" {
			fieldSelectorStr = w.FieldSelector + ",name=" + w.ResourceName
		} else {
			fieldSelectorStr = "name=" + w.ResourceName
		}
	} else {
		fieldSelectorStr = w.FieldSelector
	}

	// Initialize timers and state
	refreshTicker := time.NewTicker(w.RefreshInterval)
	defer refreshTicker.Stop()

	initialSyncTimer := time.NewTimer(2 * time.Second)
	initialBufferingTimeout := time.NewTimer(w.InitialBufferTime)
	displayTrigger := time.NewTimer(100 * time.Millisecond)

	initialCaching := true
	firstRun := true

	// Start watching for resource changes
	watchCh, err := watcher.Watch(ctx, namespace, w.LabelSelector, fieldSelectorStr)
	if err != nil {
		return fmt.Errorf("failed to watch resources: %w", err)
	}

	for {
		select {
		case <-initialBufferingTimeout.C:
			// Mark initial buffering as complete after timeout
			if initialCaching {
				initialCaching = false
				// Trigger an immediate refresh to show all resources we've received so far
				w.refreshScreen()
				firstRun = false
			}

		case <-displayTrigger.C:
			// If we have resources and haven't displayed yet, show them now
			if len(w.resources) > 0 && initialCaching {
				// Reset the initial buffering timeout to give a bit more time
				// for other resources to arrive
				initialBufferingTimeout.Reset(150 * time.Millisecond)
			}

		case <-initialSyncTimer.C:
			// Mark initial sync as complete after the timer expires
			w.initialSyncComplete = true

		case event, ok := <-watchCh:
			if !ok {
				// Channel closed, try to reconnect
				fmt.Println("\nWatch connection lost. Reconnecting...")
				time.Sleep(w.reconnectDelay)

				// Exponential backoff for reconnect attempts
				w.reconnectDelay = min(w.reconnectDelay*2, w.maxReconnectDelay)

				// Attempt to reestablish watch
				watchCh, err = watcher.Watch(ctx, namespace, w.LabelSelector, fieldSelectorStr)
				if err != nil {
					fmt.Printf("\nFailed to reconnect: %v. Retrying in %v...\n", err, w.reconnectDelay)
					continue
				}

				// Reset delay on successful reconnection
				w.reconnectDelay = 1 * time.Second
				w.lastUpdateTime = time.Now()
				continue
			}

			if event.Error != nil {
				// Check if it's a timeout error
				if strings.Contains(event.Error.Error(), "DeadlineExceeded") ||
					strings.Contains(event.Error.Error(), "context deadline exceeded") ||
					strings.Contains(event.Error.Error(), "transport is closing") {
					// Handle timeout by reconnecting
					fmt.Println("\nWatch connection timed out. Reconnecting...")
					time.Sleep(w.reconnectDelay)

					// Exponential backoff for reconnect attempts
					w.reconnectDelay = min(w.reconnectDelay*2, w.maxReconnectDelay)

					// Attempt to reestablish watch
					watchCh, err = watcher.Watch(ctx, namespace, w.LabelSelector, fieldSelectorStr)
					if err != nil {
						fmt.Printf("\nFailed to reconnect: %v. Retrying in %v...\n", err, w.reconnectDelay)
						continue
					}

					// Reset delay on successful reconnection
					w.reconnectDelay = 1 * time.Second
					w.lastUpdateTime = time.Now()
					continue
				}

				// For other errors, return the error
				return fmt.Errorf("watch error: %w", event.Error)
			}

			resource := event.Resource
			resourceKey := resource.String()
			w.lastUpdateTime = time.Now()

			// Update our local state based on event type
			switch event.EventType {
			case "ADDED", "MODIFIED":
				oldResource, exists := w.resources[resourceKey]
				w.resources[resourceKey] = resource

				// Only add event for real changes or new additions after initial sync
				if !exists {
					if w.initialSyncComplete {
						// This is a genuinely new resource after initial sync
						w.events = append(w.events, Event{
							EventType: "ADDED",
							Resource:  resource,
							Timestamp: time.Now(),
						})
					}
				} else if event.EventType == "MODIFIED" && !oldResource.Equals(resource) {
					// Always show modifications
					w.events = append(w.events, Event{
						EventType: "MODIFIED",
						Resource:  resource,
						Timestamp: time.Now(),
					})
				}
			case "DELETED":
				delete(w.resources, resourceKey)

				// Always show deletions
				w.events = append(w.events, Event{
					EventType: "DELETED",
					Resource:  resource,
					Timestamp: time.Now(),
				})
			}

			// Limit recent events to max events
			if len(w.events) > w.maxEvents {
				w.events = w.events[len(w.events)-w.maxEvents:]
			}

			// Only do immediate refresh if we're not in initial caching phase
			if !initialCaching {
				// If this is first actual refresh after buffering, mark firstRun as false
				if firstRun {
					w.refreshScreen()
					firstRun = false
				}
			}

			// Reset reconnect delay on successful events
			w.reconnectDelay = 1 * time.Second

		case <-refreshTicker.C:
			// Only refresh if we've exited initial caching or if we have a good batch of resources
			if !initialCaching || len(w.resources) > 0 {
				w.refreshScreen()
				// If we were caching, mark as no longer caching
				initialCaching = false
				firstRun = false
			}

		case <-sig:
			fmt.Println("\nWatch interrupted")
			return nil

		case <-ctx.Done():
			if ctx.Err() == context.DeadlineExceeded {
				fmt.Println("\nWatch timeout reached")
				return nil
			}
			return ctx.Err()
		}
	}
}

// refreshScreen clears the screen and redraws the current state
func (w *ResourceWatcher) refreshScreen() {
	// Update terminal size for proper display
	w.updateTerminalSize()

	// Clear screen without adding to scrollback
	fmt.Print("\033[1;1H\033[J")

	fmt.Println()

	// Convert map to slice for sorting and rendering
	var resourcesList []types.Resource
	for _, res := range w.resources {
		resourcesList = append(resourcesList, res)
	}

	// Print table with current resource state
	if len(resourcesList) > 0 {
		if w.resourceToRowsRenderer != nil {
			rows := w.resourceToRowsRenderer(resourcesList)
			w.renderTable(rows)
		} else {
			fmt.Println("No custom resource renderer defined")
		}
		fmt.Println()
	} else {
		fmt.Println("No resources found")
		fmt.Println()
	}

	// Print recent events if there are any
	if len(w.events) > 0 {
		if w.eventRenderer != nil {
			eventLines := w.eventRenderer(w.events)
			for _, line := range eventLines {
				fmt.Println(line)
			}
		} else {
			w.printDefaultEvents()
		}
	}

	fmt.Println()
	fmt.Println("Press Ctrl+C to exit.")
}

// renderTable renders a table with the provided rows
func (w *ResourceWatcher) renderTable(rows [][]string) {
	if len(rows) == 0 {
		return
	}

	// Create a new table if we don't have one yet
	if w.tableRenderer == nil {
		table := pterm.DefaultTable.WithHasHeader(!w.ShowHeaders)
		// Customize the header style to use BoldBlue
		headerStyle := pterm.NewStyle(pterm.FgCyan, pterm.Bold)
		table = table.WithHeaderStyle(headerStyle)
		w.tableRenderer = table
	}

	// Calculate how many rows we can display based on terminal height
	// Account for header, footer, event display etc.
	headerLines := 3                // Header text + blank line + table header
	footerLines := 3                // Blank line + "Press Ctrl+C" + buffer
	eventLines := len(w.events) + 1 // +1 for blank line
	if eventLines == 1 {
		eventLines = 0 // No blank line if no events
	}

	maxTableLines := w.termHeight - headerLines - footerLines - eventLines - 1
	displayRows := rows

	// Ensure maxTableLines is at least 1 to avoid slice bounds errors
	if maxTableLines < 1 {
		maxTableLines = 1
	}

	// If we have more rows than can fit, truncate and add a message
	if len(rows) > maxTableLines {
		displayRows = rows[:maxTableLines]
		fmt.Printf("... %d more resources not shown (limited by terminal height) ...\n", len(rows)-maxTableLines)
	}

	// Render the table
	err := w.tableRenderer.WithData(displayRows).Render()
	if err != nil {
		fmt.Println("Error rendering table:", err)
	}
}

// printDefaultEvents prints recent events in a formatted way
func (w *ResourceWatcher) printDefaultEvents() {
	for _, event := range w.events {
		var symbol, color string
		var eventPrefix string

		switch event.EventType {
		case "ADDED":
			symbol = "+"
			color = format.Green
			eventPrefix = "ADDED"
		case "MODIFIED":
			symbol = "~"
			color = format.Yellow
			eventPrefix = "MODIFIED"
		case "DELETED":
			symbol = "-"
			color = format.Red
			eventPrefix = "DELETED"
		}

		eventText := fmt.Sprintf("[%s] %s resource \"%s\"",
			format.Colorize(color, symbol),
			eventPrefix,
			format.Colorize(format.Bold, event.Resource.String()))

		fmt.Println(eventText)
	}
}

// updateTerminalSize updates the cached terminal dimensions
func (w *ResourceWatcher) updateTerminalSize() {
	width, height, err := getTerminalSize()
	if err != nil {
		// Default if can't determine
		width, height = 100, 24
	}
	w.termWidth = width
	w.termHeight = height
}
