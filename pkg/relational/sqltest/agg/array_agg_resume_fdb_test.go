package sqltest

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"fdb.dev/pkg/relational/sqltest/testkit"

	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/core/embedded"
)

// ARRAY_AGG partial state across repeated resource-limit resumes: one scanned
// row per page, so every element is decoded and re-encoded through the
// continuation once per later row of its group, structured elements included,
// and a group that has seen only ignored NULLs stays a present empty array.
func TestFDB_ArrayAggRepeatedResume(t *testing.T) {
	t.Parallel()
	db := testkit.SetupErrorDB(t, "/FRL/testdb_array_agg_resume", "aaresume",
		"CREATE TYPE AS STRUCT pt (x BIGINT, tag STRING) "+
			"CREATE TABLE t (g BIGINT, id BIGINT, n BIGINT, p pt, PRIMARY KEY (g, id))")
	ctx := context.Background()
	for _, r := range []string{
		"1, 1, NULL, (1, 'a')", "1, 2, NULL, (2, 'b')", "1, 3, 5, (3, 'c')",
		"2, 4, NULL, (4, 'd')", "2, 5, NULL, (5, 'e')",
	} {
		testkit.MustExec(t, db, ctx, "INSERT INTO t VALUES ("+r+")")
	}
	q := "SELECT g, ARRAY_AGG(n IGNORE NULLS), ARRAY_AGG(p), ARRAY_AGG(p.tag LIMIT 2), COUNT(*) FROM t GROUP BY g"
	if plan := testkit.ExplainVia(t, ctx, db, q); !strings.Contains(plan, "StreamingAgg") || strings.Contains(plan, "Sort") {
		t.Fatalf("want a streaming aggregate over the primary-key scan, so each page break lands in its continuation; got\n%s", plan)
	}
	read := func(limit int) string {
		conn := testkit.PinEmbeddedConn(t, db, func(ec *embedded.EmbeddedConnection) {
			if limit > 0 {
				ec.SetOptions(api.NewOptionsBuilder().Set(api.OptExecutionScannedRowsLimit, limit).Build())
			}
		})
		rows, err := conn.QueryContext(ctx, q)
		if err != nil {
			t.Fatalf("limit %d: %v", limit, err)
		}
		defer rows.Close()
		var out []string
		for rows.Next() {
			var g, cnt int64
			var ns, ps, tags any
			if err := rows.Scan(&g, &ns, &ps, &tags, &cnt); err != nil {
				t.Fatalf("limit %d: scan: %v", limit, err)
			}
			out = append(out, fmt.Sprintf("%d %v %v %v %d", g, ns, renderStructs(t, ps), tags, cnt))
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("limit %d: %v", limit, err)
		}
		return strings.Join(out, " | ")
	}
	whole := read(0)
	const want = "1 [5] [{1 a} {2 b} {3 c}] [a b] 3 | 2 [] [{4 d} {5 e}] [d e] 2"
	if whole != want {
		t.Fatalf("unpaged = %q, want %q", whole, want)
	}
	for _, limit := range []int{1, 2} {
		if got := read(limit); got != whole {
			t.Errorf("paged %d row(s) at a time = %q, want %q", limit, got, whole)
		}
	}
}

// renderStructs renders an array of structs as [{attr ...} ...].
func renderStructs(t *testing.T, v any) string {
	t.Helper()
	elems, ok := v.([]any)
	if !ok {
		return fmt.Sprintf("%T(%v)", v, v)
	}
	out := make([]string, len(elems))
	for i, e := range elems {
		st, isStruct := e.(api.Struct)
		if !isStruct {
			t.Fatalf("element %d = %T, want an api.Struct", i, e)
		}
		attrs := make([]string, st.AttributeCount())
		for a := range attrs {
			val, err := st.Attribute(a + 1)
			if err != nil {
				t.Fatal(err)
			}
			attrs[a] = fmt.Sprint(val)
		}
		out[i] = "{" + strings.Join(attrs, " ") + "}"
	}
	return "[" + strings.Join(out, " ") + "]"
}
