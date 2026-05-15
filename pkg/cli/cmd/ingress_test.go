package cmd

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/runestack/rune/pkg/types"
)

func TestProjectIngressRow_NotExposed(t *testing.T) {
	cases := []*types.Service{
		nil,
		{ID: "x"},
		{ID: "x", Expose: &types.ServiceExpose{}},           // missing host
		{ID: "x", Expose: &types.ServiceExpose{Port: "80"}}, // missing host
	}
	for i, svc := range cases {
		if _, ok := projectIngressRow(svc); ok {
			t.Fatalf("case %d: expected not exposed, got ok", i)
		}
	}
}

func TestProjectIngressRow_TLSMode(t *testing.T) {
	svc := &types.Service{
		ID:        "api",
		Namespace: "prod",
		Expose: &types.ServiceExpose{
			Port: "8080",
			Host: "api.example.com",
			Path: "/v1",
			TLS:  &types.ExposeServiceTLS{Mode: types.ExposeTLSModeACME},
		},
	}
	row, ok := projectIngressRow(svc)
	if !ok {
		t.Fatal("expected exposed row")
	}
	if row.TLSMode != types.ExposeTLSModeACME {
		t.Errorf("expected acme TLS mode, got %q", row.TLSMode)
	}
	if row.Service != "api" || row.Namespace != "prod" || row.Host != "api.example.com" || row.Path != "/v1" {
		t.Errorf("row mismatch: %+v", row)
	}

	svc.Expose.TLS = &types.ExposeServiceTLS{Secret: "tls-secret"}
	row, _ = projectIngressRow(svc)
	if row.TLSMode != types.ExposeTLSModeManual {
		t.Errorf("expected manual TLS mode, got %q", row.TLSMode)
	}

	svc.Expose.TLS = &types.ExposeServiceTLS{Auto: true}
	row, _ = projectIngressRow(svc)
	if row.TLSMode != types.ExposeTLSModeACME {
		t.Errorf("auto should imply acme; got %q", row.TLSMode)
	}

	svc.Expose.TLS = nil
	row, _ = projectIngressRow(svc)
	if row.TLSMode != "" {
		t.Errorf("expected empty TLS mode, got %q", row.TLSMode)
	}
}

func TestCollectIngressRows_FiltersAndSorts(t *testing.T) {
	in := []*types.Service{
		{ID: "z", Namespace: "prod", Expose: &types.ServiceExpose{Host: "z.example.com"}},
		{ID: "no-expose", Namespace: "prod"},
		{ID: "a", Namespace: "prod", Expose: &types.ServiceExpose{Host: "a.example.com"}},
		{ID: "a", Namespace: "dev", Expose: &types.ServiceExpose{Host: "a.example.com"}},
	}
	rows := collectIngressRows(in)
	if len(rows) != 3 {
		t.Fatalf("expected 3 exposed rows, got %d", len(rows))
	}
	// dev/a, prod/a, prod/z
	got := []string{rows[0].Namespace + "/" + rows[0].Service, rows[1].Namespace + "/" + rows[1].Service, rows[2].Namespace + "/" + rows[2].Service}
	want := []string{"dev/a", "prod/a", "prod/z"}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("row %d: got %s want %s", i, got[i], want[i])
		}
	}
}

func TestPrintIngressTable_Empty(t *testing.T) {
	var buf bytes.Buffer
	if err := printIngressTable(&buf, nil); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "No exposed services") {
		t.Errorf("expected empty-state message, got %q", buf.String())
	}
}

func TestPrintIngressTable_Headers(t *testing.T) {
	exp := time.Now().Add(48 * time.Hour)
	rows := []ingressRow{{
		Namespace: "prod",
		Service:   "api",
		Host:      "api.example.com",
		TLSMode:   types.ExposeTLSModeACME,
		Cert: &types.IngressCertStatus{
			State:     types.IngressCertIssued,
			Host:      "api.example.com",
			ExpiresAt: &exp,
		},
	}}
	var buf bytes.Buffer
	if err := printIngressTable(&buf, rows); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, want := range []string{"NAMESPACE", "SERVICE", "HOST", "TLS", "CERT", "EXPIRES", "prod", "api", "api.example.com", "acme", "Issued"} {
		if !strings.Contains(out, want) {
			t.Errorf("expected output to contain %q; got:\n%s", want, out)
		}
	}
}

func TestWriteIngressRows_JSON(t *testing.T) {
	rows := []ingressRow{{Namespace: "n", Service: "s", Host: "h", TLSMode: "acme"}}
	var buf bytes.Buffer
	if err := writeIngressRows(&buf, rows, "json"); err != nil {
		t.Fatal(err)
	}
	var decoded []ingressRow
	if err := json.Unmarshal(buf.Bytes(), &decoded); err != nil {
		t.Fatalf("invalid json: %v\n%s", err, buf.String())
	}
	if len(decoded) != 1 || decoded[0].Service != "s" {
		t.Errorf("decode mismatch: %+v", decoded)
	}
}

func TestPrintIngressDetail_Failed(t *testing.T) {
	retry := time.Now().Add(time.Hour)
	row := ingressRow{
		Namespace: "prod",
		Service:   "api",
		Host:      "api.example.com",
		TLSMode:   types.ExposeTLSModeACME,
		Cert: &types.IngressCertStatus{
			State:     types.IngressCertFailed,
			Host:      "api.example.com",
			LastError: "rate limited",
			NextRetry: &retry,
		},
	}
	var buf bytes.Buffer
	if err := printIngressDetail(&buf, row); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, want := range []string{"prod/api", "Failed", "rate limited", "retry:"} {
		if !strings.Contains(out, want) {
			t.Errorf("expected %q; got:\n%s", want, out)
		}
	}
}

func TestTruncDuration(t *testing.T) {
	cases := []struct {
		in   time.Duration
		want string
	}{
		{-time.Second, "expired"},
		{0, "expired"},
		{30 * time.Second, "30s"},
		{5 * time.Minute, "5m"},
		{3 * time.Hour, "3h"},
		{72 * time.Hour, "3d"},
	}
	for _, c := range cases {
		if got := truncDuration(c.in); got != c.want {
			t.Errorf("truncDuration(%s) = %q want %q", c.in, got, c.want)
		}
	}
}
