// Package otelsql is the SQL schema of an OTel store: one fdb-relational
// schema template whose tables hold one tenant's spans, logs, GenAI messages
// and message trace markers, so SQL joins them within one record store.
//
// The template is plain Java fdb-relational 4.14.2 DDL: Java parses it and
// stores the same template bytes (conformance/rfc204_template_golden_test.go).
// Attributes are typed repeated structs, one array per OTLP value type, so a
// fan-out VALUE index holds one entry per attribute and an equality never
// coerces between types. Index keys end with the base record's time and
// primary key, which is what lets Java order a page by them.
package otelsql

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/core/keyspace"
)

// TemplateName names the schema template; a schema of every tenant database
// is created with it.
const TemplateName = "OTEL_V1"

// TemplateVersion is the initial catalog version assigned by CREATE SCHEMA
// TEMPLATE. Evolution must use the catalog's version/evolution validator, not
// overwrite this version with different DDL.
const TemplateVersion = 1

// MaxIndexedStringBytes caps a string attribute value stored in an indexed
// str_value column: an index key over FDB's 10 KB limit fails the write
// (Java StandardIndexMaintainer.checkKeyValueSizes). A longer value is stored
// in long_str_attrs and found by its digest.
const MaxIndexedStringBytes = 1024

// TemplateBody is the template's DDL after CREATE SCHEMA TEMPLATE <name>.
//
// Types: one struct per attribute value type. scope is 'resource', 'scope'
// or the owning record's own scope ('span', 'log'); event and link attributes
// live on their event and link.
//
// Tables:
//   - spans: one row per span, primary key (trace_id, span_id).
//   - logs: one row per log record, primary key event_id; trace_id and
//     span_id are NULL for an uncorrelated log.
//   - messages: one part of a stored GenAI message, primary key
//     (writer, seq, part); spans name them in message_refs. content_hash is
//     the whole-message digest on part zero ONLY; other parts leave it NULL.
//     NULL hashes do not conflict in the unique index. This is a writer
//     contract, not a DDL constraint.
//   - message_traces: a message occurs in a trace.
//
// TEXT is intentionally absent: Java relational has no TEXT DDL/query access.
// A Go FULLTEXT overlay needs its own metadata version and migration policy.
// Typed columns are incomplete projections, not an ingest encoder. Fidelity
// requires the envelope plus lossless message manifests/reference reconstruction;
// storing full GenAI history in every span would restore quadratic storage.
const TemplateBody = `CREATE TYPE AS STRUCT str_attr (scope STRING, attr_key STRING, str_value STRING)
CREATE TYPE AS STRUCT long_str_attr (scope STRING, attr_key STRING, digest BYTES, str_value STRING)
CREATE TYPE AS STRUCT int_attr (scope STRING, attr_key STRING, int_value BIGINT)
CREATE TYPE AS STRUCT double_attr (scope STRING, attr_key STRING, double_value DOUBLE)
CREATE TYPE AS STRUCT bool_attr (scope STRING, attr_key STRING, bool_value BOOLEAN)
CREATE TYPE AS STRUCT bytes_attr (scope STRING, attr_key STRING, bytes_value BYTES)
CREATE TYPE AS STRUCT any_attr (scope STRING, attr_key STRING, json_value STRING)
CREATE TYPE AS STRUCT span_event (ordinal BIGINT, name STRING, time_ns BIGINT, dropped_attributes BIGINT,
  str_attrs str_attr ARRAY, int_attrs int_attr ARRAY, double_attrs double_attr ARRAY,
  bool_attrs bool_attr ARRAY, any_attrs any_attr ARRAY)
CREATE TYPE AS STRUCT span_link (ordinal BIGINT, trace_id BYTES, span_id BYTES, trace_state STRING,
  flags BIGINT, dropped_attributes BIGINT, str_attrs str_attr ARRAY, int_attrs int_attr ARRAY,
  double_attrs double_attr ARRAY, bool_attrs bool_attr ARRAY, any_attrs any_attr ARRAY)
CREATE TYPE AS STRUCT message_ref (attribute BIGINT, writer BIGINT, seq BIGINT, newest BOOLEAN, ordinals BIGINT ARRAY)
CREATE TABLE spans (trace_id BYTES, span_id BYTES, parent_span_id BYTES, trace_state STRING, flags BIGINT,
  name STRING, kind INTEGER, start_ns BIGINT, end_ns BIGINT, status_code INTEGER, status_message STRING,
  service_name STRING, scope_name STRING, scope_version STRING, resource_schema_url STRING, scope_schema_url STRING,
  dropped_attributes BIGINT, dropped_events BIGINT, dropped_links BIGINT,
  str_attrs str_attr ARRAY, long_str_attrs long_str_attr ARRAY, int_attrs int_attr ARRAY,
  double_attrs double_attr ARRAY, bool_attrs bool_attr ARRAY, bytes_attrs bytes_attr ARRAY, any_attrs any_attr ARRAY,
  events span_event ARRAY, links span_link ARRAY, message_refs message_ref ARRAY, otlp BYTES,
  PRIMARY KEY (trace_id, span_id))
CREATE INDEX spans_time AS SELECT start_ns, trace_id, span_id FROM spans ORDER BY start_ns, trace_id, span_id
CREATE INDEX spans_span_id AS SELECT span_id, start_ns, trace_id FROM spans ORDER BY span_id, start_ns, trace_id
CREATE INDEX spans_str_attr AS SELECT a.scope, a.attr_key, a.str_value, s.start_ns, s.trace_id, s.span_id
  FROM spans AS s, (SELECT scope, attr_key, str_value FROM s.str_attrs) AS a
  ORDER BY a.scope, a.attr_key, a.str_value, s.start_ns, s.trace_id, s.span_id
CREATE INDEX spans_long_str_attr AS SELECT a.scope, a.attr_key, a.digest, s.start_ns, s.trace_id, s.span_id
  FROM spans AS s, (SELECT scope, attr_key, digest FROM s.long_str_attrs) AS a
  ORDER BY a.scope, a.attr_key, a.digest, s.start_ns, s.trace_id, s.span_id
CREATE INDEX spans_int_attr AS SELECT a.scope, a.attr_key, a.int_value, s.start_ns, s.trace_id, s.span_id
  FROM spans AS s, (SELECT scope, attr_key, int_value FROM s.int_attrs) AS a
  ORDER BY a.scope, a.attr_key, a.int_value, s.start_ns, s.trace_id, s.span_id
CREATE INDEX spans_double_attr AS SELECT a.scope, a.attr_key, a.double_value, s.start_ns, s.trace_id, s.span_id
  FROM spans AS s, (SELECT scope, attr_key, double_value FROM s.double_attrs) AS a
  ORDER BY a.scope, a.attr_key, a.double_value, s.start_ns, s.trace_id, s.span_id
CREATE INDEX spans_bool_attr AS SELECT a.scope, a.attr_key, a.bool_value, s.start_ns, s.trace_id, s.span_id
  FROM spans AS s, (SELECT scope, attr_key, bool_value FROM s.bool_attrs) AS a
  ORDER BY a.scope, a.attr_key, a.bool_value, s.start_ns, s.trace_id, s.span_id
CREATE TABLE logs (event_id STRING, trace_id BYTES, span_id BYTES, flags BIGINT, time_ns BIGINT,
  observed_ns BIGINT, effective_ns BIGINT, time_source STRING, severity_number INTEGER, severity_text STRING,
  event_name STRING, body_text STRING, service_name STRING, scope_name STRING, scope_version STRING,
  resource_schema_url STRING, scope_schema_url STRING, dropped_attributes BIGINT,
  str_attrs str_attr ARRAY, long_str_attrs long_str_attr ARRAY, int_attrs int_attr ARRAY,
  double_attrs double_attr ARRAY, bool_attrs bool_attr ARRAY, bytes_attrs bytes_attr ARRAY, any_attrs any_attr ARRAY,
  otlp BYTES, PRIMARY KEY (event_id))
CREATE INDEX logs_trace AS SELECT trace_id, span_id, effective_ns, event_id FROM logs
  ORDER BY trace_id, span_id, effective_ns, event_id
CREATE INDEX logs_time AS SELECT effective_ns, event_id FROM logs ORDER BY effective_ns, event_id
CREATE INDEX logs_str_attr AS SELECT a.scope, a.attr_key, a.str_value, l.effective_ns, l.event_id
  FROM logs AS l, (SELECT scope, attr_key, str_value FROM l.str_attrs) AS a
  ORDER BY a.scope, a.attr_key, a.str_value, l.effective_ns, l.event_id
CREATE INDEX logs_int_attr AS SELECT a.scope, a.attr_key, a.int_value, l.effective_ns, l.event_id
  FROM logs AS l, (SELECT scope, attr_key, int_value FROM l.int_attrs) AS a
  ORDER BY a.scope, a.attr_key, a.int_value, l.effective_ns, l.event_id
CREATE TABLE messages (writer BIGINT, seq BIGINT, part BIGINT, content_hash BYTES, role STRING, content STRING,
  token_offset BIGINT, raw BYTES, encoding STRING, raw_bytes BIGINT, parts BIGINT, unindexed_tokens BIGINT,
  large BIGINT ARRAY, shard BIGINT, PRIMARY KEY (writer, seq, part))
CREATE UNIQUE INDEX messages_hash AS SELECT content_hash FROM messages ORDER BY content_hash
CREATE TABLE message_traces (writer BIGINT, seq BIGINT, trace_id BYTES, PRIMARY KEY (writer, seq, trace_id))
CREATE INDEX message_traces_trace AS SELECT trace_id, writer, seq FROM message_traces ORDER BY trace_id, writer, seq`

// TemplateDDL is the statement that creates the template.
func TemplateDDL() string {
	return "CREATE SCHEMA TEMPLATE " + TemplateName + " " + TemplateBody
}

// CreateTemplate stores the template in the catalog of the connection's
// cluster.
func CreateTemplate(ctx context.Context, db *sql.DB) error {
	if _, err := db.ExecContext(ctx, TemplateDDL()); err != nil {
		return fmt.Errorf("otelsql: create schema template %s: %w", TemplateName, err)
	}
	return nil
}

// CreateTenant creates a tenant's database and its schema over the template;
// the schema's record store holds every table of that tenant. Paths must be
// /DOMAIN/DATABASE and schema a single identifier, using ASCII letters, digits
// and underscores (not starting with a digit). Like unquoted relational DDL,
// names fold to uppercase. The domain must already be registered.
func CreateTenant(ctx context.Context, db *sql.DB, databasePath, schema string) error {
	databasePath, schemaPath, err := tenantPaths(databasePath, schema)
	if err != nil {
		return err
	}
	if _, err := db.ExecContext(ctx, `CREATE DATABASE "`+databasePath+`"`); err != nil {
		return fmt.Errorf("otelsql: create database %s: %w", databasePath, err)
	}
	if _, err := db.ExecContext(ctx, `CREATE SCHEMA "`+schemaPath+`" WITH TEMPLATE `+TemplateName); err != nil {
		return fmt.Errorf("otelsql: create schema %s: %w", schemaPath, err)
	}
	return nil
}

func tenantPaths(databasePath, schema string) (string, string, error) {
	invalid := func() (string, string, error) {
		return "", "", api.NewErrorf(api.ErrCodeInvalidPath,
			"otelsql: expected /DOMAIN/DATABASE and one schema identifier: %q, %q", databasePath, schema)
	}
	parts, err := keyspace.ParseDBPath(databasePath)
	if err != nil || len(parts) != 2 {
		return invalid()
	}
	for _, name := range []string{parts[0], parts[1], schema} {
		if name == "" {
			return invalid()
		}
		for i, c := range name {
			if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || c == '_' || (i > 0 && c >= '0' && c <= '9')) {
				return invalid()
			}
		}
	}
	databasePath = strings.ToUpper(databasePath)
	resolved, err := keyspace.ToDatabasePath(databasePath)
	if err != nil || resolved.System {
		return invalid()
	}
	return databasePath, databasePath + "/" + strings.ToUpper(schema), nil
}
