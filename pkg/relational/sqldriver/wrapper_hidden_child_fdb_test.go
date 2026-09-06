package sqldriver_test

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
)

// TestFDB_AWrapperHiddenRecordConstructorFailsTheQuery pins a query that FAILS
// today, and the mechanism that makes it fail.
//
// The plan-time bake stamps each record constructor with a protobuf descriptor
// synthesised from its own inferred type. A stamped parent then builds a
// MESSAGE, and expects each record-typed field to hand it a message too. An
// unstamped constructor hands back a name-keyed map instead, which cannot be
// stored in a message field — so where the ordinary cost of an unstamped
// constructor is a weaker TYPE, the cost of a stamped parent over an unstamped
// child is a query that does not answer at all.
//
// That pair was believed unreachable, on the argument that a parent's type
// CONTAINS its children's, so synthesising the parent registers the child's
// message and the child's own lookup then succeeds. The argument has a hole: a
// type-changing WRAPPER between the two. Array unification inserts a promotion
// with an anonymous target type, and the parent's type then contains the
// TARGET's shape rather than the wrapped constructor's, so the child is not
// registered by its parent's synthesis and is left unstamped beside a stamped
// parent.
//
// Measured at the merge-base too, with the same error, so this is not a
// regression of the work this test ships with — it is a pre-existing defect
// that the reachability argument above was wrong about. TODO.md, "A stamped
// record constructor over a wrapper-hidden child fails the query", carries the
// closure. When it lands this test reddens: assert the ROWS then, not the error.
func TestFDB_AWrapperHiddenRecordConstructorFailsTheQuery(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_wraphidden")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_wraphidden")
	mwjoMustExec(t, setup, ctx, `CREATE SCHEMA TEMPLATE wraphidden_tpl
		CREATE TABLE t (id BIGINT, PRIMARY KEY (id))`)
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_wraphidden/s1 WITH TEMPLATE wraphidden_tpl")
	db, err := sql.Open("fdbsql", fmt.Sprintf("fdbsql:///testdb_wraphidden?cluster_file=%s&schema=s1", clusterFilePath))
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	// A row must exist: with an empty table the projection is never evaluated
	// and the query succeeds, which would make this test pass for the wrong
	// reason.
	mwjoMustExec(t, db, ctx, "INSERT INTO t VALUES (1)")

	// The two array elements have DIFFERENT record shapes, which is what makes
	// unification insert a promotion over each. With one element, or two of the
	// same shape, no wrapper appears and the query answers.
	const wrapperHidden = `SELECT ([(1 AS "$lead"), (2 AS A)] AS CH) FROM t`
	rows, err := db.QueryContext(ctx, wrapperHidden)
	if err == nil {
		defer rows.Close()
		var got []any
		for rows.Next() {
			var col any
			if scanErr := rows.Scan(&col); scanErr != nil {
				t.Fatalf("%s: scan: %v", wrapperHidden, scanErr)
			}
			got = append(got, col)
		}
		t.Fatalf("%s answered with %#v (rows.Err=%v) — the wrapper-hidden constructor is stamped "+
			"now, so TODO.md's booking has closed: assert the ROWS here instead of the failure",
			wrapperHidden, got, rows.Err())
	}
	if !strings.Contains(err.Error(), "cannot store") || !strings.Contains(err.Error(), "in message field") {
		t.Fatalf("%s failed with %v, want the cannot-store-in-message-field refusal: a different "+
			"error means the query is being rejected somewhere else now and this pin no longer "+
			"describes the defect it names", wrapperHidden, err)
	}

	// The counterweight, varying ONE thing: two elements again, so the array's
	// arity and its record-ness are held fixed, and only the SHAPE difference that
	// forces the promotion is removed. A single-element control would have varied
	// element count too, and would leave "two-element record arrays fail" as an
	// unexcluded explanation for the failure above.
	const noWrapper = `SELECT ([(1 AS A), (2 AS A)] AS CH) FROM t`
	control, err := db.QueryContext(ctx, noWrapper)
	if err != nil {
		t.Fatalf("%s: %v — two records of the SAME shape need no promotion and must still "+
			"answer, or the failure above is not attributable to the shape difference and could "+
			"just as well be about arrays of records", noWrapper, err)
	}
	defer control.Close()
	if !control.Next() {
		t.Fatalf("%s returned no row (err=%v), want one", noWrapper, control.Err())
	}
}
