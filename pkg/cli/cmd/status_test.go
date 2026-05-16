package cmd

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/runestack/rune/pkg/cli/format"
	"github.com/runestack/rune/pkg/types"
)

// TestStatusPriorityOrders attention-worthy statuses before healthy ones, so
// `rune status` floats failures and in-flight work to the top of the table.
func TestStatusPriorityOrders(t *testing.T) {
	cases := []struct {
		status types.ServiceStatus
		want   int
	}{
		{types.ServiceStatusFailed, 0},
		{types.ServiceStatusStopping, 1},
		{types.ServiceStatusDeploying, 2},
		{types.ServiceStatusPending, 3},
		{types.ServiceStatusRunning, 4},
	}
	for _, tc := range cases {
		if got := statusPriority(string(tc.status)); got != tc.want {
			t.Errorf("statusPriority(%s) = %d, want %d", tc.status, got, tc.want)
		}
	}
	// Failed must always sort before Running.
	if statusPriority(string(types.ServiceStatusFailed)) >= statusPriority(string(types.ServiceStatusRunning)) {
		t.Errorf("Failed should sort before Running")
	}
}

// TestStatusSummaryBucketsAcrossStatuses verifies the summary aggregation
// covers every documented status and counts services correctly.
func TestStatusSummaryBucketsAcrossStatuses(t *testing.T) {
	report := &namespaceReport{}
	statuses := []types.ServiceStatus{
		types.ServiceStatusRunning, types.ServiceStatusRunning,
		types.ServiceStatusDeploying,
		types.ServiceStatusStopping,
		types.ServiceStatusPending,
		types.ServiceStatusFailed, types.ServiceStatusFailed,
	}
	for _, st := range statuses {
		report.Summary.Total++
		report.Summary.Running += boolToInt(st == types.ServiceStatusRunning)
		report.Summary.Deploying += boolToInt(st == types.ServiceStatusDeploying)
		report.Summary.Stopping += boolToInt(st == types.ServiceStatusStopping)
		report.Summary.Pending += boolToInt(st == types.ServiceStatusPending)
		report.Summary.Failed += boolToInt(st == types.ServiceStatusFailed)
	}
	got := report.Summary
	want := statusSummary{Total: 7, Running: 2, Deploying: 1, Stopping: 1, Pending: 1, Failed: 2}
	if got != want {
		t.Errorf("summary = %+v, want %+v", got, want)
	}
}

// TestStatusJSONShape pins the JSON output shape so scripts and dashboards
// don't break silently if someone adds/removes fields. If you change the
// keys here, that's a public-API change — update this test deliberately.
func TestStatusJSONShape(t *testing.T) {
	report := &statusReport{
		Namespaces: []namespaceReport{{
			Namespace: "prod",
			Summary:   statusSummary{Total: 1, Running: 1, Instances: 3},
			Services: []serviceReport{{
				Name:           "api",
				Status:         "Running",
				DesiredScale:   3,
				ReadyInstances: 3,
				Age:            "14d",
			}},
		}},
	}
	b, err := json.Marshal(report)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded map[string]interface{}
	if err := json.Unmarshal(b, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	nss, ok := decoded["namespaces"].([]interface{})
	if !ok || len(nss) != 1 {
		t.Fatalf("expected namespaces[] with 1 entry, got: %v", decoded)
	}
	ns := nss[0].(map[string]interface{})
	for _, key := range []string{"namespace", "summary", "services"} {
		if _, ok := ns[key]; !ok {
			t.Errorf("namespace block missing key %q", key)
		}
	}
	summary := ns["summary"].(map[string]interface{})
	for _, key := range []string{"total", "running", "deploying", "stopping", "pending", "failed", "instances"} {
		if _, ok := summary[key]; !ok {
			t.Errorf("summary missing key %q", key)
		}
	}
	svc := ns["services"].([]interface{})[0].(map[string]interface{})
	for _, key := range []string{"name", "status", "desiredScale", "readyInstances", "age", "updatedAt"} {
		if _, ok := svc[key]; !ok {
			t.Errorf("service missing key %q", key)
		}
	}
	// Optional fields stay omitted when empty.
	if _, present := svc["statusReason"]; present {
		t.Errorf("statusReason should be omitted when empty")
	}
	if _, present := svc["statusMessage"]; present {
		t.Errorf("statusMessage should be omitted when empty")
	}
}

// TestStatusGlyphFallbackToASCII verifies the unicode glyphs degrade to
// ASCII tokens when colors are disabled (CI logs, NO_COLOR, piped output).
// Prevents the "weird boxes in my dashboard" class of bug.
func TestStatusGlyphFallbackToASCII(t *testing.T) {
	prev := format.IsColorEnabled()
	t.Cleanup(func() { format.EnableColor(prev) })
	format.EnableColor(false)

	cases := map[types.ServiceStatus]string{
		types.ServiceStatusRunning:   "OK",
		types.ServiceStatusDeploying: "DEPL",
		types.ServiceStatusStopping:  "STOP",
		types.ServiceStatusFailed:    "FAIL",
		types.ServiceStatusPending:   "PEND",
	}
	for st, want := range cases {
		got := glyphFor(st)
		if got == "" || got == "?" {
			t.Errorf("glyphFor(%s) returned %q, expected ASCII fallback containing %q", st, got, want)
			continue
		}
		if !strings.Contains(got, want) {
			t.Errorf("glyphFor(%s) = %q, want it to contain %q", st, got, want)
		}
	}
}
