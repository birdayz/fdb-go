package sqldriver_test

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	"fdb.dev/pkg/relational/api"
)

// TestFDB_FieldValuedComputedScalar pins a bug where a PARENTHESIZED plain
// column in a correlated scalar projection walks to a bare FieldValue, and the
// materialization must NOT re-key it to `_0` — the projection executor keys
// field-valued projections by the COLUMN NAME, so a seed expecting `_0` fails
// the leg adapter on a valid query.
func TestFDB_FieldValuedComputedScalar(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dbPath := "/w4b_fv"
	setup := openTestDB(t, dbPath)
	if _, err := setup.ExecContext(ctx, "CREATE DATABASE "+dbPath); err != nil {
		t.Fatalf("db: %v", err)
	}
	const tmpl = "w4b_fv_tmpl"
	if _, err := setup.ExecContext(ctx, "CREATE SCHEMA TEMPLATE "+tmpl+
		" CREATE TABLE customers (id BIGINT, name STRING, PRIMARY KEY (id))"+
		" CREATE TABLE orders (id BIGINT, amount BIGINT, PRIMARY KEY (id))"+
		" CREATE TABLE corders (id BIGINT, customer_id BIGINT, PRIMARY KEY (id))"); err != nil {
		t.Fatalf("tmpl: %v", err)
	}
	if _, err := setup.ExecContext(ctx, "CREATE SCHEMA "+dbPath+"/main WITH TEMPLATE "+tmpl); err != nil {
		t.Fatalf("schema: %v", err)
	}
	db, err := sql.Open("fdbsql", "fdbsql://"+strings.ToUpper(dbPath)+"?cluster_file="+clusterFilePath+"&schema=MAIN")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.ExecContext(ctx, "INSERT INTO customers VALUES (1, 'alice')"); err != nil {
		t.Fatalf("seed customers: %v", err)
	}
	if _, err := db.ExecContext(ctx, "INSERT INTO orders VALUES (1, 100)"); err != nil {
		t.Fatalf("seed orders: %v", err)
	}

	// The inner select list is a BARE column, deliberately unparenthesised.
	// It read `(o.amount)`, which is no longer a bare field value: a
	// parenthesised select element is a one-field RECORD (measured against a
	// live JVM, conformance/paren_star_java_probe_test.go), so the
	// parenthesised spelling made this a struct-valued scalar subquery and
	// stopped testing the bare-field path this case is named for. The
	// parenthesised spelling is a separate shape and is pinned separately —
	// see correlated_scalar_join_inner_fdb_test.go case (e).
	var v sql.NullInt64
	const q = "SELECT (SELECT o.amount FROM orders o WHERE o.id = c.id) " +
		"FROM customers c WHERE c.id = 1"
	if err := db.QueryRowContext(ctx, q).Scan(&v); err != nil {
		t.Fatalf("field-valued computed scalar: %v\n  sql: %s", err, q)
	}
	if !v.Valid || v.Int64 != 100 {
		t.Errorf("field-valued computed scalar = %d (valid=%v), want 100", v.Int64, v.Valid)
	}

	// A COMPUTED wrapper around the same column (`(o.amount) + 0`) must still
	// take the materialized `_0` path — the fix routes only BARE field values
	// back to the plain-column path.
	const qComputed = "SELECT (SELECT (o.amount) + 0 FROM orders o WHERE o.id = c.id) " +
		"FROM customers c WHERE c.id = 1"
	if err := db.QueryRowContext(ctx, qComputed).Scan(&v); err != nil {
		t.Fatalf("computed-over-parenthesized scalar: %v\n  sql: %s", err, qComputed)
	}
	if !v.Valid || v.Int64 != 100 {
		t.Errorf("computed-over-parenthesized scalar = %d (valid=%v), want 100", v.Int64, v.Valid)
	}

	// An OUTER-scope parenthesized column (`(c.id)`) is NOT an inner row key at
	// all: routing it to the plain-column path strips the outer qualifier and
	// silently reads the INNER column of the same name (the order id, not the
	// customer id). It must take the MATERIALIZED path — its value comes from
	// the outer binding, evaluated per outer row. The seeded order (id=5,
	// customer_id=1) keeps the ids DISTINCT: want the outer c.id=1, never the
	// inner o.id=5.
	if _, err := db.ExecContext(ctx, "INSERT INTO corders VALUES (5, 1)"); err != nil {
		t.Fatalf("seed corder: %v", err)
	}
	//
	// The subquery's sole element is `(c.id)`, a ONE-FIELD RECORD, so the
	// column is a STRUCT and the id is read through it rather than scanned as
	// an int64. That is not incidental to this test — `(expr)` is a record in
	// Java too, MEASURED on the live JVM
	// (conformance/paren_star_java_probe_test.go, `column_one_paren`: JAVA
	// `_0(STRUCT)` `{VAL: 10}`) — and the scope question this test exists for
	// is unchanged by the wrapper: it is asked of the value INSIDE it.
	//
	// A scalar subquery in SELECT position is itself a Go-only read-side
	// extension: all four of the probe's subquery arms are 42601 syntax errors
	// on the live JVM, so there is no Java behaviour here to conform to, only
	// Go's own one-element rule applied consistently inside a construct Java
	// cannot parse.
	const qOuter = "SELECT (SELECT (c.id) FROM corders o WHERE o.customer_id = c.id) " +
		"FROM customers c WHERE c.id = 1"
	var outer any
	if err := db.QueryRowContext(ctx, qOuter).Scan(&outer); err != nil {
		t.Fatalf("outer-scope parenthesized scalar: %v\n  sql: %s", err, qOuter)
	}
	os, isStruct := outer.(api.Struct)
	if !isStruct {
		t.Fatalf("outer-scope parenthesized scalar: got %T, want an api.Struct (a one-element record constructor)", outer)
	}
	if got := os.Attributes(); len(got) != 1 || got[0] != int64(1) {
		t.Errorf("outer-scope parenthesized scalar = %v, want [1] (the OUTER c.id; the inner o.id=5 means the bared key read the wrong scope)", got)
	}

	for _, label := range []string{"MiXeD", "literal.dot"} {
		for _, clustered := range []bool{false, true} {
			name := "single/" + label
			from := "customers AS c WHERE c.id = 1"
			if clustered {
				name = "clustered/" + label
				from = "customers AS c, customers AS x WHERE c.id = 1 AND x.id = 1"
			}
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				query := `SELECT (SELECT o.amount AS "` + label + `" FROM orders AS o WHERE o.id = c.id) AS "` + label + `" FROM ` + from
				var firstPlan string
				for i := 0; i < 5; i++ {
					var plan string
					if err := db.QueryRowContext(ctx, "EXPLAIN "+query).Scan(&plan); err != nil {
						t.Fatal(err)
					}
					if !strings.Contains(plan, "FlatMap") || (clustered && !strings.Contains(plan, "NestedLoopJoin")) {
						t.Fatalf("scalar seed route missing: %s", plan)
					}
					if i == 0 {
						firstPlan = plan
					} else if plan != firstPlan {
						t.Fatalf("private identity leaked into EXPLAIN:\n%s\n%s", firstPlan, plan)
					}
				}
				rows, err := db.QueryContext(ctx, query)
				if err != nil {
					t.Fatal(err)
				}
				defer rows.Close()
				columns, err := rows.Columns()
				if err != nil || len(columns) != 1 || columns[0] != label {
					t.Fatalf("scalar public labels=(%v,%v), want [%s]", columns, err, label)
				}
				types, err := rows.ColumnTypes()
				if err != nil || len(types) != 1 || types[0].DatabaseTypeName() != "BIGINT" {
					t.Fatalf("scalar public types=(%v,%v), want one BIGINT", types, err)
				}
				if nullable, known := types[0].Nullable(); !known || !nullable {
					t.Fatalf("scalar nullability=(%v,%v), want known nullable", nullable, known)
				}
				if !rows.Next() {
					t.Fatalf("scalar row missing: %v", rows.Err())
				}
				var got int64
				if err := rows.Scan(&got); err != nil || got != 100 {
					t.Fatalf("scalar=(%d,%v), want 100", got, err)
				}
				if rows.Next() || rows.Err() != nil {
					t.Fatalf("unexpected extra row or iteration error: %v", rows.Err())
				}
			})
		}
	}

	// (The AT-ordinal-alias scope corner — `AS v AT c` colliding with an outer
	// alias `c` — is pinned white-box in the embedded package
	// (TestInnerSourceAliases_MirrorsUnnestBinder): SQL INSERT cannot seed
	// array columns, and the rule under test is exactly the binder mirror.)
}

// TestFDB_AggregateLocalOuterRef pins a clustered-outer correlated scalar whose
// AGGREGATE OPERAND references an outer column. Aggregate operands evaluate
// against the aggregate's inner input row, which does not carry the level-2
// positional outer binding — the pull-up must NOT bake aggregate-local outer
// refs. This shape has NEVER planned (0AF00 on master, verified in a master
// worktree); an un-guarded bake would regress the clean plan error into a loud
// baked-ref runtime error. The pin asserts master parity: the clean 0AF00,
// never the runtime error. (Making the shape actually WORK needs aggregate
// evaluation to receive the outer binding — a separate follow-on.)
func TestFDB_AggregateLocalOuterRef(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dbPath := "/w4b_ag"
	setup := openTestDB(t, dbPath)
	if _, err := setup.ExecContext(ctx, "CREATE DATABASE "+dbPath); err != nil {
		t.Fatalf("db: %v", err)
	}
	const tmpl = "w4b_ag_tmpl"
	if _, err := setup.ExecContext(ctx, "CREATE SCHEMA TEMPLATE "+tmpl+
		" CREATE TABLE customers (id BIGINT, name STRING, PRIMARY KEY (id))"+
		" CREATE TABLE extras (id BIGINT, ref BIGINT, PRIMARY KEY (id))"+
		" CREATE TABLE orders (id BIGINT, customer_id BIGINT, amount BIGINT, PRIMARY KEY (id))"); err != nil {
		t.Fatalf("tmpl: %v", err)
	}
	if _, err := setup.ExecContext(ctx, "CREATE SCHEMA "+dbPath+"/main WITH TEMPLATE "+tmpl); err != nil {
		t.Fatalf("schema: %v", err)
	}
	db, err := sql.Open("fdbsql", "fdbsql://"+strings.ToUpper(dbPath)+"?cluster_file="+clusterFilePath+"&schema=MAIN")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.ExecContext(ctx, "INSERT INTO customers VALUES (1, 'alice')"); err != nil {
		t.Fatalf("seed customers: %v", err)
	}
	if _, err := db.ExecContext(ctx, "INSERT INTO extras VALUES (10, 7)"); err != nil {
		t.Fatalf("seed extras: %v", err)
	}
	// alice: orders 100 + 300; SUM(amount + e.ref) = 100+7 + 300+7 = 414.
	if _, err := db.ExecContext(ctx, "INSERT INTO orders VALUES (1,1,100),(2,1,300)"); err != nil {
		t.Fatalf("seed orders: %v", err)
	}

	// The aggregate operand references E — the RIGHTMOST outer leg. The
	// ordinal path must decline (un-bakeable aggregate-local carrier) and the
	// query falls to the name-model path, which cannot plan it — exactly as on
	// master. A runtime baked-ref error here means the guard regressed.
	var v sql.NullInt64
	const q = "SELECT (SELECT SUM(o.amount + e.ref) FROM orders o WHERE o.customer_id = c.id) " +
		"FROM customers c, extras e WHERE c.id = 1 AND e.id = 10"
	err = db.QueryRowContext(ctx, q).Scan(&v)
	if err == nil {
		t.Fatalf("aggregate-local outer ref unexpectedly planned: got %d (valid=%v) — flip this pin to rows if the shape gained support", v.Int64, v.Valid)
	}
	if !strings.Contains(err.Error(), "0AF00") {
		t.Errorf("aggregate-local outer ref error = %v, want master-parity 0AF00 (clean plan error), never a baked-ref runtime error", err)
	}
}
