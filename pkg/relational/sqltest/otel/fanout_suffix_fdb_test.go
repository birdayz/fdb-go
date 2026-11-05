package sqltest

import (
	"context"
	"fmt"
	"math"
	"strings"
	"testing"

	"fdb.dev/pkg/relational/sqltest/testkit"
)

// Omitting both PK columns forces suffix discovery through SQL metadata.
// This is Go's single-type read-side extension, not a Java plan-parity claim.
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

func TestFDB_OtelFanOutElementRangeRequiresSort(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := testkit.SetupPlanShapeDB(t, "fanout_element_range", `
CREATE TYPE AS STRUCT attr (scope STRING, k STRING, iv BIGINT)
CREATE TABLE spans (id BIGINT, start_ns BIGINT, attrs attr ARRAY, PRIMARY KEY (id))
CREATE INDEX spans_attr AS SELECT a.scope, a.k, a.iv, s.start_ns
  FROM spans AS s, (SELECT scope, k, iv FROM s.attrs) AS a
  ORDER BY a.scope, a.k, a.iv, s.start_ns`)
	testkit.MustExecCtx(t, db, ctx, `INSERT INTO spans VALUES
(1,300,[('span','tokens',101)]), (2,100,[('span','tokens',200),('span','tokens',250)]),
(3,200,[('span','tokens',150)]), (4,50,[('span','tokens',99)])`)
	const query = "SELECT s.start_ns FROM spans AS s WHERE EXISTS (SELECT 1 FROM s.attrs AS a " +
		"WHERE a.scope = 'span' AND a.k = 'tokens' AND a.iv > 100) ORDER BY s.start_ns"
	if plan := testkit.Explainer(t, db, ctx)(query); !strings.Contains(plan, "IndexScan(SPANS_ATTR, [=, =, <>") || !strings.Contains(plan, "InMemorySort") {
		t.Fatalf("range-bound element cannot order the time suffix: %s", plan)
	}
	if got := otelRows(t, db, ctx, query); got != "[100] [200] [300]" {
		t.Fatalf("range-bound element time order = %s", got)
	}
}

func TestFDB_OtelFanOutSignedZeroEqualityRequiresSort(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := testkit.SetupPlanShapeDB(t, "fanout_signed_zero", `
CREATE TYPE AS STRUCT attr (z DOUBLE, payload STRING)
CREATE TABLE items (id BIGINT, b DOUBLE, attrs attr ARRAY, PRIMARY KEY (id))
CREATE INDEX attr_z_b AS SELECT a.z, r.b FROM items AS r, (SELECT z FROM r.attrs) AS a ORDER BY a.z, r.b`)
	testkit.MustExecCtx(t, db, ctx, `INSERT INTO items VALUES
(1,0.0,[(-0.0,'x')]), (2,-0.0,[(0.0,'x')]), (3,0.0,[(1.0,'x')]), (4,1.0,[(0.0,'x')])`)
	for _, id := range []int64{1, 2} {
		var element, scalar float64
		if err := db.QueryRowContext(ctx, "SELECT a.z, r.b FROM items AS r, r.attrs AS a WHERE r.id = ?", id).Scan(&element, &scalar); err != nil {
			t.Fatal(err)
		}
		if element != 0 || scalar != 0 || math.Signbit(element) != (id == 1) || math.Signbit(scalar) != (id == 2) {
			t.Fatalf("row %d lost signed-zero counterexample: element=%x scalar=%x", id, math.Float64bits(element), math.Float64bits(scalar))
		}
	}
	const query = "SELECT r.id, r.b FROM items AS r WHERE r.b = 0.0 AND EXISTS (SELECT 1 FROM r.attrs AS a WHERE a.z = 0.0) ORDER BY r.b "
	for _, tc := range []struct {
		direction string
		ids       []int64
	}{{"ASC", []int64{2, 1}}, {"DESC", []int64{1, 2}}} {
		for _, limit := range []string{"", " LIMIT 1"} {
			q := query + tc.direction + limit
			plan := testkit.Explainer(t, db, ctx)(q)
			if !strings.Contains(plan, "IndexScan(ATTR_Z_B, [=, =]") || !strings.Contains(plan, "InMemorySort") {
				t.Fatalf("two widened equalities require a sort above the fan-out scan: %s", plan)
			}
			rows, err := db.QueryContext(ctx, q)
			if err != nil {
				t.Fatal(err)
			}
			n := 0
			for rows.Next() {
				var id int64
				var scalar float64
				if err := rows.Scan(&id, &scalar); err != nil {
					rows.Close()
					t.Fatal(err)
				}
				if n >= len(tc.ids) || id != tc.ids[n] || scalar != 0 || math.Signbit(scalar) != (id == 2) {
					rows.Close()
					t.Fatalf("%s row %d = (%d,%x), want IDs %v with stored signs", q, n, id, math.Float64bits(scalar), tc.ids)
				}
				n++
			}
			if err := rows.Err(); err != nil {
				rows.Close()
				t.Fatal(err)
			}
			rows.Close()
			wantCount := len(tc.ids)
			if limit != "" {
				wantCount = 1
			}
			if n != wantCount {
				t.Fatalf("%s returned %d rows, want %d", q, n, wantCount)
			}
		}
	}
}

func TestFDB_OtelResidualFanOutEqualityPreservesRows(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := testkit.SetupPlanShapeDB(t, "residual_fanout", `
CREATE TYPE AS STRUCT attr (k BIGINT, payload STRING)
CREATE TABLE items (a BIGINT, id BIGINT, b BIGINT, attrs attr ARRAY, PRIMARY KEY (id))
CREATE INDEX fan AS SELECT r.a, x.k FROM items AS r, (SELECT k FROM r.attrs) AS x ORDER BY r.a, x.k
CREATE INDEX bb ON items (b,a)`)
	testkit.MustExecCtx(t, db, ctx, `INSERT INTO items VALUES
(1,1,1,[(2,'x'),(7,'x')]), (1,2,1,[(1,'x'),(7,'x')]),
(1,3,2,[(7,'x')]), (1,4,1,[(2,'x')])`)
	for _, prefix := range []string{"r.a >= 0 AND ", ""} {
		query := "SELECT r.a, r.id FROM items AS r WHERE " + prefix +
			"r.b = 1 AND EXISTS (SELECT 1 FROM r.attrs AS x WHERE x.k = 7) ORDER BY r.a, r.id"
		plan := testkit.Explainer(t, db, ctx)(query)
		if !strings.Contains(plan, "IndexScan(BB, [=") || strings.Contains(plan, "Intersection") {
			t.Fatalf("residual element equality must not admit the unordered intersection: %s", plan)
		}
		if got := otelRows(t, db, ctx, query); got != "[1 1] [1 2]" {
			t.Fatalf("residual fan-out equality rows = %s", got)
		}
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
