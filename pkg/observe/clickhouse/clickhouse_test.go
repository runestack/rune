package clickhouse

import (
	"strings"
	"testing"
	"time"

	"github.com/runestack/rune/pkg/observe"
)

func TestBuildLogSQL(t *testing.T) {
	start := time.Date(2026, 6, 6, 11, 0, 0, 0, time.UTC)
	end := time.Date(2026, 6, 6, 12, 0, 0, 0, time.UTC)
	q := &observe.Query{
		Selectors: []observe.Matcher{
			{Label: "service", Op: observe.MatchEqual, Value: "api"},
			{Label: "instance", Op: observe.MatchRegex, Value: "api-.*"},
			{Label: "app", Op: observe.MatchEqual, Value: "web"}, // custom => labels Map
		},
		LineFilters: []observe.LineFilter{
			{Op: observe.LineContains, Value: "boom"},
			{Op: observe.LineNotContains, Value: "noise"},
		},
		Start:     start,
		End:       end,
		Direction: observe.DirectionBackward,
	}
	sql, args, err := buildLogSQL("runesight", "logs", q)
	if err != nil {
		t.Fatal(err)
	}
	want := "SELECT timestamp, line, stream, level, namespace, service, instance, node, labels " +
		"FROM `runesight`.`logs` " +
		"WHERE service = ? AND match(instance, ?) AND labels['app'] = ? AND position(line, ?) > 0 AND position(line, ?) = 0 " +
		"AND timestamp >= ? AND timestamp < ? " +
		"ORDER BY timestamp DESC LIMIT 1000"
	if sql != want {
		t.Fatalf("SQL mismatch:\n got: %s\nwant: %s", sql, want)
	}
	wantArgs := []any{"api", "api-.*", "web", "boom", "noise", start, end}
	if len(args) != len(wantArgs) {
		t.Fatalf("arg count = %d, want %d (%v)", len(args), len(wantArgs), args)
	}
	for i := range wantArgs {
		if args[i] != wantArgs[i] {
			t.Errorf("arg[%d] = %v, want %v", i, args[i], wantArgs[i])
		}
	}
}

func TestBuildLogSQL_ForwardDirectionAndLimit(t *testing.T) {
	q := &observe.Query{
		Selectors: []observe.Matcher{{Label: "service", Op: observe.MatchEqual, Value: "api"}},
		Direction: observe.DirectionForward,
		Limit:     50,
	}
	sql, _, err := buildLogSQL("db", "t", q)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(sql, "ORDER BY timestamp ASC LIMIT 50") {
		t.Fatalf("want forward order + limit 50, got %s", sql)
	}
}

func TestBuildLogSQL_RejectsInvalidLabelKey(t *testing.T) {
	q := &observe.Query{
		Selectors: []observe.Matcher{{Label: "bad-key", Op: observe.MatchEqual, Value: "x"}},
	}
	if _, _, err := buildLogSQL("db", "t", q); err == nil {
		t.Fatal("want error for invalid label key, got nil")
	}
}

func TestBuildMetricSQL_CountGrouped(t *testing.T) {
	q := &observe.Query{
		Selectors:   []observe.Matcher{{Label: "service", Op: observe.MatchEqual, Value: "api"}},
		Aggregation: &observe.Aggregation{Op: observe.AggCountOverTime, Step: time.Minute, GroupBy: []string{"level"}},
	}
	sql, args, groupNames, err := buildMetricSQL("runesight", "logs", q)
	if err != nil {
		t.Fatal(err)
	}
	want := "SELECT toStartOfInterval(timestamp, INTERVAL 60 SECOND) AS bucket, level AS `level`, toFloat64(count()) AS value " +
		"FROM `runesight`.`logs` WHERE service = ? GROUP BY bucket, level ORDER BY bucket ASC"
	if sql != want {
		t.Fatalf("SQL mismatch:\n got: %s\nwant: %s", sql, want)
	}
	if len(args) != 1 || args[0] != "api" {
		t.Errorf("args = %v, want [api]", args)
	}
	if len(groupNames) != 1 || groupNames[0] != "level" {
		t.Errorf("groupNames = %v, want [level]", groupNames)
	}
}

func TestBuildMetricSQL_Ops(t *testing.T) {
	base := &observe.Query{Selectors: []observe.Matcher{{Label: "service", Op: observe.MatchEqual, Value: "api"}}}
	cases := []struct {
		name     string
		agg      *observe.Aggregation
		contains string
	}{
		{"rate", &observe.Aggregation{Op: observe.AggRateOverTime, Step: time.Minute}, "count() / 60 AS value"},
		{"bytes", &observe.Aggregation{Op: observe.AggBytesOverTime, Step: time.Minute}, "toFloat64(sum(length(line))) AS value"},
		{"quantile", &observe.Aggregation{Op: observe.AggQuantileOverTime, Step: time.Minute, Quantile: 0.99, Field: "dur"}, "ifNull(quantile(0.99)(toFloat64OrNull(labels['dur'])), 0) AS value"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			q := *base
			q.Aggregation = c.agg
			sql, _, _, err := buildMetricSQL("db", "t", &q)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(sql, c.contains) {
				t.Fatalf("want SQL to contain %q, got %s", c.contains, sql)
			}
		})
	}
}

func TestBuildLabelSQL(t *testing.T) {
	values, args, err := buildLabelValuesSQL("db", "t", observe.Selector{
		Name:  "service",
		Match: []observe.Matcher{{Label: "namespace", Op: observe.MatchEqual, Value: "default"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(values, "SELECT service AS v, count() AS c FROM `db`.`t` WHERE namespace = ? AND service != '' GROUP BY v ORDER BY c DESC, v ASC LIMIT 1000") {
		t.Fatalf("label-values SQL wrong: %s", values)
	}
	if len(args) != 1 || args[0] != "default" {
		t.Errorf("args = %v, want [default]", args)
	}

	names, _, err := buildLabelNamesSQL("db", "t", observe.Selector{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(names, "arrayJoin(mapKeys(labels))") {
		t.Fatalf("label-names SQL wrong: %s", names)
	}
}

func TestBuildCreateTableDDL_WithTiering(t *testing.T) {
	ddl := buildCreateTableDDL(Config{
		Database:      "runesight",
		Table:         "logs",
		StoragePolicy: "runesight_tiered",
		HotDays:       7,
		RetentionDays: 30,
	})

	for _, want := range []string{
		"CREATE TABLE IF NOT EXISTS `runesight`.`logs`",
		"timestamp DateTime64(9)",
		"labels Map(LowCardinality(String), String)",
		"INDEX idx_line line TYPE tokenbf_v1",
		"ENGINE = MergeTree",
		"PARTITION BY toDate(timestamp)",
		"ORDER BY (namespace, service, timestamp)",
		"TTL timestamp + INTERVAL 7 DAY TO VOLUME 's3', timestamp + INTERVAL 30 DAY DELETE",
		"SETTINGS storage_policy = 'runesight_tiered'",
	} {
		if !strings.Contains(ddl, want) {
			t.Errorf("DDL missing %q\n---\n%s", want, ddl)
		}
	}
}

func TestBuildCreateTableDDL_NoTieringByDefault(t *testing.T) {
	ddl := buildCreateTableDDL(Config{Database: "runesight", Table: "logs"})
	if strings.Contains(ddl, "TTL ") {
		t.Errorf("did not expect a TTL clause without retention/tiering config:\n%s", ddl)
	}
	if strings.Contains(ddl, "storage_policy") {
		t.Errorf("did not expect storage_policy without StoragePolicy config:\n%s", ddl)
	}
}

func TestBuildCreateTableDDL_RetentionOnlyNoMove(t *testing.T) {
	// HotDays set but no StoragePolicy => no move-to-volume (nowhere to move),
	// retention DELETE still applies.
	ddl := buildCreateTableDDL(Config{Database: "db", Table: "t", HotDays: 7, RetentionDays: 30})
	if strings.Contains(ddl, "TO VOLUME") {
		t.Errorf("must not emit TO VOLUME without a storage policy:\n%s", ddl)
	}
	if !strings.Contains(ddl, "TTL timestamp + INTERVAL 30 DAY DELETE") {
		t.Errorf("want retention DELETE TTL:\n%s", ddl)
	}
}

func TestNew_Defaults(t *testing.T) {
	s, err := New(Config{DSN: "clickhouse://localhost:9000"})
	if err != nil {
		t.Fatal(err)
	}
	if s.cfg.Database != "runesight" || s.cfg.Table != "logs" {
		t.Errorf("defaults not applied: db=%q table=%q", s.cfg.Database, s.cfg.Table)
	}
	if _, err := New(Config{}); err == nil {
		t.Error("want error when DSN missing")
	}
}

func TestCapabilities_Advanced(t *testing.T) {
	s, _ := New(Config{DSN: "clickhouse://localhost:9000"})
	c := s.Capabilities()
	if c.MaxTier != observe.TierAdvanced || !c.RawSQL || !c.Percentiles || !c.HighCardinalityFilters {
		t.Errorf("clickhouse must advertise Advanced tier: %+v", c)
	}
}
