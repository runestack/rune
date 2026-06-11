package observe_test

import (
	"testing"
	"time"

	"github.com/runestack/rune/pkg/observe"
)

func TestExtractFields_Logfmt(t *testing.T) {
	got := observe.ExtractFields(
		`level=info msg="payment ok" dur=231 method=POST path=/api/pay ok=true`,
		[]observe.ParserStage{observe.ParserLogfmt})
	want := map[string]string{
		"level": "info", "msg": "payment ok", "dur": "231",
		"method": "POST", "path": "/api/pay", "ok": "true",
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("logfmt[%s] = %q, want %q (all: %v)", k, got[k], v, got)
		}
	}

	// Non-logfmt lines extract nothing — and never error.
	if got := observe.ExtractFields("plain text line", []observe.ParserStage{observe.ParserLogfmt}); len(got) != 0 {
		t.Errorf("plain line extracted %v", got)
	}
	// Bad keys are dropped rather than minting unaddressable labels.
	if got := observe.ExtractFields(`a-b=1 good=2`, []observe.ParserStage{observe.ParserLogfmt}); got["good"] != "2" || len(got) != 1 {
		t.Errorf("key sanitation wrong: %v", got)
	}
	// Unterminated quote takes the rest raw.
	if got := observe.ExtractFields(`msg="oops`, []observe.ParserStage{observe.ParserLogfmt}); got["msg"] != "oops" {
		t.Errorf("unterminated quote: %v", got)
	}
}

func TestExtractFields_JSON(t *testing.T) {
	got := observe.ExtractFields(
		`{"level":"error","status":500,"dur":12.5,"ok":false,"nested":{"x":1},"arr":[1],"nil":null}`,
		[]observe.ParserStage{observe.ParserJSON})
	want := map[string]string{"level": "error", "status": "500", "dur": "12.5", "ok": "false", "nil": ""}
	for k, v := range want {
		if gv, ok := got[k]; !ok || gv != v {
			t.Errorf("json[%s] = %q, want %q (all: %v)", k, gv, v, got)
		}
	}
	if _, ok := got["nested"]; ok {
		t.Error("nested objects must be skipped")
	}
	if got := observe.ExtractFields("not json", []observe.ParserStage{observe.ParserJSON}); len(got) != 0 {
		t.Errorf("non-json extracted %v", got)
	}
}

func TestCompileLabelFilters(t *testing.T) {
	// Numeric filter with a non-numeric value is a compile error.
	if _, err := observe.CompileLabelFilters([]observe.LabelFilter{
		{Label: "dur", Op: observe.LabelFilterGT, Value: "fast"},
	}); err == nil {
		t.Fatal("want error for non-numeric threshold")
	}
	// Bad regex is a compile error.
	if _, err := observe.CompileLabelFilters([]observe.LabelFilter{
		{Label: "path", Op: observe.LabelFilterRe, Value: "("},
	}); err == nil {
		t.Fatal("want error for bad regex")
	}

	cfs, err := observe.CompileLabelFilters([]observe.LabelFilter{
		{Label: "status", Op: observe.LabelFilterGTE, Value: "500"},
		{Label: "method", Op: observe.LabelFilterEq, Value: "POST"},
	})
	if err != nil {
		t.Fatal(err)
	}
	row := map[string]string{"status": "503", "method": "POST"}
	get := func(k string) (string, bool) { v, ok := row[k]; return v, ok }
	if !cfs.Matches(get) {
		t.Error("503 POST should match")
	}
	row["status"] = "200"
	if cfs.Matches(get) {
		t.Error("200 should not match >= 500")
	}
	// Missing or non-numeric label fails numeric filters.
	delete(row, "status")
	if cfs.Matches(get) {
		t.Error("missing label must fail numeric filter")
	}
}

func TestParseLogQL_Pipeline(t *testing.T) {
	q, err := observe.ParseLogQL(
		`{service="api"} |= "pay" | logfmt | status >= 500 | method = "POST"`,
		time.Time{}, time.Time{}, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(q.LineFilters) != 1 || q.LineFilters[0].Value != "pay" {
		t.Errorf("line filters: %+v", q.LineFilters)
	}
	if len(q.Parsers) != 1 || q.Parsers[0] != observe.ParserLogfmt {
		t.Errorf("parsers: %+v", q.Parsers)
	}
	if len(q.LabelFilters) != 2 {
		t.Fatalf("label filters: %+v", q.LabelFilters)
	}
	if q.LabelFilters[0] != (observe.LabelFilter{Label: "status", Op: observe.LabelFilterGTE, Value: "500"}) {
		t.Errorf("lf0: %+v", q.LabelFilters[0])
	}
	if q.LabelFilters[1] != (observe.LabelFilter{Label: "method", Op: observe.LabelFilterEq, Value: "POST"}) {
		t.Errorf("lf1: %+v", q.LabelFilters[1])
	}

	// Inside a metric query too: group by an extracted label.
	mq, err := observe.ParseLogQL(
		`sum by (status) (count_over_time({service="api"} | json [5m]))`,
		time.Time{}, time.Time{}, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(mq.Parsers) != 1 || mq.Parsers[0] != observe.ParserJSON || mq.Aggregation == nil {
		t.Errorf("metric pipeline: parsers=%v agg=%v", mq.Parsers, mq.Aggregation)
	}

	// Rejections.
	for _, bad := range []string{
		`{service="api"} | frobnicate`,            // unknown stage
		`{service="api"} | json | path =~ re`,     // unquoted regex
		`{service="api"} | logfmt | dur >`,        // missing value
		`{service="api"} | logfmt | dur > "slow"`, // handled at compile: quoted non-number passes parse...
	} {
		if bad == `{service="api"} | logfmt | dur > "slow"` {
			// parses, but the embedded store rejects at compile time.
			q, err := observe.ParseLogQL(bad, time.Time{}, time.Time{}, 0, false)
			if err != nil {
				t.Errorf("ParseLogQL(%q) should parse, got %v", bad, err)
				continue
			}
			if _, err := observe.CompileLabelFilters(q.LabelFilters); err == nil {
				t.Errorf("CompileLabelFilters(%q) should reject non-numeric threshold", bad)
			}
			continue
		}
		if _, err := observe.ParseLogQL(bad, time.Time{}, time.Time{}, 0, false); err == nil {
			t.Errorf("ParseLogQL(%q) = nil error, want error", bad)
		}
	}
}
