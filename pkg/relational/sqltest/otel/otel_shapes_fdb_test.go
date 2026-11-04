package sqltest

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"testing"

	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/sqltest/testkit"
)

// The OTel span/log SQL shapes, end to end on real FDB. Java 4.14.2's plans and
// rows for the same schema and queries are pinned by
// conformance/otel_sql_shapes_java_probe_test.go; each case cites the Java plan.
const otelShapesTemplate = "CREATE SCHEMA TEMPLATE otel_shapes_tmpl " +
	"CREATE TYPE AS STRUCT attr (scope STRING, k STRING, sv STRING, iv BIGINT) " +
	"CREATE TABLE spans (trace_id STRING, span_id STRING, start_ns BIGINT, name STRING, attrs attr ARRAY, PRIMARY KEY (trace_id, span_id)) " +
	"CREATE INDEX spans_attr_str AS SELECT a.scope, a.k, a.sv, s.start_ns, s.trace_id, s.span_id FROM spans AS s, (SELECT scope, k, sv FROM s.attrs) AS a ORDER BY a.scope, a.k, a.sv, s.start_ns, s.trace_id, s.span_id " +
	"CREATE INDEX spans_time AS SELECT start_ns, trace_id, span_id FROM spans ORDER BY start_ns, trace_id, span_id " +
	"CREATE TABLE logs (event_id STRING, trace_id STRING, span_id STRING, time_ns BIGINT, body STRING, PRIMARY KEY (event_id)) " +
	"CREATE INDEX logs_trace AS SELECT trace_id, span_id, time_ns FROM logs ORDER BY trace_id, span_id, time_ns"

const gptExists = "EXISTS (SELECT 1 FROM s.attrs AS a WHERE a.scope = 'span' AND a.k = 'model' AND a.sv = 'gpt')"

func otelShapesDB(t *testing.T, name string) (*sql.DB, context.Context, func(string) string) {
	t.Helper()
	ctx := context.Background()
	path := "/FRL/otel_shapes_" + name
	setup := testkit.OpenDB(t, path)
	testkit.MustExecCtx(t, setup, ctx, "CREATE DATABASE "+path)
	testkit.MustExecCtx(t, setup, ctx, strings.Replace(otelShapesTemplate, "otel_shapes_tmpl", "otel_shapes_"+name, 1))
	testkit.MustExecCtx(t, setup, ctx, "CREATE SCHEMA "+path+"/s WITH TEMPLATE otel_shapes_"+name)
	db, err := sql.Open("fdbsql", fmt.Sprintf("fdbsql://%s?cluster_file=%s&schema=S", strings.ToUpper(path), testkit.ClusterFile()))
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	testkit.MustExecCtx(t, db, ctx, "INSERT INTO spans VALUES "+
		"('t1', 's1', 100, 'a', [('span', 'model', 'gpt', NULL), ('span', 'tag', 'x', NULL), ('span', 'tag', 'y', NULL)]), "+
		"('t1', 's2', 200, 'b', [('span', 'model', 'gpt', NULL), ('resource', 'model', 'gpt', NULL)]), "+
		"('t2', 's3', 300, 'c', [('span', 'model', 'claude', NULL), ('span', 'turn', NULL, 3)]), "+
		"('t3', 's4', 400, 'd', [('span', 'model', 'gpt', NULL), ('span', 'tag', 'x', NULL)]), "+
		"('t3', 's5', 500, 'e', [])")
	testkit.MustExecCtx(t, db, ctx, "INSERT INTO logs VALUES ('e1', 't1', 's1', 110, 'one'), ('e2', 't1', 's1', 120, 'two'), "+
		"('e3', 't3', 's4', 410, 'three'), ('e4', 't2', NULL, 310, 'four')")
	return db, ctx, testkit.Explainer(t, db, ctx)
}

func otelRows(t *testing.T, db *sql.DB, ctx context.Context, query string) string {
	t.Helper()
	rows, err := db.QueryContext(ctx, query)
	if err != nil {
		t.Fatalf("query %q: %v", query, err)
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			t.Fatal(err)
		}
		out = append(out, fmt.Sprint(vals))
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("query %q: %v", query, err)
	}
	return strings.Join(out, " ")
}

type otelShape struct {
	name, sql, rows string
	planHas         []string
	planLacks       []string
}

func runOtelShapes(t *testing.T, name string, shapes []otelShape) {
	t.Helper()
	db, ctx, explain := otelShapesDB(t, name)
	for _, s := range shapes {
		plan := explain(s.sql)
		for _, want := range s.planHas {
			if !strings.Contains(plan, want) {
				t.Errorf("%s: plan lacks %q: %s", s.name, want, plan)
			}
		}
		for _, unwanted := range s.planLacks {
			if strings.Contains(plan, unwanted) {
				t.Errorf("%s: plan has %q: %s", s.name, unwanted, plan)
			}
		}
		if got := otelRows(t, db, ctx, s.sql); got != s.rows {
			t.Errorf("%s: rows %s, want %s", s.name, got, s.rows)
		}
	}
}

// Element predicates of one EXISTS bind the fan-out index's element columns,
// and ORDER BY its base-record suffix is a scan direction, not a sort.
func TestFDB_OtelFanOutExists(t *testing.T) {
	t.Parallel()
	runOtelShapes(t, "exists", []otelShape{
		{
			// Java: COVERING(SPANS_ATTR_STR [EQUALS, EQUALS, EQUALS]).
			name:    "several element conditions",
			sql:     "SELECT s.trace_id, s.span_id FROM spans AS s WHERE " + gptExists,
			rows:    "[t1 s1] [t1 s2] [t3 s4]",
			planHas: []string{"IndexScan(SPANS_ATTR_STR, [=, =, =,", "UnorderedPrimaryKeyDistinct"},
		},
		{
			// Java: the same scan, REVERSE, no sort.
			name:      "time desc",
			sql:       "SELECT s.trace_id, s.span_id, s.start_ns FROM spans AS s WHERE " + gptExists + " ORDER BY s.start_ns DESC",
			rows:      "[t3 s4 400] [t1 s2 200] [t1 s1 100]",
			planHas:   []string{"IndexScan(SPANS_ATTR_STR, [=, =, =,", "REVERSE"},
			planLacks: []string{"InMemorySort"},
		},
		{
			// Java: COVERING(SPANS_ATTR_STR [EQ, EQ, EQ, [>= && <]] REVERSE).
			name:      "time range desc",
			sql:       "SELECT s.trace_id, s.span_id, s.start_ns FROM spans AS s WHERE " + gptExists + " AND s.start_ns >= 150 AND s.start_ns < 450 ORDER BY s.start_ns DESC",
			rows:      "[t3 s4 400] [t1 s2 200]",
			planHas:   []string{"IndexScan(SPANS_ATTR_STR, [=, =, =, <>", "REVERSE"},
			planLacks: []string{"InMemorySort"},
		},
		{
			// Java: the same REVERSE scan for the full key order.
			name:      "time and primary key desc",
			sql:       "SELECT s.trace_id, s.span_id, s.start_ns FROM spans AS s WHERE " + gptExists + " ORDER BY s.start_ns DESC, s.trace_id DESC, s.span_id DESC",
			rows:      "[t3 s4 400] [t1 s2 200] [t1 s1 100]",
			planHas:   []string{"IndexScan(SPANS_ATTR_STR, [=, =, =,", "REVERSE"},
			planLacks: []string{"InMemorySort"},
		},
		{
			// Java: the index for one EXISTS, the other EXISTS over its elements.
			name:    "same-span conjunction",
			sql:     "SELECT s.trace_id, s.span_id FROM spans AS s WHERE " + gptExists + " AND EXISTS (SELECT 1 FROM s.attrs AS b WHERE b.scope = 'span' AND b.k = 'tag' AND b.sv = 'x')",
			rows:    "[t1 s1] [t3 s4]",
			planHas: []string{"IndexScan(SPANS_ATTR_STR", "Explode"},
		},
	})
}

// A span with two matching elements is one EXISTS row; Java 4.14.2 returns it
// twice when an element column is left unbound (DIVERGENCES.md).
func TestFDB_OtelFanOutExistsCardinality(t *testing.T) {
	t.Parallel()
	runOtelShapes(t, "cardinality", []otelShape{
		{
			name:    "element value unbound",
			sql:     "SELECT s.trace_id, s.span_id FROM spans AS s WHERE EXISTS (SELECT 1 FROM s.attrs AS a WHERE a.scope = 'span' AND a.k = 'tag')",
			rows:    "[t1 s1] [t3 s4]",
			planHas: []string{"IndexScan(SPANS_ATTR_STR, [=, =,", "UnorderedPrimaryKeyDistinct"},
		},
		{
			// Java returns [t1] [t1]: its plan has no distinct.
			name: "trace existential distinct",
			sql: "SELECT DISTINCT s1.trace_id FROM spans AS s1, spans AS s2 WHERE s1.trace_id = s2.trace_id AND " +
				"EXISTS (SELECT 1 FROM s1.attrs AS a WHERE a.scope = 'span' AND a.k = 'model' AND a.sv = 'gpt') AND " +
				"EXISTS (SELECT 1 FROM s2.attrs AS b WHERE b.scope = 'span' AND b.k = 'tag' AND b.sv = 'y')",
			rows:    "[t1]",
			planHas: []string{"IndexScan(SPANS_ATTR_STR", "Scan(SPANS, [=])"},
		},
	})
}

// Spans and logs of one store join on (trace_id, span_id) through a correlated
// two-column probe in either direction.
func TestFDB_OtelTraceJoins(t *testing.T) {
	t.Parallel()
	runOtelShapes(t, "joins", []otelShape{
		{
			// Java: SCAN(SPANS [EQ]) | FLATMAP { ISCAN(LOGS_TRACE [EQ q0.TRACE_ID, EQ q0.SPAN_ID]) }.
			name:    "spans to logs",
			sql:     "SELECT s.span_id, l.event_id FROM spans AS s, logs AS l WHERE s.trace_id = 't1' AND l.trace_id = s.trace_id AND l.span_id = s.span_id",
			rows:    "[s1 e1] [s1 e2]",
			planHas: []string{"FlatMap(outer=Scan(SPANS, [=])", "IndexScan(LOGS_TRACE, [=, =,"},
		},
		{
			// Java: SCAN(LOGS [EQ]) | FLATMAP { SCAN(SPANS [EQ q0.TRACE_ID, EQ q0.SPAN_ID]) }.
			name:    "logs to spans",
			sql:     "SELECT l.event_id, s.start_ns FROM logs AS l, spans AS s WHERE l.event_id = 'e3' AND s.trace_id = l.trace_id AND s.span_id = l.span_id",
			rows:    "[e3 400]",
			planHas: []string{"FlatMap(outer=Scan(LOGS, [=])", "Scan(SPANS, [=, =])"},
		},
		{
			// Java: ISCAN(SPANS_ATTR_STR [EQ, EQ, EQ]) | FLATMAP { ISCAN(LOGS_TRACE [EQ, EQ]) }.
			name:    "attribute spans to logs",
			sql:     "SELECT s.span_id, l.event_id FROM spans AS s, logs AS l WHERE " + gptExists + " AND l.trace_id = s.trace_id AND l.span_id = s.span_id",
			rows:    "[s1 e1] [s1 e2] [s4 e3]",
			planHas: []string{"IndexScan(SPANS_ATTR_STR, [=, =, =,", "IndexScan(LOGS_TRACE, [=, =,"},
		},
	})
}

// Keyset pages over (start_ns, trace_id, span_id) DESC. Java 4.14.2 rejects
// LIMIT (it pages with maxRows and continuations), so LIMIT is Go-only here;
// the predicate shapes and plans are Java's.
func TestFDB_OtelKeysetPages(t *testing.T) {
	t.Parallel()
	const order = " ORDER BY s.start_ns DESC, s.trace_id DESC, s.span_id DESC"
	andForm := func(start int64, trace, span string) string {
		return fmt.Sprintf(" AND s.start_ns <= %d AND (s.start_ns < %d OR s.trace_id < '%s' OR (s.trace_id = '%s' AND s.span_id < '%s'))",
			start, start, trace, trace, span)
	}
	runOtelShapes(t, "keyset", []otelShape{
		{
			// Java: COVERING(SPANS_ATTR_STR [EQ, EQ, EQ, [<=]] REVERSE) | FILTER.
			name:      "and form",
			sql:       "SELECT s.trace_id, s.span_id, s.start_ns FROM spans AS s WHERE " + gptExists + andForm(200, "t1", "s2") + order,
			rows:      "[t1 s1 100]",
			planHas:   []string{"IndexScan(SPANS_ATTR_STR, [=, =, =, <>", "REVERSE"},
			planLacks: []string{"InMemorySort", "Union"},
		},
		{
			// Java: an ordered union of three REVERSE scans COMPARE BY the key.
			name: "or form",
			sql: "SELECT s.trace_id, s.span_id, s.start_ns FROM spans AS s WHERE " + gptExists +
				" AND (s.start_ns < 200 OR (s.start_ns = 200 AND (s.trace_id < 't1' OR (s.trace_id = 't1' AND s.span_id < 's2'))))" + order,
			rows:      "[t1 s1 100]",
			planHas:   []string{"MergeSortUnion(", "IndexScan(SPANS_ATTR_STR, [=, =, =, =, =, <>]"},
			planLacks: []string{"InMemorySort"},
		},
		{
			// Java: COVERING(SPANS_TIME [[<=]] REVERSE) | FILTER.
			name:      "and form without exists",
			sql:       "SELECT s.trace_id, s.span_id, s.start_ns FROM spans AS s WHERE s.start_ns <= 400 AND (s.start_ns < 400 OR s.trace_id < 't3' OR (s.trace_id = 't3' AND s.span_id < 's4'))" + order,
			rows:      "[t2 s3 300] [t1 s2 200] [t1 s1 100]",
			planHas:   []string{"IndexScan(SPANS_TIME, [<>", "REVERSE"},
			planLacks: []string{"InMemorySort"},
		},
	})

	// Walking every page reproduces the unpaged order.
	db, ctx, _ := otelShapesDB(t, "keyset_walk")
	// Tied times and duplicate matching elements exercise the union's record
	// identity proof and page boundaries, rather than only its scan direction.
	testkit.MustExecCtx(t, db, ctx, "INSERT INTO spans VALUES ('t1', 's6', 200, 'f', [('span', 'model', 'gpt', NULL), ('span', 'model', 'gpt', NULL)])")
	full := otelRows(t, db, ctx, "SELECT s.trace_id, s.span_id, s.start_ns FROM spans AS s WHERE "+gptExists+order)
	orPage := "SELECT s.trace_id, s.span_id, s.start_ns FROM spans AS s WHERE " + gptExists +
		" AND (s.start_ns < 200 OR (s.start_ns = 200 AND (s.trace_id < 't1' OR (s.trace_id = 't1' AND s.span_id < 's6'))))" + order
	if got := otelRows(t, db, ctx, orPage); got != "[t1 s2 200] [t1 s1 100]" {
		t.Fatalf("tied-time OR page = %s", got)
	}
	var pages []string
	where := ""
	for range 10 {
		page, err := db.QueryContext(ctx, "SELECT s.trace_id, s.span_id, s.start_ns FROM spans AS s WHERE "+gptExists+where+order+" LIMIT 2")
		if err != nil {
			t.Fatal(err)
		}
		n := 0
		var trace, span string
		var start int64
		for page.Next() {
			if err := page.Scan(&trace, &span, &start); err != nil {
				t.Fatal(err)
			}
			pages = append(pages, fmt.Sprint([]any{trace, span, start}))
			n++
		}
		if err := page.Err(); err != nil {
			t.Fatal(err)
		}
		page.Close()
		if n < 2 {
			break
		}
		where = andForm(start, trace, span)
	}
	if got := strings.Join(pages, " "); got != full || full != "[t3 s4 400] [t1 s6 200] [t1 s2 200] [t1 s1 100]" {
		t.Fatalf("keyset pages %s, want %s", got, full)
	}
}

// A row-value comparison is rejected by both engines (Java: SemanticException
// on a comparand of complex type).
func TestFDB_OtelRowValueComparisonRejected(t *testing.T) {
	t.Parallel()
	db, ctx, _ := otelShapesDB(t, "rvc")
	_, err := db.QueryContext(ctx, "SELECT trace_id FROM spans WHERE (start_ns, trace_id, span_id) < (400, 't3', 's4')")
	var apiErr *api.Error
	if !errors.As(err, &apiErr) || apiErr.Code != api.ErrCodeUnsupportedQuery {
		t.Fatalf("row-value comparison: %v, want %s", err, api.ErrCodeUnsupportedQuery)
	}
}
