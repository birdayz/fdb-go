package sqltest

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"testing"

	"fdb.dev/layers/otel/otelsql"
	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/sqltest/testkit"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/proto"
)

// One tenant record store joins all OTel tables; the rfc204 otel_v1 golden
// independently pins Java's bytes for the same template.
func TestFDB_OtelTemplateV1(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	const path = "/FRL/otel_tenant_v1"
	setup := testkit.OpenDB(t, path)
	var apiErr *api.Error
	if err := otelsql.CreateTenant(ctx, setup, path, "S"); !errors.As(err, &apiErr) || apiErr.Code != api.ErrCodeUnknownSchemaTemplate {
		t.Fatalf("tenant before template = %v, want unknown schema template", err)
	}
	if err := otelsql.CreateTemplate(ctx, setup); err != nil {
		t.Fatal(err)
	}
	if err := otelsql.CreateTenant(ctx, setup, path, "S"); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("fdbsql", fmt.Sprintf("fdbsql://%s?cluster_file=%s&schema=S", strings.ToUpper(path), testkit.ClusterFile()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	explain := testkit.Explainer(t, db, ctx)

	testkit.MustExecCtx(t, db, ctx, "INSERT INTO spans (trace_id, span_id, name, start_ns, service_name, str_attrs, int_attrs, message_refs) VALUES "+
		"(X'01', X'0a', 'chat', 100, 'agent', [('span', 'gen_ai.request.model', 'gpt-4o'), ('resource', 'service.name', 'agent')], [('span', 'gen_ai.usage.input_tokens', 50)], [(1, 7, 1, true, [0])]), "+
		"(X'01', X'0b', 'tool', 200, 'agent', [('span', 'gen_ai.tool.name', 'search')], [('span', 'gen_ai.usage.input_tokens', 500)], []), "+
		"(X'02', X'0c', 'chat', 300, 'agent', [('span', 'gen_ai.request.model', 'gpt-4o')], [], [(1, 7, 2, true, [0, 1])]), "+
		"(X'03', X'0d', 'chat', 400, 'other', [('span', 'gen_ai.request.model', 'claude')], [], [])")
	testkit.MustExecCtx(t, db, ctx, "INSERT INTO logs (event_id, trace_id, span_id, effective_ns, severity_text, str_attrs) VALUES "+
		"('e1', X'01', X'0a', 110, 'ERROR', [('log', 'incident.bucket', 'b0')]), "+
		"('e2', X'02', X'0c', 310, 'INFO', [('log', 'incident.bucket', 'b1')]), "+
		"('e3', NULL, NULL, 320, 'WARN', [('log', 'incident.bucket', 'b0')])")
	testkit.MustExecCtx(t, db, ctx, "INSERT INTO messages (writer, seq, part, content_hash, role, content) VALUES "+
		"(7, 1, 0, X'f1', 'user', 'hello'), (7, 2, 0, X'f2', 'assistant', 'world')")
	testkit.MustExecCtx(t, db, ctx, "INSERT INTO message_traces VALUES (7, 1, X'01'), (7, 1, X'02'), (7, 2, X'02')")

	for _, c := range []struct {
		name, sql, rows    string
		planHas, planLacks []string
	}{
		{
			name: "string attribute, newest first",
			sql: "SELECT s.name, s.start_ns FROM spans AS s WHERE EXISTS (SELECT 1 FROM s.str_attrs AS a " +
				"WHERE a.scope = 'span' AND a.attr_key = 'gen_ai.request.model' AND a.str_value = 'gpt-4o') ORDER BY s.start_ns DESC",
			rows:      "[chat 300] [chat 100]",
			planHas:   []string{"IndexScan(SPANS_STR_ATTR, [=, =, =,", "REVERSE"},
			planLacks: []string{"InMemorySort"},
		},
		{
			name: "integer attribute range",
			sql: "SELECT s.name, s.start_ns FROM spans AS s WHERE EXISTS (SELECT 1 FROM s.int_attrs AS a " +
				"WHERE a.scope = 'span' AND a.attr_key = 'gen_ai.usage.input_tokens' AND a.int_value > 100)",
			rows:    "[tool 200]",
			planHas: []string{"IndexScan(SPANS_INT_ATTR, [=, =, <>"},
		},
		{
			name:    "span by span id",
			sql:     "SELECT name, start_ns FROM spans WHERE span_id = X'0b'",
			rows:    "[tool 200]",
			planHas: []string{"IndexScan(SPANS_SPAN_ID, [="},
		},
		{
			name:      "logs by time",
			sql:       "SELECT event_id FROM logs ORDER BY effective_ns DESC",
			rows:      "[e3] [e2] [e1]",
			planHas:   []string{"IndexScan(LOGS_TIME", "REVERSE"},
			planLacks: []string{"InMemorySort"},
		},
		{
			name: "spans of a trace with their logs",
			sql: "SELECT s.name, l.event_id FROM spans AS s, logs AS l WHERE s.trace_id = X'01' " +
				"AND l.trace_id = s.trace_id AND l.span_id = s.span_id",
			rows:    "[chat e1]",
			planHas: []string{"Scan(SPANS, [=])", "IndexScan(LOGS_TRACE, [=, =,"},
		},
		{
			name: "logs by attribute, newest first",
			sql: "SELECT l.event_id FROM logs AS l WHERE EXISTS (SELECT 1 FROM l.str_attrs AS a " +
				"WHERE a.scope = 'log' AND a.attr_key = 'incident.bucket' AND a.str_value = 'b0') ORDER BY l.effective_ns DESC",
			rows:      "[e3] [e1]",
			planHas:   []string{"IndexScan(LOGS_STR_ATTR, [=, =, =,", "REVERSE"},
			planLacks: []string{"InMemorySort"},
		},
		{
			name: "messages of a trace",
			sql: "SELECT m.role, m.content FROM message_traces AS mt, messages AS m WHERE mt.trace_id = X'02' " +
				"AND m.writer = mt.writer AND m.seq = mt.seq AND m.part = 0",
			rows:    "[user hello] [assistant world]",
			planHas: []string{"IndexScan(MESSAGE_TRACES_TRACE, [=", "Scan(MESSAGES, [=, =, =])"},
		},
		{
			name:    "message by content digest",
			sql:     "SELECT writer, seq FROM messages WHERE content_hash = X'f2'",
			rows:    "[7 2]",
			planHas: []string{"IndexScan(MESSAGES_HASH, [=]"},
		},
		{
			name: "span message references",
			sql:  "SELECT r.writer, r.seq FROM spans AS s, s.message_refs AS r WHERE s.trace_id = X'02' AND s.span_id = X'0c'",
			rows: "[7 2]",
		},
	} {
		plan := explain(c.sql)
		for _, want := range c.planHas {
			if !strings.Contains(plan, want) {
				t.Errorf("%s: plan lacks %q: %s", c.name, want, plan)
			}
		}
		for _, unwanted := range c.planLacks {
			if strings.Contains(plan, unwanted) {
				t.Errorf("%s: plan has %q: %s", c.name, unwanted, plan)
			}
		}
		if got := otelRows(t, db, ctx, c.sql); got != c.rows {
			t.Errorf("%s: rows %s, want %s", c.name, got, c.rows)
		}
	}
	t.Run("remaining typed indexes", func(t *testing.T) {
		testkit.MustExecCtx(t, db, ctx, "INSERT INTO spans (trace_id, span_id, start_ns, long_str_attrs) VALUES "+
			"(X'05', X'01', 600, [('span','long',X'0102','long value')]), (X'05', X'02', 500, [('span','long',X'0304','other value')])")
		testkit.MustExecCtx(t, db, ctx, "INSERT INTO logs (event_id, effective_ns, int_attrs) VALUES "+
			"('i1', 600, [('log','count',7)]), ('i2', 500, [('log','count',8)])")
		for _, tc := range []struct{ query, index, rows string }{
			{"SELECT s.start_ns FROM spans AS s WHERE EXISTS (SELECT 1 FROM s.long_str_attrs AS a WHERE a.scope = 'span' AND a.attr_key = 'long' AND a.digest = X'0102')", "SPANS_LONG_STR_ATTR", "[600]"},
			{"SELECT l.event_id FROM logs AS l WHERE EXISTS (SELECT 1 FROM l.int_attrs AS a WHERE a.scope = 'log' AND a.attr_key = 'count' AND a.int_value = 7)", "LOGS_INT_ATTR", "[i1]"},
		} {
			if plan := explain(tc.query); !strings.Contains(plan, "IndexScan("+tc.index+", [=, =, =,") {
				t.Errorf("%s equality lookup: %s", tc.index, plan)
			}
			if got := otelRows(t, db, ctx, tc.query); got != tc.rows {
				t.Errorf("%s rows = %s, want %s", tc.index, got, tc.rows)
			}
		}
	})
	t.Run("multipart message uniqueness", func(t *testing.T) {
		// Only part zero carries the whole-message digest. Chunks have NULL
		// digests, and UNIQUE deliberately permits multiple NULLs.
		testkit.MustExecCtx(t, db, ctx, "INSERT INTO messages (writer, seq, part, content_hash, content) VALUES "+
			"(7, 1, 1, NULL, 'chunk one'), (7, 1, 2, NULL, 'chunk two'), (7, 3, 0, NULL, 'no digest')")
		if got := otelRows(t, db, ctx, "SELECT part FROM messages WHERE writer = 7 AND seq = 1 ORDER BY part"); got != "[0] [1] [2]" {
			t.Fatalf("message parts = %s", got)
		}
		_, err := db.ExecContext(ctx, "INSERT INTO messages (writer, seq, part, content_hash) VALUES (8, 1, 0, X'f1')")
		var apiErr *api.Error
		if !errors.As(err, &apiErr) || apiErr.Code != api.ErrCodeUniqueConstraintViolation {
			t.Fatalf("duplicate whole-message digest = %v, want UNIQUE violation", err)
		}
		if got := otelRows(t, db, ctx, "SELECT COUNT(*) FROM messages WHERE writer = 8"); got != "[0]" {
			t.Fatalf("rejected message persisted: %s", got)
		}
	})
	t.Run("typed arrays and original OTLP", func(t *testing.T) {
		testOtelTemplateValues(t, db, ctx)
	})
}

func testOtelTemplateValues(t *testing.T, db *sql.DB, ctx context.Context) {
	t.Helper()
	arrays := []string{"str_attrs", "long_str_attrs", "int_attrs", "double_attrs", "bool_attrs", "bytes_attrs", "any_attrs"}
	testkit.MustExecCtx(t, db, ctx, "INSERT INTO spans (trace_id, span_id, start_ns) VALUES (X'04', X'01', 1)")
	empty := make([]string, len(arrays))
	null := make([]string, len(arrays))
	for i := range arrays {
		empty[i], null[i] = "[]", "NULL"
	}
	for _, row := range []struct{ span, values string }{{"02", strings.Join(empty, ",")}, {"03", strings.Join(null, ",")}} {
		testkit.MustExecCtx(t, db, ctx, "INSERT INTO spans (trace_id, span_id, "+strings.Join(arrays, ",")+") VALUES (X'04', X'"+row.span+"', "+row.values+")")
	}
	for _, column := range arrays {
		got := otelRows(t, db, ctx, "SELECT "+column+" IS NULL, CARDINALITY("+column+") FROM spans WHERE trace_id = X'04' ORDER BY span_id")
		if got != "[true <nil>] [false 0] [true <nil>]" {
			t.Errorf("%s missing/empty/NULL = %s", column, got)
		}
	}

	// Hand-inserted rows pin SQL storage types and opaque envelope preservation,
	// not exporter extraction or decomposed-message reconstruction fidelity.
	attrs := []*commonpb.KeyValue{
		{Key: "v", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "3"}}},
		{Key: "v", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_IntValue{IntValue: 3}}},
		{Key: "double", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_DoubleValue{DoubleValue: 3.5}}},
		{Key: "bool", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_BoolValue{BoolValue: false}}},
		{Key: "bytes", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_BytesValue{BytesValue: []byte{0, 255}}}},
		{Key: "array", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_ArrayValue{ArrayValue: &commonpb.ArrayValue{Values: []*commonpb.AnyValue{{Value: &commonpb.AnyValue_IntValue{IntValue: 1}}}}}}},
		{Key: "kvlist", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_KvlistValue{KvlistValue: &commonpb.KeyValueList{Values: []*commonpb.KeyValue{{Key: "empty", Value: &commonpb.AnyValue{}}}}}}},
		{Key: "unset", Value: &commonpb.AnyValue{}},
	}
	original := &tracepb.ResourceSpans{ScopeSpans: []*tracepb.ScopeSpans{{Spans: []*tracepb.Span{{Name: "fidelity", Attributes: attrs}}}}}
	original.ProtoReflect().SetUnknown([]byte{0xf8, 0x07, 0x01}) // unknown field 127
	raw, err := proto.Marshal(original)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.ExecContext(ctx, "INSERT INTO spans (trace_id, span_id, start_ns, str_attrs, int_attrs, double_attrs, bool_attrs, bytes_attrs, any_attrs, otlp) VALUES "+
		"(X'04', X'04', 4, [('span','v','3')], [('span','v',3)], [('span','v',3.5)], [('span','v',false)], [('span','v',X'00ff')], [('span','array','[1]'),('span','kvlist','{\"empty\":null}'),('span','unset','null')], ?)", raw)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ array, predicate, index string }{
		{"str_attrs", "a.str_value = '3'", "SPANS_STR_ATTR"},
		{"int_attrs", "a.int_value = 3", "SPANS_INT_ATTR"},
		{"double_attrs", "a.double_value = 3.5", "SPANS_DOUBLE_ATTR"},
		{"bool_attrs", "a.bool_value = false", "SPANS_BOOL_ATTR"},
		{"bytes_attrs", "a.bytes_value = X'00ff'", ""},
		{"any_attrs", "a.json_value = '[1]'", ""},
	} {
		query := "SELECT s.start_ns FROM spans AS s WHERE EXISTS (SELECT 1 FROM s." + tc.array + " AS a WHERE a.scope = 'span' AND a.attr_key = 'v' AND " + tc.predicate + ")"
		if tc.array == "any_attrs" {
			query = strings.Replace(query, "a.attr_key = 'v'", "a.attr_key = 'array'", 1)
		}
		plan := testkit.Explainer(t, db, ctx)(query)
		if tc.index != "" && !strings.Contains(plan, "IndexScan("+tc.index) {
			t.Errorf("%s: expected typed index, got %s", tc.array, plan)
		}
		if got := otelRows(t, db, ctx, query); got != "[4]" {
			t.Errorf("%s predicate returned %s", tc.array, got)
		}
	}
	// Logical equality spans both physical signed-zero groups, so it cannot
	// provide the time suffix's order even though the element is equality-bound.
	testkit.MustExecCtx(t, db, ctx, "INSERT INTO spans (trace_id, span_id, start_ns, double_attrs) VALUES "+
		"(X'04', X'05', 20, [('span','zero',-0.0)]), (X'04', X'06', 10, [('span','zero',0.0)])")
	zeroQuery := "SELECT s.start_ns FROM spans AS s WHERE EXISTS (SELECT 1 FROM s.double_attrs AS a WHERE a.scope = 'span' AND a.attr_key = 'zero' AND a.double_value = 0.0) ORDER BY s.start_ns"
	if plan := testkit.Explainer(t, db, ctx)(zeroQuery); !strings.Contains(plan, "InMemorySort") && !strings.Contains(plan, "IndexScan(SPANS_TIME") {
		t.Errorf("signed-zero element cannot order the time suffix: %s", plan)
	}
	if got := otelRows(t, db, ctx, zeroQuery); got != "[10] [20]" {
		t.Errorf("signed-zero time order = %s", got)
	}
	var stored []byte
	if err := db.QueryRowContext(ctx, "SELECT otlp FROM spans WHERE trace_id = X'04' AND span_id = X'04'").Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(raw, stored) {
		t.Fatal("SQL changed original OTLP bytes")
	}
	restored := &tracepb.ResourceSpans{}
	if err := proto.Unmarshal(stored, restored); err != nil || !proto.Equal(original, restored) {
		t.Fatalf("OTLP AnyValue fidelity: %v", err)
	}
}
