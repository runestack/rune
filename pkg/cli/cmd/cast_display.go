package cmd

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/runestack/rune/pkg/api/client"
	"github.com/runestack/rune/pkg/cli/format"
	"golang.org/x/term"
)

// planActionGlyph maps a plan action to its terraform-style glyph + label.
// Prune is destructive and is flagged as such in the block.
func planActionGlyph(action string) (glyph, label string, destructive bool) {
	switch action {
	case "create":
		return "+", "create", false
	case "update":
		return "~", "update", false
	case "prune":
		return "-", "prune", true
	case "adopt":
		return "↪", "adopt", false
	case "reference":
		return "=", "reference", false
	default:
		return "?", action, false
	}
}

// planActionOrder is the display order of action groups in the plan block.
var planActionOrder = []string{"create", "update", "prune", "adopt", "reference"}

// renderPlanBlock prints the terraform-style plan block (C5): grouped counts
// with per-resource lines, prune flagged as destructive. Returns whether the
// plan contains any destructive (prune) change.
func renderPlanBlock(w io.Writer, releaseName, namespace string, revision int, plan *client.Plan) bool {
	byAction := map[string][]string{}
	for _, ch := range plan.Changes {
		ref := fmt.Sprintf("%s/%s", strings.TrimSuffix(ch.ResourceType, "_class")+classSuffix(ch.ResourceType), ch.Name)
		byAction[ch.Action] = append(byAction[ch.Action], ref)
	}

	header := fmt.Sprintf("Release %q", releaseName)
	if revision > 0 {
		header += fmt.Sprintf(" → revision %d", revision)
	}
	header += fmt.Sprintf("   (namespace: %s)", namespace)
	fmt.Fprintln(w, format.Header("%s", header))

	destructive := false
	for _, action := range planActionOrder {
		refs := byAction[action]
		glyph, label, isDestructive := planActionGlyph(action)
		count := len(refs)
		sort.Strings(refs)
		detail := ""
		if count > 0 {
			detail = "  (" + strings.Join(refs, ", ") + ")"
		}
		line := fmt.Sprintf("  %s %-9s %d%s", glyph, label, count, detail)
		switch {
		case isDestructive && count > 0:
			destructive = true
			fmt.Fprintln(w, format.Warning("%s        ⚠ destructive", line))
		case action == "create" && count > 0:
			fmt.Fprintln(w, format.Success("%s", line))
		default:
			fmt.Fprintln(w, line)
		}
	}

	// Surface conflicts explicitly: a non-applyable plan needs --adopt.
	if !plan.Applyable {
		fmt.Fprintln(w)
		fmt.Fprintln(w, format.Error("Plan has ownership conflicts:"))
		for _, ch := range plan.Changes {
			if ch.Conflict != "" {
				fmt.Fprintf(w, "  - %s/%s: %s\n", ch.ResourceType, ch.Name, ch.Conflict)
			}
		}
		fmt.Fprintln(w, "  pass --adopt to take ownership.")
	}
	return destructive
}

// classSuffix preserves "storageclass" display for the storage_class type.
func classSuffix(rt string) string {
	if rt == "storage_class" {
		return "class"
	}
	return ""
}

// planHasConfirmable reports whether a plan contains changes that require an
// explicit confirm (prune or adopt) before applying (C5).
func planHasConfirmable(plan *client.Plan) bool {
	for _, ch := range plan.Changes {
		if ch.Action == "prune" || ch.Action == "adopt" {
			return true
		}
	}
	return false
}

// planHasPrune reports whether the plan prunes anything (destructive).
func planHasPrune(plan *client.Plan) bool {
	for _, ch := range plan.Changes {
		if ch.Action == "prune" {
			return true
		}
	}
	return false
}

// confirmApply prompts the operator to confirm a plan before applying. It is
// skipped (returns true) when --yes is set, when stdin is not a TTY (CI), or
// when the plan needs no confirmation. Only plans containing prune/adopt prompt.
func confirmApply(plan *client.Plan, opts *castOptions) (bool, error) {
	if opts.yes || opts.detach {
		return true, nil
	}
	if !planHasConfirmable(plan) {
		return true, nil
	}
	// Non-interactive (CI / piped): refuse rather than hang, since the plan is
	// destructive and we have no way to ask.
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		return false, fmt.Errorf("plan requires confirmation (it prunes or adopts resources) but stdin is not a terminal; re-run with --yes to proceed")
	}
	fmt.Fprint(os.Stderr, "\nApply this plan? Only 'yes' will be accepted: ")
	reader := bufio.NewReader(os.Stdin)
	line, err := reader.ReadString('\n')
	if err != nil && err != io.EOF {
		return false, err
	}
	return strings.TrimSpace(strings.ToLower(line)) == "yes", nil
}

// castJSONOutput is the structured --output json payload: the plan plus the
// applied result (C5, table-stakes for CI / the dashboard).
type castJSONOutput struct {
	Release   string             `json:"release"`
	Namespace string             `json:"namespace"`
	Revision  int                `json:"revision,omitempty"`
	Status    string             `json:"status,omitempty"`
	DryRun    bool               `json:"dryRun"`
	Applyable bool               `json:"applyable"`
	Plan      []castJSONChange   `json:"plan"`
	Counts    map[string]int     `json:"counts"`
	Owns      []castJSONResource `json:"owns,omitempty"`
}

type castJSONChange struct {
	Action       string `json:"action"`
	ResourceType string `json:"resourceType"`
	Namespace    string `json:"namespace"`
	Name         string `json:"name"`
	Conflict     string `json:"conflict,omitempty"`
}

type castJSONResource struct {
	ResourceType string `json:"resourceType"`
	Namespace    string `json:"namespace"`
	Name         string `json:"name"`
}

// buildCastJSON assembles the structured output from a plan and (optional)
// applied release.
func buildCastJSON(releaseName, namespace string, dryRun bool, plan *client.Plan, rel *castReleaseResult) castJSONOutput {
	out := castJSONOutput{
		Release:   releaseName,
		Namespace: namespace,
		DryRun:    dryRun,
		Counts:    map[string]int{},
	}
	if plan != nil {
		out.Applyable = plan.Applyable
		for _, ch := range plan.Changes {
			out.Plan = append(out.Plan, castJSONChange{
				Action:       ch.Action,
				ResourceType: ch.ResourceType,
				Namespace:    ch.Namespace,
				Name:         ch.Name,
				Conflict:     ch.Conflict,
			})
			out.Counts[ch.Action]++
		}
	}
	if rel != nil {
		out.Revision = rel.Revision
		out.Status = rel.Status
		for _, o := range rel.Owns {
			out.Owns = append(out.Owns, castJSONResource{
				ResourceType: o.ResourceType,
				Namespace:    o.Namespace,
				Name:         o.Name,
			})
		}
	}
	return out
}

// castReleaseResult is a render-friendly projection of the applied Release used
// by the JSON output (kept decoupled from types.Release to avoid importing it
// here).
type castReleaseResult struct {
	Revision int
	Status   string
	Owns     []castJSONResource
}

// writeCastJSON emits the structured output to stdout.
func writeCastJSON(out castJSONOutput) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(out)
}
