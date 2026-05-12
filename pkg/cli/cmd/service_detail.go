// Package cmd: rendering for the single-service detail view.
//
// `rune get service <name>` is a detail view, not a one-row table.
// When a service is unhealthy this is the screen the developer reads
// to learn *why* — without typing a second command. The shape is
// deliberately a short paragraph plus a per-instance list, not a
// k8s-style describe page.
package cmd

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/fatih/color"
	"github.com/runestack/rune/pkg/api/client"
	"github.com/runestack/rune/pkg/types"
)

// renderServiceDetail prints a single Service in the human-readable
// "paragraph" form. Instances are fetched via instClient so we can show
// per-instance status and the Status Message that explains a failure.
//
// Both clients are required; pass nil only in tests where no instance
// detail is wanted.
func renderServiceDetail(w io.Writer, svc *types.Service, instClient *client.InstanceClient) error {
	if svc == nil {
		return fmt.Errorf("service is nil")
	}

	// Header line: "<name>  <namespace>                       <Status> for <age>"
	statusWord := colorizeServiceStatus(svc.Status)
	age := humanAge(serviceUpdated(svc))
	fmt.Fprintf(w, "%s  %s   %s for %s\n",
		bold(svc.Name), dim(svc.Namespace), statusWord, age)
	fmt.Fprintln(w)

	// Spec summary line.
	specBits := []string{}
	if svc.Image != "" {
		specBits = append(specBits, svc.Image)
	}
	specBits = append(specBits, fmt.Sprintf("scale %d", svc.Scale))
	if r := readyCount(svc, instClient); r != "" {
		specBits = append(specBits, fmt.Sprintf("%s ready", r))
	}
	if svc.ImagePull != "" && svc.ImagePull != "always" {
		specBits = append(specBits, fmt.Sprintf("imagePull: %s", svc.ImagePull))
	}
	fmt.Fprintf(w, "  %s\n", strings.Join(specBits, " · "))

	// Exposure / TLS line, only when relevant.
	if svc.Expose != nil && svc.Expose.Host != "" {
		scheme := "http"
		if svc.Expose.TLS != nil {
			scheme = "https"
		}
		exposed := fmt.Sprintf("exposed at %s://%s%s", scheme, svc.Expose.Host, svc.Expose.Path)
		if svc.IngressCert != nil {
			exposed += fmt.Sprintf("  ·  TLS %s", string(svc.IngressCert.State))
			if svc.IngressCert.ExpiresAt != nil {
				exposed += fmt.Sprintf(" (expires %s)", humanUntil(*svc.IngressCert.ExpiresAt))
			}
		}
		fmt.Fprintf(w, "  %s\n", exposed)
	}

	// Service-level failure block. The reconciler rolls the worst
	// instance's StatusMessage up to Service.StatusMessage so this
	// renders even before we list instances.
	if svc.Status == types.ServiceStatusFailed && (svc.StatusReason != "" || svc.StatusMessage != "") {
		fmt.Fprintln(w)
		reason := svc.StatusReason
		if reason == "" {
			reason = "Failed"
		}
		fmt.Fprintf(w, "  %s %s\n", red("✗"), bold(reason))
		if svc.StatusMessage != "" {
			fmt.Fprintf(w, "    %s\n", svc.StatusMessage)
		}
	}

	// Volumes block (RUNE-070/072). Show declared mounts so a developer
	// can immediately see which Volumes back the service. Binding/Node
	// state lives on the Volume itself (`rune get volume`); we keep this
	// section short — name, mountPath, and how the volume is sourced.
	renderServiceVolumes(w, svc)

	// Instance list. Skip the API call if the caller didn't pass a
	// client (tests).
	if instClient == nil {
		fmt.Fprintln(w)
		printDetailHints(w, svc)
		return nil
	}

	instances, err := instClient.ListInstances(svc.Namespace, svc.Name, "", "")
	if err != nil {
		// Don't fail the whole render; just note we couldn't reach the
		// instance list. The user still got the spec/status block.
		fmt.Fprintf(w, "\n  %s could not list instances: %v\n", yellow("!"), err)
		printDetailHints(w, svc)
		return nil
	}

	if len(instances) == 0 {
		fmt.Fprintf(w, "\n  no instances yet\n")
	} else {
		fmt.Fprintln(w)
		fmt.Fprintf(w, "  Instances (%d):\n", len(instances))
		// Sort: failed first, then pending, then running. Within a
		// bucket, by name for determinism.
		sort.SliceStable(instances, func(i, j int) bool {
			a, b := instanceSortKey(instances[i].Status), instanceSortKey(instances[j].Status)
			if a != b {
				return a < b
			}
			return instances[i].Name < instances[j].Name
		})
		for _, inst := range instances {
			renderInstanceLine(w, inst)
		}
	}

	fmt.Fprintln(w)
	printDetailHints(w, svc)
	return nil
}

// renderInstanceLine prints one instance in the per-instance block.
// For unhealthy instances, the StatusMessage is shown indented
// underneath. This is the developer's "why" without describe/events.
func renderInstanceLine(w io.Writer, inst *types.Instance) {
	mark := "✓"
	colorize := green
	switch inst.Status {
	case types.InstanceStatusFailed, types.InstanceStatusExited, types.InstanceStatusUnknown:
		mark, colorize = "✗", red
	case types.InstanceStatusPending, types.InstanceStatusCreated, types.InstanceStatusStarting:
		mark, colorize = "·", yellow
	}

	node := inst.NodeID
	if node == "" {
		node = "-"
	}

	restarts := ""
	if inst.Metadata != nil && inst.Metadata.RestartCount > 0 {
		restarts = fmt.Sprintf("  ×%d", inst.Metadata.RestartCount)
	}

	uptime := ""
	if inst.Status == types.InstanceStatusRunning && !inst.UpdatedAt.IsZero() {
		uptime = "  " + humanAge(inst.UpdatedAt)
	}

	fmt.Fprintf(w, "    %s %-24s  %-14s  %s%s%s\n",
		colorize(mark),
		inst.Name,
		node,
		colorize(string(inst.Status)),
		uptime,
		restarts,
	)
	if inst.StatusMessage != "" &&
		(inst.Status == types.InstanceStatusFailed ||
			inst.Status == types.InstanceStatusExited ||
			inst.Status == types.InstanceStatusUnknown) {
		fmt.Fprintf(w, "        %s\n", inst.StatusMessage)
	}
}

// renderServiceVolumes prints a one-line-per-mount block summarising
// the volumes wired into the service. We deliberately do NOT fetch the
// underlying Volume objects here — keeping this view a pure spec
// summary means it always renders even when the API server is wedged
// or the volumes have been GC'd. For binding state, point users at
// `rune get volume`.
func renderServiceVolumes(w io.Writer, svc *types.Service) {
	if len(svc.Volumes) == 0 {
		return
	}
	fmt.Fprintln(w)
	fmt.Fprintf(w, "  Volumes (%d):\n", len(svc.Volumes))

	mounts := make([]types.VolumeMount, len(svc.Volumes))
	copy(mounts, svc.Volumes)
	sort.SliceStable(mounts, func(i, j int) bool {
		return mounts[i].MountPath < mounts[j].MountPath
	})
	for _, m := range mounts {
		source := "?"
		switch {
		case m.Claim != nil:
			source = "claim:" + m.Claim.Name
		case m.ClaimTemplate != nil:
			ct := m.ClaimTemplate
			parts := []string{"claimTemplate"}
			if ct.StorageClassName != "" {
				parts = append(parts, "class="+ct.StorageClassName)
			}
			if ct.Size != "" {
				parts = append(parts, "size="+ct.Size)
			}
			if ct.AccessMode != "" {
				parts = append(parts, "mode="+string(ct.AccessMode))
			}
			source = strings.Join(parts, " ")
		}
		ro := ""
		if m.ReadOnly {
			ro = " (ro)"
		}
		sub := ""
		if m.SubPath != "" {
			sub = " subPath=" + m.SubPath
		}
		fmt.Fprintf(w, "    %-16s %s%s%s  %s\n",
			m.Name, m.MountPath, ro, sub, dim(source))
	}
}

// printDetailHints prints exactly one suggested next command. Keep it
// to one — multiple hints become wallpaper users tune out.
func printDetailHints(w io.Writer, svc *types.Service) {
	cmd := fmt.Sprintf("rune logs %s -n %s --tail=50", svc.Name, svc.Namespace)
	if svc.Status == types.ServiceStatusFailed {
		fmt.Fprintf(w, "  %s\n", dim(cmd))
	} else {
		fmt.Fprintf(w, "  %s\n", dim(cmd))
	}
}

// instanceSortKey orders failed (0) before pending (1) before
// running (2) before everything else (3). Used so the developer sees
// the broken thing first when there are many instances.
func instanceSortKey(s types.InstanceStatus) int {
	switch s {
	case types.InstanceStatusFailed, types.InstanceStatusExited, types.InstanceStatusUnknown:
		return 0
	case types.InstanceStatusPending, types.InstanceStatusCreated, types.InstanceStatusStarting:
		return 1
	case types.InstanceStatusRunning:
		return 2
	default:
		return 3
	}
}

// readyCount returns "n/m" if we can compute it from the instance list,
// or "" otherwise. It's intentionally cheap — one extra list call we
// were already making for the instance section.
func readyCount(svc *types.Service, instClient *client.InstanceClient) string {
	if svc.Scale == 0 {
		return ""
	}
	if instClient == nil {
		return ""
	}
	insts, err := instClient.ListInstances(svc.Namespace, svc.Name, "", "")
	if err != nil {
		return ""
	}
	running := 0
	for _, in := range insts {
		if in.Status == types.InstanceStatusRunning {
			running++
		}
	}
	return fmt.Sprintf("%d/%d", running, svc.Scale)
}

// serviceUpdated returns the most useful timestamp for the header.
// Updated > Created > now.
func serviceUpdated(svc *types.Service) time.Time {
	if svc.Metadata != nil && !svc.Metadata.UpdatedAt.IsZero() {
		return svc.Metadata.UpdatedAt
	}
	if svc.Metadata != nil && !svc.Metadata.CreatedAt.IsZero() {
		return svc.Metadata.CreatedAt
	}
	return time.Now()
}

// humanAge returns a short duration like "14m" or "2d".
func humanAge(t time.Time) string {
	if t.IsZero() {
		return "0s"
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}

// humanUntil is the symmetric form for future timestamps ("89d").
func humanUntil(t time.Time) string {
	if t.IsZero() {
		return "?"
	}
	d := time.Until(t)
	if d < 0 {
		return "expired"
	}
	switch {
	case d < time.Hour:
		return fmt.Sprintf("in %dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("in %dh", int(d.Hours()))
	default:
		return fmt.Sprintf("in %dd", int(d.Hours()/24))
	}
}

// colorizeServiceStatus produces a short coloured word for the header
// line of the detail view. We use our own helper rather than the
// table colorizer because we want a different shape ("Failed for 12m"
// vs the table "Failed" pill).
func colorizeServiceStatus(s types.ServiceStatus) string {
	switch s {
	case types.ServiceStatusRunning:
		return green(string(s))
	case types.ServiceStatusFailed:
		return red(string(s))
	case types.ServiceStatusDeploying, types.ServiceStatusPending:
		return yellow(string(s))
	default:
		return string(s)
	}
}

// Tiny colour helpers. Centralised here so the detail view's palette
// is consistent with the rest of the CLI without dragging pterm into
// every render.
var (
	red    = color.New(color.FgRed).SprintFunc()
	green  = color.New(color.FgGreen).SprintFunc()
	yellow = color.New(color.FgYellow).SprintFunc()
	dim    = color.New(color.Faint).SprintFunc()
	bold   = color.New(color.Bold).SprintFunc()
)
