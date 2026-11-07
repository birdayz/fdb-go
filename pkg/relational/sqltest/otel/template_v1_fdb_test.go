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
	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer"
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

	refs := [][]otelsql.MessageRef{
		{{Writer: 7, Seq: 1, Attribute: 1, Newest: true, Ordinals: []int{0}}},
		{{Writer: 7, Seq: 1, Attribute: 1, Ordinals: []int{0}}, {Writer: 7, Seq: 2, Attribute: 2, Newest: true, Ordinals: []int{1}}},
	}
	chunks := make([][]byte, len(refs))
	for i, r := range refs {
		c, err := otelsql.PackRefChunks(r)
		if err != nil || len(c) != 1 {
			t.Fatalf("refs %d: %d chunks, %v", i, len(c), err)
		}
		chunks[i] = c[0]
	}
	testkit.MustExecCtx(t, db, ctx, fmt.Sprintf("INSERT INTO spans (trace_id, span_id, name, start_ns, service_name, str_attrs, int_attrs, ref_chunks) VALUES "+
		"(X'01', X'0a', 'chat', 100, 'agent', [('span', 'gen_ai.request.model', 'gpt-4o'), ('resource', 'service.name', 'agent')], [('span', 'gen_ai.usage.input_tokens', 50)], [(0, X'%x')]), "+
		"(X'01', X'0b', 'tool', 200, 'agent', [('span', 'gen_ai.tool.name', 'search')], [('span', 'gen_ai.usage.input_tokens', 500)], []), "+
		"(X'02', X'0c', 'chat', 300, 'agent', [('span', 'gen_ai.request.model', 'gpt-4o')], [], [(0, X'%x')]), "+
		"(X'03', X'0d', 'chat', 400, 'other', [('span', 'gen_ai.request.model', 'claude')], [], [])", chunks[0], chunks[1]))
	testkit.MustExecCtx(t, db, ctx, "INSERT INTO logs (event_id, trace_id, span_id, effective_ns, severity_text, event_name, str_attrs) VALUES "+
		"('e1', X'01', X'0a', 110, 'ERROR', 'gen_ai.choice', [('log', 'incident.bucket', 'b0')]), "+
		"('e2', X'02', X'0c', 310, 'INFO', NULL, [('log', 'incident.bucket', 'b1')]), "+
		"('e3', NULL, NULL, 320, 'WARN', 'gen_ai.choice', [('log', 'incident.bucket', 'b0')])")
	testkit.MustExecCtx(t, db, ctx, "INSERT INTO messages (writer, seq, part, content_hash, role, content, shard) VALUES "+
		"(7, 1, 0, X'f1', 'user', 'hello', 7), (7, 2, 0, X'f2', 'assistant', 'world', 7)")
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
			// The attribute index covers the witness's trace and span.
			name: "log witnesses from the attribute index",
			sql: "SELECT l.event_id, l.trace_id, l.span_id FROM logs AS l WHERE EXISTS (SELECT 1 FROM l.str_attrs AS a " +
				"WHERE a.scope = 'log' AND a.attr_key = 'incident.bucket' AND a.str_value = 'b0') ORDER BY l.effective_ns DESC",
			rows:      "[e3 <nil> <nil>] [e1 [1] [10]]",
			planHas:   []string{"COVERING", "IndexScan(LOGS_STR_ATTR, [=, =, =,", "REVERSE"},
			planLacks: []string{"InMemorySort"},
		},
		{
			name:      "logs by event name, newest first",
			sql:       "SELECT event_id, trace_id FROM logs WHERE event_name = 'gen_ai.choice' ORDER BY effective_ns DESC",
			rows:      "[e3 <nil>] [e1 [1]]",
			planHas:   []string{"COVERING", "IndexScan(LOGS_EVENT, [=,", "REVERSE"},
			planLacks: []string{"InMemorySort"},
		},
		{
			name:    "store meta",
			sql:     "SELECT name FROM store_meta",
			rows:    "",
			planHas: []string{"Scan(STORE_META"},
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
		testkit.MustExecCtx(t, db, ctx, "INSERT INTO logs (event_id, effective_ns, int_attrs, long_str_attrs, double_attrs, bool_attrs) VALUES "+
			"('i1', 600, [('log','count',7)], [('log','long',X'0102','long value')], [('log','ratio',0.5)], [('log','ok',true)]), "+
			"('i2', 500, [('log','count',8)], [('log','long',X'0304','other value')], [('log','ratio',1.5)], [('log','ok',false)])")
		for _, tc := range []struct{ query, index, rows string }{
			{"SELECT s.start_ns FROM spans AS s WHERE EXISTS (SELECT 1 FROM s.long_str_attrs AS a WHERE a.scope = 'span' AND a.attr_key = 'long' AND a.digest = X'0102')", "SPANS_LONG_STR_ATTR", "[600]"},
			{"SELECT l.event_id FROM logs AS l WHERE EXISTS (SELECT 1 FROM l.int_attrs AS a WHERE a.scope = 'log' AND a.attr_key = 'count' AND a.int_value = 7)", "LOGS_INT_ATTR", "[i1]"},
			{"SELECT l.event_id FROM logs AS l WHERE EXISTS (SELECT 1 FROM l.long_str_attrs AS a WHERE a.scope = 'log' AND a.attr_key = 'long' AND a.digest = X'0304')", "LOGS_LONG_STR_ATTR", "[i2]"},
			{"SELECT l.event_id FROM logs AS l WHERE EXISTS (SELECT 1 FROM l.double_attrs AS a WHERE a.scope = 'log' AND a.attr_key = 'ratio' AND a.double_value = 0.5)", "LOGS_DOUBLE_ATTR", "[i1]"},
			{"SELECT l.event_id FROM logs AS l WHERE EXISTS (SELECT 1 FROM l.bool_attrs AS a WHERE a.scope = 'log' AND a.attr_key = 'ok' AND a.bool_value = false)", "LOGS_BOOL_ATTR", "[i2]"},
		} {
			if plan := explain(tc.query); !strings.Contains(plan, "IndexScan("+tc.index+", [=, =, =,") {
				t.Errorf("%s equality lookup: %s", tc.index, plan)
			}
			if got := otelRows(t, db, ctx, tc.query); got != tc.rows {
				t.Errorf("%s rows = %s, want %s", tc.index, got, tc.rows)
			}
		}
	})
	t.Run("binary keyset pages", func(t *testing.T) {
		testOtelTemplateBinaryKeysetPages(t, db, ctx)
	})
	t.Run("message full text", func(t *testing.T) {
		testOtelTemplateFullText(t, db, ctx)
	})
	t.Run("direct readers and EnsureTenant", func(t *testing.T) {
		testOtelTemplateDirect(t, db, ctx, path, chunks, refs)
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

// SQL plans MESSAGES_TEXT for TEXT_CONTAINS_*(content, ?, 'otel_message_v1')
// with the role bound and every shard listed; an unbound shard or another
// tokenizer keeps record filtering, with the same rows.
func testOtelTemplateFullText(t *testing.T, db *sql.DB, ctx context.Context) {
	t.Helper()
	testkit.MustExecCtx(t, db, ctx, "INSERT INTO messages (writer, seq, part, role, content, shard) VALUES "+
		"(20, 1, 0, 'user', 'the quick brown fox', 4), (21, 1, 0, 'user', 'brown dogs and a quick fox', 5), "+
		"(22, 1, 0, 'assistant', 'quick brown fox', 6), (23, 1, 0, 'user', 'Crème BRÛLÉE, quick', 7)")
	shards := make([]string, otelsql.MessageTextShards)
	for i := range shards {
		shards[i] = fmt.Sprint(i)
	}
	all := " AND role = ? AND shard IN (" + strings.Join(shards, ", ") + ")"
	for _, tc := range []struct {
		fn, terms, rows string
	}{
		{"TEXT_CONTAINS_ALL", "quick fox", "[20] [21]"},
		{"TEXT_CONTAINS_PHRASE", "quick brown fox", "[20]"},
		{"TEXT_CONTAINS_ANY", "dogs creme", "[21] [23]"},
	} {
		for _, form := range []struct {
			tokenizer, filter string
			indexed           bool
		}{
			{otelsql.MessageTokenizerName, all, true},
			{otelsql.MessageTokenizerName, " AND role = ?", false},
			{"default", all, false},
		} {
			query := "SELECT writer FROM messages WHERE " + tc.fn + "(content, ?, '" + form.tokenizer + "')" + form.filter + " ORDER BY writer"
			var plan string
			if err := db.QueryRowContext(ctx, "EXPLAIN "+query, tc.terms, "user").Scan(&plan); err != nil {
				t.Fatal(err)
			}
			if strings.Contains(plan, "TextIndexScan(MESSAGES_TEXT") != form.indexed {
				t.Errorf("%s: indexed=%v plan %s", query, form.indexed, plan)
			}
			rows, err := db.QueryContext(ctx, query, tc.terms, "user")
			if err != nil {
				t.Fatal(err)
			}
			var got []string
			for rows.Next() {
				var w int64
				if err := rows.Scan(&w); err != nil {
					t.Fatal(err)
				}
				got = append(got, fmt.Sprintf("[%d]", w))
			}
			if err := rows.Err(); err != nil {
				t.Fatal(err)
			}
			rows.Close()
			if strings.Join(got, " ") != tc.rows {
				t.Errorf("%s: rows %v, want %s", query, got, tc.rows)
			}
		}
	}
}

// Direct Record Layer readers and writers of SQL's stores: SPANS_REF_CHUNKS
// holds one entry per chunk and MESSAGES_LARGE the large messages; EnsureTenant
// accepts the template SQL created, and a compiled STORE_META row in a tenant
// it creates reads back through SQL.
func testOtelTemplateDirect(t *testing.T, db *sql.DB, ctx context.Context, path string, chunks [][]byte, refs [][]otelsql.MessageRef) {
	t.Helper()
	raw, err := fdb.OpenDatabase(testkit.ClusterFile())
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	rl := recordlayer.NewFDBDatabase(raw)
	ts, err := otelsql.EnsureTenant(ctx, rl, path, "S", nil)
	if err != nil {
		t.Fatalf("EnsureTenant over the SQL-created template: %v", err)
	}
	testkit.MustExecCtx(t, db, ctx, "INSERT INTO messages (writer, seq, part, parts, large, shard) VALUES (30, 1, 0, 2, [1], 14), (30, 1, 1, NULL, NULL, 14), (31, 1, 0, 1, NULL, 15)")
	scan := func(index string, prefix tuple.Tuple) []tuple.Tuple {
		t.Helper()
		var out []tuple.Tuple
		if _, err := rl.Run(ctx, func(rc *recordlayer.FDBRecordContext) (any, error) {
			out = nil
			store, err := ts.Builder(rc).Open()
			if err != nil {
				return nil, err
			}
			cur := store.ScanIndex(ts.MetaData.GetIndex(index), recordlayer.TupleRangeAllOf(prefix), nil, recordlayer.ForwardScan())
			defer cur.Close()
			for {
				r, err := cur.OnNext(ctx)
				if err != nil || !r.HasNext() {
					return nil, err
				}
				out = append(out, r.GetValue().Key)
			}
		}); err != nil {
			t.Fatal(err)
		}
		return out
	}
	entries := scan("SPANS_REF_CHUNKS", tuple.Tuple{[]byte{2}})
	if len(entries) != 1 || len(entries[0]) < 5 || !bytes.Equal(entries[0][0].([]byte), []byte{2}) || entries[0][1] != int64(300) ||
		!bytes.Equal(entries[0][2].([]byte), []byte{0x0c}) || entries[0][3] != int64(0) || !bytes.Equal(entries[0][4].([]byte), chunks[1]) {
		t.Fatalf("SPANS_REF_CHUNKS of trace 02: %v", entries)
	}
	got, err := otelsql.UnpackRefChunk(entries[0][4].([]byte))
	if err != nil || fmt.Sprint(got) != fmt.Sprint(refs[1]) {
		t.Fatalf("unpacked refs %v (%v), want %v", got, err, refs[1])
	}
	if n := len(scan("SPANS_REF_CHUNKS", tuple.Tuple{[]byte{1}})); n != 1 {
		t.Fatalf("trace 01 has %d ref chunk entries, want 1 (span 0b has none)", n)
	}
	if large := scan("MESSAGES_LARGE", tuple.Tuple{int64(1)}); len(large) != 1 || large[0][1] != int64(30) || large[0][2] != int64(1) {
		t.Fatalf("MESSAGES_LARGE %v", large)
	}

	ts2, err := otelsql.EnsureTenant(ctx, rl, path, "S2", nil)
	if err != nil {
		t.Fatal(err)
	}
	n := int64(3)
	meta, err := otelsql.BuildStoreMetaCompiled(otelsql.StoreMeta{Name: otelsql.StoreMetaMessageWriter, Int: &n})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rl.Run(ctx, func(rc *recordlayer.FDBRecordContext) (any, error) {
		store, err := ts2.Builder(rc).Open()
		if err != nil {
			return nil, err
		}
		return store.SaveRecord(meta)
	}); err != nil {
		t.Fatal(err)
	}
	s2, err := sql.Open("fdbsql", fmt.Sprintf("fdbsql://%s?cluster_file=%s&schema=S2", strings.ToUpper(path), testkit.ClusterFile()))
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	if got := otelRows(t, s2, ctx, "SELECT name, int_value FROM store_meta"); got != "[message_writer 3]" {
		t.Fatalf("store_meta via SQL: %s", got)
	}
	if got := otelRows(t, s2, ctx, "SELECT COUNT(*) FROM spans"); got != "[0]" {
		t.Fatalf("new tenant spans: %s", got)
	}
}

func testOtelTemplateBinaryKeysetPages(t *testing.T, db *sql.DB, ctx context.Context) {
	t.Helper()
	binaryID := func(width int, first, last byte) []byte {
		id := make([]byte, width)
		id[0], id[width-1] = first, last
		return id
	}
	// This explicit order crosses time, trace and span boundaries, including NUL/high bytes.
	want := []struct {
		trace, span []byte
		start       int64
	}{
		{binaryID(16, 0x80, 0), binaryID(8, 0, 1), 300},
		{binaryID(16, 1, 0), binaryID(8, 0, 0xff), 200},
		{binaryID(16, 1, 0), binaryID(8, 0, 0), 200},
		{binaryID(16, 0, 0xff), binaryID(8, 0xff, 0), 200},
		{binaryID(16, 0, 0), binaryID(8, 0, 1), 200},
		{binaryID(16, 0xff, 0xff), binaryID(8, 0xff, 0xff), 100},
	}
	const attr = "('span','binary-page','yes')"
	for _, i := range []int{5, 2, 0, 4, 1, 3} {
		row := want[i]
		testkit.MustExecCtx(t, db, ctx, fmt.Sprintf("INSERT INTO spans (trace_id, span_id, start_ns, str_attrs) VALUES (X'%x',X'%x',%d,[%s,%s])",
			row.trace, row.span, row.start, attr, attr))
	}
	testkit.MustExecCtx(t, db, ctx, fmt.Sprintf("INSERT INTO spans (trace_id, span_id, start_ns, str_attrs) VALUES (X'%x',X'%x',400,[('span','binary-page','no')])",
		binaryID(16, 0xff, 0), binaryID(8, 0xff, 0)))
	const query = "SELECT s.trace_id, s.span_id, s.start_ns FROM spans AS s WHERE EXISTS (SELECT 1 FROM s.str_attrs AS a WHERE a.scope = 'span' AND a.attr_key = 'binary-page' AND a.str_value = 'yes')"
	const order = " ORDER BY s.start_ns DESC, s.trace_id DESC, s.span_id DESC"
	explain := testkit.Explainer(t, db, ctx)
	for _, form := range []string{"AND", "OR"} {
		for _, pageSize := range []int{1, 2, 3} {
			t.Run(fmt.Sprintf("%s/%d", form, pageSize), func(t *testing.T) {
				cursor := ""
				next := 0
				terminated := false
				seen := make(map[string]bool)
				for range len(want) + 2 {
					q := query + cursor + order + fmt.Sprintf(" LIMIT %d", pageSize)
					plan := explain(q)
					if !strings.Contains(plan, "IndexScan(SPANS_STR_ATTR, [=, =, =,") || !strings.Contains(plan, "REVERSE") || strings.Contains(plan, "InMemorySort") {
						t.Fatalf("binary page must use the ordered fan-out index: %s", plan)
					}
					if form == "OR" && cursor != "" {
						if !strings.Contains(plan, "MergeSortUnion(") || !strings.Contains(plan, "IndexScan(SPANS_STR_ATTR, [=, =, =, =, =, <>]") {
							t.Fatalf("expanded binary page must merge all bound key arms: %s", plan)
						}
					} else if strings.Contains(plan, "Union") {
						t.Fatalf("conjunctive/first binary page should use one range scan: %s", plan)
					}
					rows, err := db.QueryContext(ctx, q)
					if err != nil {
						t.Fatal(err)
					}
					n := 0
					var trace, span []byte
					var start int64
					for rows.Next() {
						if err := rows.Scan(&trace, &span, &start); err != nil {
							rows.Close()
							t.Fatal(err)
						}
						if next >= len(want) || !bytes.Equal(trace, want[next].trace) || !bytes.Equal(span, want[next].span) || start != want[next].start {
							rows.Close()
							t.Fatalf("page row %d = (%x,%x,%d), want explicit binary sequence", next, trace, span, start)
						}
						key := fmt.Sprintf("%x/%x", trace, span)
						if seen[key] {
							rows.Close()
							t.Fatalf("duplicate identity across pages: %s", key)
						}
						seen[key] = true
						trace, span = bytes.Clone(trace), bytes.Clone(span)
						next++
						n++
					}
					if err := rows.Err(); err != nil {
						rows.Close()
						t.Fatal(err)
					}
					rows.Close()
					if n > pageSize {
						t.Fatalf("page returned %d rows, limit %d", n, pageSize)
					}
					if n == 0 {
						terminated = true
						break
					}
					if form == "AND" {
						cursor = fmt.Sprintf(" AND s.start_ns <= %d AND (s.start_ns < %d OR s.trace_id < X'%x' OR (s.trace_id = X'%x' AND s.span_id < X'%x'))", start, start, trace, trace, span)
					} else {
						cursor = fmt.Sprintf(" AND (s.start_ns < %d OR (s.start_ns = %d AND (s.trace_id < X'%x' OR (s.trace_id = X'%x' AND s.span_id < X'%x'))))", start, start, trace, trace, span)
					}
				}
				if !terminated || next != len(want) {
					t.Fatalf("binary page walk terminated=%t rows=%d, want %d", terminated, next, len(want))
				}
			})
		}
	}
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
