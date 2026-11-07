// Package otelsql defines the Java-readable OTel SQL schema template.
// See docs/compatibility.md "OTel SQL schema foundation" for writer contracts.
package otelsql

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/core/keyspace"
)

// TemplateName names the schema template; a schema of every tenant database
// is created with it.
const TemplateName = "OTEL_V1"

// TemplateVersion is the initial catalog version; evolution must use the
// catalog's version/evolution validator rather than overwrite its DDL.
const TemplateVersion = 1

// MaxIndexedStringBytes keeps indexed strings within FDB's key limit;
// longer values belong in long_str_attrs and are looked up by digest.
const MaxIndexedStringBytes = 1024

// TemplateBody follows CREATE SCHEMA TEMPLATE <name>; storage and fidelity
// contracts are in docs/compatibility.md "OTel SQL schema foundation". It is
// what Java builds too: TemplateDDL adds the Go-only TemplateOverlay.
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
CREATE TYPE AS STRUCT message_ref_chunk (chunk BIGINT, refs BYTES)
CREATE TABLE spans (trace_id BYTES, span_id BYTES, parent_span_id BYTES, trace_state STRING, flags BIGINT,
  name STRING, kind INTEGER, start_ns BIGINT, end_ns BIGINT, status_code INTEGER, status_message STRING,
  service_name STRING, scope_name STRING, scope_version STRING, resource_schema_url STRING, scope_schema_url STRING,
  dropped_attributes BIGINT, dropped_events BIGINT, dropped_links BIGINT,
  str_attrs str_attr ARRAY, long_str_attrs long_str_attr ARRAY, int_attrs int_attr ARRAY,
  double_attrs double_attr ARRAY, bool_attrs bool_attr ARRAY, bytes_attrs bytes_attr ARRAY, any_attrs any_attr ARRAY,
  events span_event ARRAY, links span_link ARRAY, ref_chunks message_ref_chunk ARRAY, otlp BYTES,
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
CREATE INDEX spans_ref_chunks AS SELECT s.trace_id, s.start_ns, s.span_id, c.chunk, c.refs
  FROM spans AS s, (SELECT chunk, refs FROM s.ref_chunks) AS c
  ORDER BY s.trace_id, s.start_ns, s.span_id, c.chunk, c.refs
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
CREATE INDEX logs_str_attr AS SELECT a.scope, a.attr_key, a.str_value, l.effective_ns, l.event_id, l.trace_id, l.span_id
  FROM logs AS l, (SELECT scope, attr_key, str_value FROM l.str_attrs) AS a
  ORDER BY a.scope, a.attr_key, a.str_value, l.effective_ns, l.event_id, l.trace_id, l.span_id
CREATE INDEX logs_long_str_attr AS SELECT a.scope, a.attr_key, a.digest, l.effective_ns, l.event_id, l.trace_id, l.span_id
  FROM logs AS l, (SELECT scope, attr_key, digest FROM l.long_str_attrs) AS a
  ORDER BY a.scope, a.attr_key, a.digest, l.effective_ns, l.event_id, l.trace_id, l.span_id
CREATE INDEX logs_int_attr AS SELECT a.scope, a.attr_key, a.int_value, l.effective_ns, l.event_id, l.trace_id, l.span_id
  FROM logs AS l, (SELECT scope, attr_key, int_value FROM l.int_attrs) AS a
  ORDER BY a.scope, a.attr_key, a.int_value, l.effective_ns, l.event_id, l.trace_id, l.span_id
CREATE INDEX logs_double_attr AS SELECT a.scope, a.attr_key, a.double_value, l.effective_ns, l.event_id, l.trace_id, l.span_id
  FROM logs AS l, (SELECT scope, attr_key, double_value FROM l.double_attrs) AS a
  ORDER BY a.scope, a.attr_key, a.double_value, l.effective_ns, l.event_id, l.trace_id, l.span_id
CREATE INDEX logs_bool_attr AS SELECT a.scope, a.attr_key, a.bool_value, l.effective_ns, l.event_id, l.trace_id, l.span_id
  FROM logs AS l, (SELECT scope, attr_key, bool_value FROM l.bool_attrs) AS a
  ORDER BY a.scope, a.attr_key, a.bool_value, l.effective_ns, l.event_id, l.trace_id, l.span_id
CREATE INDEX logs_event AS SELECT event_name, effective_ns, event_id, trace_id, span_id FROM logs
  ORDER BY event_name, effective_ns, event_id, trace_id, span_id
CREATE TABLE message_traces (writer BIGINT, seq BIGINT, trace_id BYTES, PRIMARY KEY (writer, seq, trace_id))
CREATE INDEX message_traces_trace AS SELECT trace_id, writer, seq FROM message_traces ORDER BY trace_id, writer, seq
CREATE TABLE store_meta (name STRING, int_value BIGINT, bytes_value BYTES, str_value STRING, PRIMARY KEY (name))
CREATE TABLE messages (writer BIGINT, seq BIGINT, part BIGINT, content_hash BYTES, role STRING, content STRING,
  token_offset BIGINT, raw BYTES, encoding STRING, raw_bytes BIGINT, parts BIGINT, unindexed_tokens BIGINT,
  large BIGINT ARRAY, shard BIGINT, PRIMARY KEY (writer, seq, part))
CREATE UNIQUE INDEX messages_hash AS SELECT content_hash FROM messages ORDER BY content_hash
CREATE INDEX messages_large AS SELECT "L", m.writer, m.seq FROM messages AS m, m.large AS "L"
  ORDER BY "L", m.writer, m.seq`

// TemplateOverlay is the Go-only part of the template, as Java's DDL has no
// FULLTEXT INDEX. Its TEXT index uses the default tokenizer, which Java's
// store opener knows; writers bound overlong tokens with IndexableText.
// TemplateBody's last index clause is on messages, so the overlay's leaves
// the DDL's table order, and with it the records descriptor (union fields,
// record type keys), TemplateBody's: the rfc204 golden covers it.
const TemplateOverlay = `CREATE FULLTEXT INDEX messages_text ON messages(content) PARTITION BY (role, shard)`

// TemplateDDL is the statement that creates the template: TemplateBody and
// TemplateOverlay.
func TemplateDDL() string {
	return "CREATE SCHEMA TEMPLATE " + TemplateName + " " + TemplateBody + "\n" + TemplateOverlay
}

// CreateTemplate stores the template in the catalog of the connection's
// cluster.
func CreateTemplate(ctx context.Context, db *sql.DB) error {
	if _, err := db.ExecContext(ctx, TemplateDDL()); err != nil {
		return fmt.Errorf("otelsql: create schema template %s: %w", TemplateName, err)
	}
	return nil
}

// CreateTenant creates the schema, resuming if the database already exists.
// Paths use /DOMAIN/DATABASE and one ASCII schema identifier; names fold uppercase.
func CreateTenant(ctx context.Context, db *sql.DB, databasePath, schema string) error {
	databasePath, schemaPath, err := tenantPaths(databasePath, schema)
	if err != nil {
		return err
	}
	if _, err := db.ExecContext(ctx, `CREATE DATABASE "`+databasePath+`"`); err != nil {
		var apiErr *api.Error
		if !errors.As(err, &apiErr) || apiErr.Code != api.ErrCodeDatabaseAlreadyExists {
			return fmt.Errorf("otelsql: create database %s: %w", databasePath, err)
		}
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
