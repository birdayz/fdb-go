package sqldriver_test

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
)

// ORDER BY over a whole STRUCT must fail at PLAN TIME, not leak a raw internal
// comparator error at row time.
//
// Java's route: LogicalOperator.generateSort (LogicalOperator.java:552-571)
// feeds RequestedOrdering.ofPrimitiveParts (RequestedOrdering.java:313-326),
// which — unlike the GROUPING path's Values.primitiveAccessorsForType
// (Values.java:99-121) — never expands a record into its primitive leaves. The
// record-typed ordering therefore matches no index ordering, Cascades ends
// with no final expression, and UnableToPlanException
// (CascadesPlanner.java:407) surfaces as 0AF00 (ExceptionUtil.java:79-80).
//
// Go reached a different place for the same query ONLY because Go has an
// in-memory sort fallback that Java's Cascades does not. The fallback accepted
// the record key and the comparator failed at ROW TIME with
// "values: no ordering defined between *dynamicpb.Message and
// *dynamicpb.Message" — an internal message, leaked to the user, and
// data-dependently (a single-row table never compares anything and answers
// fine). ImplementInMemorySortRule now declines the unorderable key, so no
// plan survives and Go fails the same way Java does.
func TestFDB_StructOrderingGate(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	setup := openTestDB(t, "/structordergate")
	if _, err := setup.ExecContext(ctx, "CREATE DATABASE /structordergate"); err != nil {
		t.Fatalf("db: %v", err)
	}
	if _, err := setup.ExecContext(ctx,
		"CREATE SCHEMA TEMPLATE sog_tmpl "+
			"CREATE TYPE AS STRUCT ADDR (city STRING, zip BIGINT) "+
			"CREATE TABLE T_S (id BIGINT, home ADDR, PRIMARY KEY (id))"); err != nil {
		t.Fatalf("tmpl: %v", err)
	}
	if _, err := setup.ExecContext(ctx, "CREATE SCHEMA /structordergate/s WITH TEMPLATE sog_tmpl"); err != nil {
		t.Fatalf("schema: %v", err)
	}
	dsn := fmt.Sprintf("fdbsql:///structordergate?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	// TWO rows is load-bearing. The leak is DATA-DEPENDENT: with one row the
	// sort never invokes the comparator and the query answers normally, so a
	// single-row fixture cannot express the defect at all.
	for _, ins := range []string{
		"INSERT INTO T_S VALUES (1, ('sf', 94100))",
		"INSERT INTO T_S VALUES (2, ('la', 90001))",
	} {
		if _, err := db.ExecContext(ctx, ins); err != nil {
			t.Fatalf("%s: %v", ins, err)
		}
	}

	drain := func(q string) error {
		rows, err := db.QueryContext(ctx, q)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var v any
			if err := rows.Scan(&v); err != nil {
				return err
			}
		}
		return rows.Err()
	}

	// mustRejectCleanly requires a rejection that does NOT expose engine
	// internals. The specific internal string is named so the test states what
	// it is defending against rather than merely asserting "some error".
	mustRejectCleanly := func(t *testing.T, q string) {
		t.Helper()
		err := drain(q)
		if err == nil {
			t.Fatalf("ORDER BY over a whole struct ANSWERED instead of failing to plan.\nquery: %s\n"+
				"sortKeysAreOrderable in ImplementInMemorySortRule is no longer declining the "+
				"record-typed key, so Go's in-memory sort fallback is accepting an ordering "+
				"Java cannot satisfy.", q)
		}
		if strings.Contains(err.Error(), "no ordering defined between") {
			t.Fatalf("ORDER BY over a whole struct leaked the RAW INTERNAL comparator error.\n"+
				"query: %s\ngot:   %v\n"+
				"This is the row-time leak the plan-time decline exists to prevent; it names "+
				"*dynamicpb.Message to the user and only appears when two rows actually get "+
				"compared.", q, err)
		}
		if !strings.Contains(err.Error(), "0AF00") {
			t.Fatalf("ORDER BY over a whole struct rejected with an unexpected error.\n"+
				"query: %s\ngot:   %v\nwant 0AF00 (Java: UnableToPlanException)", q, err)
		}
	}

	t.Run("order_by_struct_fails_to_plan", func(t *testing.T) {
		t.Parallel()
		mustRejectCleanly(t, "SELECT id FROM T_S ORDER BY home")
	})

	// KNOWN REMAINING GAP, characterized deliberately rather than hidden.
	//
	// Through a DERIVED TABLE the same query still leaks the raw comparator
	// error, and the cause is a layer BELOW the decline above: the ORDER BY key
	// never reaches the semantic scope at all. translateSort mints the carrier
	// as `&values.FieldValue{Field: k.Expr, Typ: values.UnknownType}` and then
	// A DERIVED-TABLE ORDER BY over a struct column. It used to LEAK the raw
	// comparator error ("no ordering defined between …") because the two paths
	// that type a column disagreed: the comparison operand was typed by the
	// semantic resolver while the sort key was baked by NAME against the input
	// column list (names only, no types), so the key reached
	// ImplementInMemorySortRule typed UNKNOWN — which the rule admits on
	// purpose, since a bound parameter is not evidence of an unorderable key.
	//
	// Resolving the key through the scope closed that: it now carries its real
	// RECORD type and is refused at planning, the same 0AF00 the base-table
	// case produces. The characterization test that recorded the leak has been
	// replaced by this pin, exactly as its own note instructed.
	t.Run("order_by_struct_through_derived_rejects_cleanly", func(t *testing.T) {
		t.Parallel()
		mustRejectCleanly(t, "SELECT x.id FROM (SELECT id, home AS h FROM T_S) x ORDER BY x.h")
	})

	// A RECURSIVE CTE reaches the decline too — but only because BOTH fixes are
	// present, which is why it is pinned separately from the base-table case.
	// The CTE mint site's StructFields carry is what gives the sort key a
	// RECORD type at all; the decline is what then refuses it. Revert the carry
	// and this query leaks the raw comparator error again (measured), so a
	// regression in either half re-arms it.
	t.Run("recursive_cte_order_by_struct_fails_to_plan", func(t *testing.T) {
		t.Parallel()
		mustRejectCleanly(t,
			"WITH RECURSIVE r AS ("+
				"SELECT id, home AS h FROM T_S "+
				"UNION ALL SELECT id, h FROM r WHERE id < 0) "+
				"SELECT r.id FROM r ORDER BY r.h")
	})

	// COUNTERWEIGHT. The decline must be keyed on the sort key's TYPE, not on
	// "this table has a struct column" — a scalar ORDER BY on the same table,
	// and an ORDER BY on a struct's LEAF field, must both still plan and sort.
	// Without this, a decline that simply gave up whenever a struct was in
	// scope would pass every assertion above.
	t.Run("scalar_and_leaf_orderings_still_plan", func(t *testing.T) {
		t.Parallel()
		if err := drain("SELECT id FROM T_S ORDER BY id"); err != nil {
			t.Fatalf("ORDER BY on a scalar must still plan: %v", err)
		}
		if err := drain("SELECT id FROM T_S ORDER BY home.city"); err != nil {
			t.Fatalf("ORDER BY on a struct LEAF field must still plan: %v", err)
		}
	})
}
