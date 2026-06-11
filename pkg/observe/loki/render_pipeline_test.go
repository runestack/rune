package loki

import (
	"testing"
	"time"

	"github.com/runestack/rune/pkg/observe"
)

// Parser stages and label filters render back to natively-valid Loki LogQL.
func TestRenderLogQL_Pipeline(t *testing.T) {
	cases := []struct{ logql, want string }{
		{`{service="api"} | logfmt | status >= 500`,
			`{service="api"} | logfmt | status >= 500`},
		{`{service="api"} |= "pay" | json | method = "POST" | path =~ "/api/.*"`,
			`{service="api"} |= "pay" | json | method = "POST" | path =~ "/api/.*"`},
		{`sum by (status) (count_over_time({service="api"} | logfmt [5m]))`,
			`sum by (status) (count_over_time({service="api"} | logfmt [300s]))`},
	}
	for _, c := range cases {
		q, err := observe.ParseLogQL(c.logql, time.Time{}, time.Time{}, 0, false)
		if err != nil {
			t.Fatalf("parse %q: %v", c.logql, err)
		}
		got, err := renderLogQL(q)
		if err != nil {
			t.Fatalf("render %q: %v", c.logql, err)
		}
		if got != c.want {
			t.Errorf("render(%q) = %q, want %q", c.logql, got, c.want)
		}
	}
}
