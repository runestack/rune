package clickhouse

import (
	"fmt"
	"strings"
)

// This file builds the ClickHouse schema DDL, including the S3 tiering clauses.
//
// S3 tiering on ClickHouse, unlike Loki, is not automatic: ClickHouse is
// local-disk-first (columnar MergeTree parts on SSD). Tiering is expressed as a
// TTL "move to volume" rule against a server-configured storage policy that
// defines a hot (local) volume and a cold (s3) volume:
//
//	<storage_configuration>
//	  <disks>
//	    <s3><type>s3</type><endpoint>https://...</endpoint>...</s3>
//	  </disks>
//	  <policies>
//	    <runesight_tiered>
//	      <volumes>
//	        <hot><disk>default</disk></hot>
//	        <s3><disk>s3</disk></s3>
//	      </volumes>
//	    </runesight_tiered>
//	  </policies>
//	</storage_configuration>
//
// The disk/policy definition lives in ClickHouse server config (operators own
// the credentials); the table opts into it via SETTINGS storage_policy and a TTL
// that ages parts hot SSD -> S3 -> deleted. buildCreateTableDDL emits those
// clauses; the operator supplies the matching policy.

// buildCreateDatabaseDDL returns the CREATE DATABASE statement.
func buildCreateDatabaseDDL(cfg Config) string {
	return "CREATE DATABASE IF NOT EXISTS " + quoteIdent(cfg.Database)
}

// buildCreateTableDDL returns the CREATE TABLE statement for the log table:
// promoted hot dimensions as typed columns, the remaining labels as a Map, a
// token bloom-filter skip index over the raw line for substring search, and the
// S3 tiering / retention TTL when configured.
func buildCreateTableDDL(cfg Config) string {
	var b strings.Builder
	fmt.Fprintf(&b, "CREATE TABLE IF NOT EXISTS %s (\n", tableRef(cfg.Database, cfg.Table))
	b.WriteString("  timestamp DateTime64(9) CODEC(Delta(8), ZSTD(1)),\n")
	b.WriteString("  namespace LowCardinality(String),\n")
	b.WriteString("  service LowCardinality(String),\n")
	b.WriteString("  instance String,\n")
	b.WriteString("  node LowCardinality(String),\n")
	b.WriteString("  level LowCardinality(String),\n")
	b.WriteString("  stream LowCardinality(String),\n")
	b.WriteString("  line String CODEC(ZSTD(1)),\n")
	b.WriteString("  labels Map(LowCardinality(String), String),\n")
	// tokenbf_v1 skip index accelerates `position(line, 'x')` / token search.
	b.WriteString("  INDEX idx_line line TYPE tokenbf_v1(32768, 3, 0) GRANULARITY 4\n")
	b.WriteString(")\nENGINE = MergeTree\n")
	b.WriteString("PARTITION BY toDate(timestamp)\n")
	b.WriteString("ORDER BY (namespace, service, timestamp)")

	if ttl := buildTTLClause(cfg); ttl != "" {
		b.WriteString("\n")
		b.WriteString(ttl)
	}
	if cfg.StoragePolicy != "" {
		fmt.Fprintf(&b, "\nSETTINGS storage_policy = '%s'", cfg.StoragePolicy)
	}
	return b.String()
}

// buildTTLClause builds the TTL: move parts older than HotDays to the S3 volume
// (the tiering step), then DELETE parts older than RetentionDays. Either part is
// omitted when its config is zero; an empty clause means "keep everything on the
// hot disk forever". The timestamp column is DateTime64, which ClickHouse
// rejects in a TTL expression (it requires DateTime/Date), so it is downcast.
func buildTTLClause(cfg Config) string {
	var rules []string
	if cfg.HotDays > 0 && cfg.StoragePolicy != "" {
		vol := cfg.S3Volume
		if vol == "" {
			vol = "s3"
		}
		rules = append(rules, fmt.Sprintf("toDateTime(timestamp) + INTERVAL %d DAY TO VOLUME '%s'", cfg.HotDays, vol))
	}
	if cfg.RetentionDays > 0 {
		rules = append(rules, fmt.Sprintf("toDateTime(timestamp) + INTERVAL %d DAY DELETE", cfg.RetentionDays))
	}
	if len(rules) == 0 {
		return ""
	}
	return "TTL " + strings.Join(rules, ", ")
}
