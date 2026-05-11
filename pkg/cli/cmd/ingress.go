// Package cmd: `rune ingress` command family (RUNE-067).
//
// Surfaces the per-service ingress + TLS lifecycle owned by the
// ingress controller and ACME orchestrator (RUNE-066). All commands
// are thin views over the existing Service API: a service is
// considered "exposed" when Spec.Expose.Host is set, and its
// IngressCertStatus is read directly off the Service. There is no
// new server-side endpoint in v1.
package cmd

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/runestack/rune/pkg/types"
	"gopkg.in/yaml.v3"
)

// ingressRow is the projected view of a single exposed service used
// by both the table and structured outputs.
type ingressRow struct {
	Namespace string                   `json:"namespace" yaml:"namespace"`
	Service   string                   `json:"service" yaml:"service"`
	Host      string                   `json:"host" yaml:"host"`
	Path      string                   `json:"path,omitempty" yaml:"path,omitempty"`
	Port      string                   `json:"port,omitempty" yaml:"port,omitempty"`
	TLSMode   string                   `json:"tlsMode,omitempty" yaml:"tlsMode,omitempty"`
	Cert      *types.IngressCertStatus `json:"cert,omitempty" yaml:"cert,omitempty"`
}

// projectIngressRow returns the projected view of a service, plus
// false if the service is not exposed via the ingress controller.
func projectIngressRow(svc *types.Service) (ingressRow, bool) {
	if svc == nil || svc.Expose == nil || svc.Expose.Host == "" {
		return ingressRow{}, false
	}
	row := ingressRow{
		Namespace: svc.Namespace,
		Service:   svc.ID,
		Host:      svc.Expose.Host,
		Path:      svc.Expose.Path,
		Port:      svc.Expose.Port,
		Cert:      svc.IngressCert,
	}
	if svc.Expose.TLS != nil {
		switch {
		case svc.Expose.TLS.IsACME():
			row.TLSMode = types.ExposeTLSModeACME
		case svc.Expose.TLS.SecretName != "":
			row.TLSMode = types.ExposeTLSModeManual
		}
	}
	return row, true
}

// collectIngressRows projects and sorts a service list for stable
// output ordering.
func collectIngressRows(svcs []*types.Service) []ingressRow {
	rows := make([]ingressRow, 0, len(svcs))
	for _, s := range svcs {
		if r, ok := projectIngressRow(s); ok {
			rows = append(rows, r)
		}
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Namespace != rows[j].Namespace {
			return rows[i].Namespace < rows[j].Namespace
		}
		if rows[i].Host != rows[j].Host {
			return rows[i].Host < rows[j].Host
		}
		return rows[i].Service < rows[j].Service
	})
	return rows
}

func writeIngressRows(w io.Writer, rows []ingressRow, output string) error {
	switch strings.ToLower(output) {
	case "json":
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(rows)
	case "yaml":
		return yaml.NewEncoder(w).Encode(rows)
	default:
		return printIngressTable(w, rows)
	}
}

func writeIngressDetail(w io.Writer, row ingressRow, output string) error {
	switch strings.ToLower(output) {
	case "json":
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(row)
	case "yaml":
		return yaml.NewEncoder(w).Encode(row)
	default:
		return printIngressDetail(w, row)
	}
}

func printIngressTable(w io.Writer, rows []ingressRow) error {
	if len(rows) == 0 {
		_, err := fmt.Fprintln(w, "No exposed services found.")
		return err
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	if _, err := fmt.Fprintln(tw, "NAMESPACE\tSERVICE\tHOST\tTLS\tCERT\tEXPIRES"); err != nil {
		return err
	}
	for _, r := range rows {
		state, expires := certCols(r.Cert)
		tls := r.TLSMode
		if tls == "" {
			tls = "-"
		}
		if _, err := fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n",
			emptyDash(r.Namespace), r.Service, r.Host, tls, state, expires); err != nil {
			return err
		}
	}
	return tw.Flush()
}

func printIngressDetail(w io.Writer, r ingressRow) error {
	fmt.Fprintf(w, "service:   %s/%s\n", emptyDash(r.Namespace), r.Service)
	fmt.Fprintf(w, "host:      %s\n", r.Host)
	if r.Path != "" {
		fmt.Fprintf(w, "path:      %s\n", r.Path)
	}
	if r.Port != "" {
		fmt.Fprintf(w, "port:      %s\n", r.Port)
	}
	tls := r.TLSMode
	if tls == "" {
		tls = "(none)"
	}
	fmt.Fprintf(w, "tls mode:  %s\n", tls)
	if r.Cert == nil {
		fmt.Fprintln(w, "cert:      (none)")
		return nil
	}
	c := r.Cert
	fmt.Fprintf(w, "cert:\n")
	fmt.Fprintf(w, "  state:   %s\n", c.State)
	fmt.Fprintf(w, "  host:    %s\n", c.Host)
	if c.IssuedAt != nil {
		fmt.Fprintf(w, "  issued:  %s\n", c.IssuedAt.UTC().Format(time.RFC3339))
	}
	if c.ExpiresAt != nil {
		fmt.Fprintf(w, "  expires: %s (in %s)\n",
			c.ExpiresAt.UTC().Format(time.RFC3339),
			truncDuration(time.Until(*c.ExpiresAt)))
	}
	if c.LastError != "" {
		fmt.Fprintf(w, "  error:   %s\n", c.LastError)
	}
	if c.NextRetry != nil {
		fmt.Fprintf(w, "  retry:   %s\n", c.NextRetry.UTC().Format(time.RFC3339))
	}
	return nil
}

// certCols renders the (state, expires-in) columns for the table.
func certCols(c *types.IngressCertStatus) (string, string) {
	if c == nil {
		return "-", "-"
	}
	state := string(c.State)
	if state == "" {
		state = "-"
	}
	exp := "-"
	if c.ExpiresAt != nil {
		exp = truncDuration(time.Until(*c.ExpiresAt))
	}
	return state, exp
}

// truncDuration renders a duration with day/hour granularity for
// the operator-facing table. Negative durations are rendered as
// "expired".
func truncDuration(d time.Duration) string {
	if d <= 0 {
		return "expired"
	}
	if d >= 24*time.Hour {
		return fmt.Sprintf("%dd", int(d/(24*time.Hour)))
	}
	if d >= time.Hour {
		return fmt.Sprintf("%dh", int(d/time.Hour))
	}
	if d >= time.Minute {
		return fmt.Sprintf("%dm", int(d/time.Minute))
	}
	return fmt.Sprintf("%ds", int(d/time.Second))
}

func emptyDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
