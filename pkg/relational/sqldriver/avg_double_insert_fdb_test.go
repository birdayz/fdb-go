package sqldriver_test

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	"fdb.dev/pkg/relational/api"
)

// avgInsertDB sets up src (seeded 10/20/30), a BIGINT-valued dbig, and a
// DOUBLE-valued ddbl for AVG/SUM INSERT…SELECT promotion tests.
func avgInsertDB(t *testing.T, tag string) (*sql.DB, context.Context) {
	t.Helper()
	ctx := context.Background()
	dbPath := "/avgins_" + tag
	setup := openTestDB(t, dbPath)
	if _, err := setup.ExecContext(ctx, "CREATE DATABASE "+dbPath); err != nil {
		t.Fatalf("db: %v", err)
	}
	tmpl := "avgins_tmpl_" + tag
	if _, err := setup.ExecContext(ctx, "CREATE SCHEMA TEMPLATE "+tmpl+
		" CREATE TABLE src (id BIGINT, v BIGINT, PRIMARY KEY (id))"+
		" CREATE TABLE dbig (id BIGINT, v BIGINT, PRIMARY KEY (id))"+
		" CREATE TABLE ddbl (id BIGINT, v DOUBLE, PRIMARY KEY (id))"); err != nil {
		t.Fatalf("tmpl: %v", err)
	}
	if _, err := setup.ExecContext(ctx, "CREATE SCHEMA "+dbPath+"/main WITH TEMPLATE "+tmpl); err != nil {
		t.Fatalf("schema: %v", err)
	}
	db, err := sql.Open("fdbsql", "fdbsql://"+dbPath+"?cluster_file="+clusterFilePath+"&schema=main")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.ExecContext(ctx, "INSERT INTO src VALUES (1, 10), (2, 20), (3, 30)"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	return db, ctx
}

// TestFDB_AvgDoubleInsertPromotion pins the plan-time INSERT…SELECT promotion
// guard: AVG(BIGINT) types DOUBLE, and DOUBLE→BIGINT has no edge in Java's
// promotion lattice, so the INSERT is rejected with SQLSTATE 22000 — matching
// Java's plan-time PromoteValue, INDEPENDENT of how many rows the source yields.
func TestFDB_AvgDoubleInsertPromotion(t *testing.T) {
	t.Parallel()
	db, ctx := avgInsertDB(t, "promo")

	t.Run("avg_into_bigint_rejected", func(t *testing.T) {
		_, err := db.ExecContext(ctx, "INSERT INTO dbig SELECT 100, AVG(v) FROM src")
		requireSQLSTATE(t, err, api.ErrCodeCannotConvertType)
	})

	// The empty-source axis (reviewer NAK): even with ZERO rows the rejection
	// fires, because it derives from the structural type, not a materialized
	// value — the runtime converter never sees a float here.
	t.Run("avg_into_bigint_empty_source_rejected", func(t *testing.T) {
		_, err := db.ExecContext(ctx, "INSERT INTO dbig SELECT 101, AVG(v) FROM src WHERE id > 999")
		requireSQLSTATE(t, err, api.ErrCodeCannotConvertType)
	})

	// The tree-contains-aggregate axis: AVG(v)+1 has a top-level
	// ArithmeticValue but still types DOUBLE; rejected even over an empty source.
	// A top-level type assert would miss this; the WalkValue provenance catches it.
	t.Run("avg_plus_one_into_bigint_empty_source_rejected", func(t *testing.T) {
		_, err := db.ExecContext(ctx, "INSERT INTO dbig SELECT 102, AVG(v) + 1 FROM src WHERE id > 999")
		requireSQLSTATE(t, err, api.ErrCodeCannotConvertType)
	})

	// AVG into a DOUBLE column is accepted (DOUBLE→DOUBLE) and correct.
	t.Run("avg_into_double_ok", func(t *testing.T) {
		if _, err := db.ExecContext(ctx, "INSERT INTO ddbl SELECT 200, AVG(v) FROM src"); err != nil {
			t.Fatalf("AVG→DOUBLE should be accepted: %v", err)
		}
		var got float64
		if err := db.QueryRowContext(ctx, "SELECT v FROM ddbl WHERE id = 200").Scan(&got); err != nil {
			t.Fatalf("read back: %v", err)
		}
		if got != 20.0 {
			t.Fatalf("AVG(10,20,30) = %v, want 20.0", got)
		}
	})

	// SUM(BIGINT) types BIGINT → accepted (LONG→LONG): the guard does NOT
	// over-reject non-AVG aggregates.
	t.Run("sum_into_bigint_ok", func(t *testing.T) {
		if _, err := db.ExecContext(ctx, "INSERT INTO dbig SELECT 300, SUM(v) FROM src"); err != nil {
			t.Fatalf("SUM→BIGINT should be accepted: %v", err)
		}
		var got int64
		if err := db.QueryRowContext(ctx, "SELECT v FROM dbig WHERE id = 300").Scan(&got); err != nil {
			t.Fatalf("read back: %v", err)
		}
		if got != 60 {
			t.Fatalf("SUM(10,20,30) = %d, want 60", got)
		}
	})

	// SUM(BIGINT) into a DOUBLE column → accepted (LONG→DOUBLE widening). Pins
	// the converged goToProtoValue: this path previously errored ("cannot
	// convert int64 to proto field kind double").
	t.Run("sum_into_double_ok", func(t *testing.T) {
		if _, err := db.ExecContext(ctx, "INSERT INTO ddbl SELECT 301, SUM(v) FROM src"); err != nil {
			t.Fatalf("SUM→DOUBLE should be accepted (LONG→DOUBLE): %v", err)
		}
		var got float64
		if err := db.QueryRowContext(ctx, "SELECT v FROM ddbl WHERE id = 301").Scan(&got); err != nil {
			t.Fatalf("read back: %v", err)
		}
		if got != 60.0 {
			t.Fatalf("SUM→DOUBLE = %v, want 60.0", got)
		}
	})

	// Plain BIGINT-column arithmetic into BIGINT is NOT guarded (no aggregate
	// in the tree) — proves provenance-keying, not type-keying: plain-column
	// projections are never false-rejected by this guard.
	t.Run("plain_arith_into_bigint_ok", func(t *testing.T) {
		if _, err := db.ExecContext(ctx, "INSERT INTO dbig SELECT 400, v + 1 FROM src WHERE id = 1"); err != nil {
			t.Fatalf("plain v+1 → BIGINT should be accepted: %v", err)
		}
		var got int64
		if err := db.QueryRowContext(ctx, "SELECT v FROM dbig WHERE id = 400").Scan(&got); err != nil {
			t.Fatalf("read back: %v", err)
		}
		if got != 11 {
			t.Fatalf("v+1 = %d, want 11", got)
		}
	})
}

// TestFDB_AvgDoubleValuesInsert pins the INSERT…VALUES path: a DOUBLE literal
// into a BIGINT column is rejected (the removed whole-float coercion) — Java's
// lattice has no DOUBLE→LONG edge, value-independent (whole or fractional).
func TestFDB_AvgDoubleValuesInsert(t *testing.T) {
	t.Parallel()
	db, ctx := avgInsertDB(t, "values")

	t.Run("whole_double_into_bigint_rejected", func(t *testing.T) {
		_, err := db.ExecContext(ctx, "INSERT INTO dbig VALUES (500, 5.0)")
		requireSQLSTATE(t, err, api.ErrCodeCannotConvertType)
	})
	t.Run("fractional_double_into_bigint_rejected", func(t *testing.T) {
		_, err := db.ExecContext(ctx, "INSERT INTO dbig VALUES (501, 5.5)")
		requireSQLSTATE(t, err, api.ErrCodeCannotConvertType)
	})
	t.Run("double_into_double_ok", func(t *testing.T) {
		if _, err := db.ExecContext(ctx, "INSERT INTO ddbl VALUES (502, 5.5)"); err != nil {
			t.Fatalf("5.5 → DOUBLE should be accepted: %v", err)
		}
	})
	// An integer literal into a DOUBLE column widens (INT/LONG→DOUBLE).
	t.Run("int_into_double_ok", func(t *testing.T) {
		if _, err := db.ExecContext(ctx, "INSERT INTO ddbl VALUES (503, 7)"); err != nil {
			t.Fatalf("7 → DOUBLE should be accepted: %v", err)
		}
	})
}

// TestFDB_AvgRuntimeTypeAndNull pins AVG's runtime semantics (unchanged): DOUBLE
// value, NULL over an empty group.
func TestFDB_AvgRuntimeTypeAndNull(t *testing.T) {
	t.Parallel()
	db, ctx := avgInsertDB(t, "runtime")

	var avg float64
	if err := db.QueryRowContext(ctx, "SELECT AVG(v) FROM src").Scan(&avg); err != nil {
		t.Fatalf("AVG: %v", err)
	}
	if avg != 20.0 {
		t.Fatalf("AVG = %v, want 20.0", avg)
	}

	var n sql.NullFloat64
	if err := db.QueryRowContext(ctx, "SELECT AVG(v) FROM src WHERE id > 999").Scan(&n); err != nil {
		t.Fatalf("AVG empty: %v", err)
	}
	if n.Valid {
		t.Fatalf("AVG over empty group = %v, want NULL", n.Float64)
	}
}

// TestFDB_AvgWithAggregateIndexPresent — reviewer's index-presence axis. AVG has
// NO aggregate index (it always streams), so a SUM aggregate index on the table
// cannot shift AVG's typing: AVG still types DOUBLE and rejects into BIGINT, and
// EXPLAIN shows a streaming aggregation, not an AggregateIndex scan.
func TestFDB_AvgWithAggregateIndexPresent(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dbPath := "/avgins_idx"
	setup := openTestDB(t, dbPath)
	if _, err := setup.ExecContext(ctx, "CREATE DATABASE "+dbPath); err != nil {
		t.Fatalf("db: %v", err)
	}
	if _, err := setup.ExecContext(ctx, "CREATE SCHEMA TEMPLATE avgins_idx_tmpl"+
		" CREATE TABLE src (id BIGINT, g BIGINT, v BIGINT, PRIMARY KEY (id))"+
		" CREATE TABLE dbig (id BIGINT, s BIGINT, PRIMARY KEY (id))"+
		" CREATE INDEX sum_by_g AS SELECT SUM(v) FROM src GROUP BY g"); err != nil {
		t.Fatalf("tmpl: %v", err)
	}
	if _, err := setup.ExecContext(ctx, "CREATE SCHEMA "+dbPath+"/main WITH TEMPLATE avgins_idx_tmpl"); err != nil {
		t.Fatalf("schema: %v", err)
	}
	db, err := sql.Open("fdbsql", "fdbsql://"+dbPath+"?cluster_file="+clusterFilePath+"&schema=main")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.ExecContext(ctx, "INSERT INTO src VALUES (1,1,10),(2,1,20),(3,2,30)"); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// AVG must NOT use the aggregate-index path (it has none) — it streams, even
	// with a SUM aggregate index present on the same table/grouping.
	plan := planExplainVia(t, ctx, db, "SELECT g, AVG(v) FROM src GROUP BY g")
	if strings.Contains(plan, "AggregateIndex") {
		t.Fatalf("AVG must not use an AggregateIndex (it has none); plan:\n%s", plan)
	}

	// And a (scalar) AVG→BIGINT still rejects: index presence does not change
	// AVG's DOUBLE typing. (The bare GROUP BY-aggregate INSERT…SELECT source is a
	// LogicalAggregate without a Project — deferred to the PromoteValue follow-up;
	// the scalar form exercises the same DOUBLE-typing-under-index property.)
	_, err = db.ExecContext(ctx, "INSERT INTO dbig SELECT 1, AVG(v) FROM src")
	requireSQLSTATE(t, err, api.ErrCodeCannotConvertType)
}
