package sqldriver_test

// `WHERE TRUE AND TRUE` is a tautology reached through a CONJUNCTION, and it
// indexes every record exactly as bare `WHERE TRUE` does.
//
// Java never has to classify that shape, because it never constructs it. An
// index predicate becomes a QueryPredicate through IndexPredicate.AndPredicate
// .toPredicate (IndexPredicate.java:344-345), which delegates to
// AndPredicate.and — and that CONSTRUCTOR drops every tautological conjunct and
// returns ConstantPredicate.TRUE when none remain (AndPredicate.java:188-206).
// A conjunction of tautologies therefore never exists as an AndPredicate in
// Java; it has already collapsed to the constant before anything asks whether
// it is filtering.
//
// So the completeness proof must NORMALIZE the way Java's constructor does and
// then apply the same narrow ConstantPredicate.TRUE test, rather than grow a
// classifier that reasons about And nodes Java would never have built.

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"testing"
)

func TestFDB_ConjunctiveTautologyIndexPredicateExecutes(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_taut_and")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_taut_and")
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE taut_and "+
			"CREATE TABLE t1 (id BIGINT, col1 BIGINT, PRIMARY KEY (id)) "+
			"CREATE INDEX i_and AS SELECT col1 FROM t1 WHERE TRUE AND TRUE")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_taut_and/s WITH TEMPLATE taut_and")
	dsn := fmt.Sprintf("fdbsql:///testdb_taut_and?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	mwjoMustExec(t, db, ctx,
		"INSERT INTO t1 (id, col1) VALUES (1, 10), (2, 20), (3, 30), (4, 200)")

	const q = "SELECT col1 FROM t1 WHERE col1 < 100"

	// Both halves matter and they fail separately. The conversion half decides
	// whether a candidate is built for the index at all; the executor half
	// decides whether the resulting plan may run. A fix to only one leaves the
	// other silently broken, so the plan assertion and the row assertion are
	// both load-bearing.
	var plan string
	if err := db.QueryRowContext(ctx, "EXPLAIN "+q).Scan(&plan); err != nil {
		t.Fatalf("EXPLAIN %q: %v", q, err)
	}
	if !strings.Contains(strings.ToUpper(plan), "I_AND") {
		t.Fatalf("plan does not use the conjunctive-tautology index I_AND:\n%s\n"+
			"— TRUE AND TRUE rejects no record, so the candidate conversion must "+
			"collapse it to the constant the way AndPredicate.and does and leave the "+
			"index as matchable as any full value index", plan)
	}

	rows, err := db.QueryContext(ctx, q)
	if err != nil {
		t.Fatalf("query %q over a WHERE TRUE AND TRUE index: %v\n"+
			"— a conjunction of tautologies rejects no record, so the index is "+
			"complete and the executor's filtered-index backstop must admit it", q, err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var v int64
		if err := rows.Scan(&v); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got = append(got, fmt.Sprintf("%d", v))
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows.Err: %v", err)
	}
	sort.Strings(got)
	if want := "10 20 30"; strings.Join(got, " ") != want {
		t.Fatalf("rows = %q, want %q", strings.Join(got, " "), want)
	}
}
