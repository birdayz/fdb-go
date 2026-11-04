package sqltest

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"fdb.dev/pkg/relational/sqltest/testkit"
)

// The index deliberately omits both primary-key columns: the SQL metadata and
// candidate wiring must discover the suffix. This pins Go's existing single-type
// read-side extension past Java's record-type ordering horizon (DIVERGENCES.md),
// not a claim that Java selects the same implicit-PK plan.
func TestFDB_OtelImplicitFanOutPKSuffix(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := testkit.SetupPlanShapeDB(t, "implicit_fanout_pk", `
CREATE TYPE AS STRUCT attr (scope STRING, k STRING, sv STRING)
CREATE TABLE spans (trace_id STRING, span_id STRING, start_ns BIGINT, attrs attr ARRAY,
  PRIMARY KEY (trace_id, span_id))
CREATE INDEX spans_attr AS SELECT a.scope, a.k, a.sv, s.start_ns
  FROM spans AS s, (SELECT scope, k, sv FROM s.attrs) AS a
  ORDER BY a.scope, a.k, a.sv, s.start_ns`)
	testkit.MustExecCtx(t, db, ctx, `INSERT INTO spans VALUES
('t1','s1',100,[('span','model','gpt')]),
('t1','s2',200,[('span','model','gpt')]),
('t1','s3',200,[('span','model','gpt'),('span','model','other')]),
('t2','s1',200,[('span','model','gpt')]),
('t3','s1',300,[('span','model','gpt')])`)
	const exists = "EXISTS (SELECT 1 FROM s.attrs AS a WHERE a.scope = 'span' AND a.k = 'model' AND a.sv = 'gpt')"
	const selectRows = "SELECT s.trace_id, s.span_id, s.start_ns FROM spans AS s WHERE "
	const order = " ORDER BY s.start_ns DESC, s.trace_id DESC, s.span_id DESC"
	query := selectRows + exists + order
	plan := testkit.Explainer(t, db, ctx)(query)
	if !strings.Contains(plan, "IndexScan(SPANS_ATTR") || !strings.Contains(plan, "REVERSE") || strings.Contains(plan, "InMemorySort") {
		t.Fatalf("implicit suffix must provide time/trace/span order: %s", plan)
	}
	want := "[t3 s1 300] [t2 s1 200] [t1 s3 200] [t1 s2 200] [t1 s1 100]"
	if got := otelRows(t, db, ctx, query); got != want {
		t.Fatalf("ordered implicit suffix rows = %s, want %s", got, want)
	}
	var pages []string
	where := ""
	finished := false
	for range 4 {
		pageQuery := selectRows + exists + where + order + " LIMIT 2"
		pagePlan := testkit.Explainer(t, db, ctx)(pageQuery)
		if !strings.Contains(pagePlan, "IndexScan(SPANS_ATTR") || strings.Contains(pagePlan, "InMemorySort") {
			t.Fatalf("implicit suffix page lost its index order: %s", pagePlan)
		}
		rows, err := db.QueryContext(ctx, pageQuery)
		if err != nil {
			t.Fatal(err)
		}
		var trace, span string
		var start int64
		count := 0
		for rows.Next() {
			if err := rows.Scan(&trace, &span, &start); err != nil {
				t.Fatal(err)
			}
			pages = append(pages, fmt.Sprint([]any{trace, span, start}))
			count++
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		rows.Close()
		if count < 2 {
			finished = true
			break
		}
		where = fmt.Sprintf(" AND s.start_ns <= %d AND (s.start_ns < %d OR s.trace_id < '%s' OR (s.trace_id = '%s' AND s.span_id < '%s'))", start, start, trace, trace, span)
	}
	if !finished || strings.Join(pages, " ") != want {
		t.Fatalf("implicit suffix pages = %v, finished=%t, want %s", pages, finished, want)
	}
	unbound := selectRows + "EXISTS (SELECT 1 FROM s.attrs AS a WHERE a.scope = 'span' AND a.k = 'model')" + order
	unboundPlan := testkit.Explainer(t, db, ctx)(unbound)
	if !strings.Contains(unboundPlan, "IndexScan(SPANS_ATTR") || !strings.Contains(unboundPlan, "InMemorySort") {
		t.Fatalf("unbound element must not order the implicit suffix: %s", unboundPlan)
	}
	if got := otelRows(t, db, ctx, unbound); got != want {
		t.Fatalf("unbound-element rows = %s, want one row per span in %s", got, want)
	}
}

func TestFDB_OtelFanOutElementNameDoesNotTrimRootPK(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := testkit.SetupPlanShapeDB(t, "fanout_pk_name", `
CREATE TYPE AS STRUCT attr (id BIGINT, payload STRING)
CREATE TABLE items (id BIGINT, b BIGINT, attrs attr ARRAY, PRIMARY KEY (id,b))
CREATE INDEX attr_id AS SELECT a.id FROM items AS r, (SELECT id FROM r.attrs) AS a ORDER BY a.id`)
	testkit.MustExecCtx(t, db, ctx, `INSERT INTO items VALUES
(1,9,[(7,'x')]), (2,1,[(7,'x')]), (3,5,[(8,'x')]), (4,2,[(7,'x'),(9,'x')])`)
	const query = "SELECT r.id, r.b FROM items AS r WHERE EXISTS (SELECT 1 FROM r.attrs AS a WHERE a.id = 7)"
	for _, tc := range []struct {
		order, rows string
		sorted      bool
	}{
		{" ORDER BY r.id, r.b", "[1 9] [2 1] [4 2]", false},
		{" ORDER BY r.b", "[2 1] [4 2] [1 9]", true},
	} {
		plan := testkit.Explainer(t, db, ctx)(query + tc.order)
		if !strings.Contains(plan, "IndexScan(ATTR_ID") || strings.Contains(plan, "InMemorySort") != tc.sorted {
			t.Errorf("%s: wrong fan-out/root-PK ordering plan: %s", tc.order, plan)
		}
		if got := otelRows(t, db, ctx, query+tc.order); got != tc.rows {
			t.Errorf("%s: rows = %s, want %s", tc.order, got, tc.rows)
		}
	}
}
