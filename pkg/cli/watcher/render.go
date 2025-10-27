package watcher

import (
	"fmt"
	"sort"
	"strings"

	"github.com/runestack/rune/pkg/cli/format"
	"github.com/runestack/rune/pkg/types"
)

func DefaultServiceResourceToRows(resources []types.Resource) [][]string {
	// First create a list of just the services
	services := make([]*types.Service, 0, len(resources))
	for _, res := range resources {
		svcRes, ok := res.(*types.Service)
		if !ok {
			continue
		}
		services = append(services, svcRes)
	}

	// Sort services by name
	sort.Slice(services, func(i, j int) bool {
		return services[i].Name < services[j].Name
	})

	// Build rows
	var rows [][]string

	// Add header row - but first check if we have any services
	allNamespaces := false
	if len(services) > 0 {
		for _, svc := range services {
			if svc.Namespace != services[0].Namespace {
				allNamespaces = true
				break
			}
		}
	}

	// Add header row
	if allNamespaces {
		rows = append(rows, []string{"NAMESPACE", "NAME", "TYPE", "STATUS", "INSTANCES", "IMAGE/COMMAND", "AGE"})
	} else {
		rows = append(rows, []string{"NAME", "TYPE", "STATUS", "INSTANCES", "IMAGE/COMMAND", "AGE"})
	}

	// Add service rows
	for _, service := range services {
		// Determine service type
		serviceType := "container"
		if service.Runtime == "process" && service.Process != nil {
			serviceType = "process"
		}

		// Format status using the same colorizeStatus function from table.go
		status := format.PTermStatusLabel(string(service.Status))

		// Format instances
		instances := fmt.Sprintf("%d/%d", countRunningInstances(service), service.Scale)

		// Determine image or command
		imageOrCommand := service.Image
		if service.Runtime == "process" && service.Process != nil {
			imageOrCommand = service.Process.Command
			if len(service.Process.Args) > 0 {
				imageOrCommand += " " + strings.Join(service.Process.Args, " ")
			}
		}

		// Truncate very long image/command strings
		if len(imageOrCommand) > 60 {
			imageOrCommand = truncateString(imageOrCommand, 60)
		}

		// Calculate age
		age := formatAge(service.Metadata.CreatedAt)

		// Create the row
		var row []string
		if allNamespaces {
			row = []string{
				service.Namespace,
				service.Name,
				serviceType,
				status,
				instances,
				imageOrCommand,
				age,
			}
		} else {
			row = []string{
				service.Name,
				serviceType,
				status,
				instances,
				imageOrCommand,
				age,
			}
		}

		rows = append(rows, row)
	}

	return rows
}

// DefaultServiceEventRenderer returns a default renderer for service events
func DefaultServiceEventRenderer(events []Event) []string {
	var lines []string
	for _, event := range events {
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

		svcRes, ok := event.Resource.(*types.Service)
		if !ok {
			continue
		}

		eventText := fmt.Sprintf("[%s] %s service \"%s\"",
			format.Colorize(color, symbol),
			eventPrefix,
			format.Colorize(format.Bold, svcRes.Name))

		lines = append(lines, eventText)
	}
	return lines
}

// DefaultInstanceResourceToRows returns a default row renderer for instances
func DefaultInstanceResourceToRows(resources []types.Resource) [][]string {
	// First create a list of just the instances
	instances := make([]*types.Instance, 0, len(resources))
	for _, res := range resources {
		instRes, ok := res.(*types.Instance)
		if !ok {
			continue
		}
		instances = append(instances, instRes)
	}

	// Sort instances by name
	sort.Slice(instances, func(i, j int) bool {
		return instances[i].Name < instances[j].Name
	})

	// Build rows
	var rows [][]string

	// Add header row - but first check if we have any instances
	allNamespaces := false
	if len(instances) > 0 {
		for _, inst := range instances {
			if inst.Namespace != instances[0].Namespace {
				allNamespaces = true
				break
			}
		}
	}

	// Add header row
	if allNamespaces {
		rows = append(rows, []string{"NAMESPACE", "NAME", "SERVICE", "NODE", "STATUS", "RESTARTS", "AGE"})
	} else {
		rows = append(rows, []string{"NAME", "SERVICE", "NODE", "STATUS", "RESTARTS", "AGE"})
	}

	// Add instance rows
	for _, instance := range instances {
		// Format status using the same colorizeStatus function from table.go
		status := format.PTermStatusLabel(string(instance.Status))

		// Format restarts (currently a placeholder as we don't track this yet)
		restarts := "0" // Placeholder

		// Calculate age
		age := formatAge(instance.CreatedAt)

		// Create the row
		var row []string
		if allNamespaces {
			row = []string{
				instance.Namespace,
				instance.Name,
				instance.ServiceID,
				instance.NodeID,
				status,
				restarts,
				age,
			}
		} else {
			row = []string{
				instance.Name,
				instance.ServiceID,
				instance.NodeID,
				status,
				restarts,
				age,
			}
		}

		rows = append(rows, row)
	}

	return rows
}

// DefaultInstanceEventRenderer returns a default renderer for instance events
func DefaultInstanceEventRenderer(events []Event) []string {
	var lines []string
	for _, event := range events {
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

		instRes, ok := event.Resource.(*types.Instance)
		if !ok {
			continue
		}

		eventText := fmt.Sprintf("[%s] %s instance \"%s\"",
			format.Colorize(color, symbol),
			eventPrefix,
			format.Colorize(format.Bold, instRes.Name))

		lines = append(lines, eventText)
	}
	return lines
}
