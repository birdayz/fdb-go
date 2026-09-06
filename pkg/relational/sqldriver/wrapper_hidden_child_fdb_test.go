package sqldriver_test

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
)

// TestFDB_AWrapperOverAnUnsynthesisableRecordFailsTheQuery pins a query that
// FAILS today, and — by measuring the whole 2x2 — which two things have to be
// true at once for it to fail.
//
// The bake stamps each record constructor with a descriptor synthesised from its
// own type. A stamped constructor builds a protobuf MESSAGE and expects every
// record-typed field to hand it a message too; an unstamped one evaluates to a
// name-keyed map, which cannot be stored in a message field.
//
// So the failure is a CONJUNCTION, and each half alone is harmless:
//
//   - A field name protobuf cannot carry. `$lead` cannot start a protobuf field
//     name, so that constructor's own descriptor never synthesises and it stays
//     unstamped. ALONE this is the ordinary documented cost: the parent's type
//     contains the child's, so the parent fails to synthesise too, everything
//     degrades to maps together, and the query ANSWERS in a weaker type.
//   - A type-changing WRAPPER between parent and child. Array unification
//     promotes elements of differing record shape to a common anonymous target,
//     so the parent's type carries the TARGET's shape rather than the
//     constructor underneath. ALONE this is also harmless: with synthesisable
//     names everything stamps and the answer is a struct.
//
// Together the wrapper hides the unsynthesisable child from the parent's type,
// the parent stamps, the child does not, and the query does not answer at all.
//
// Measured at the merge-base too, with the same error, so this is not a
// regression of the work it ships with. TODO.md, "A stamped record constructor
// over a wrapper-hidden child fails the query", carries the closure. When it
// lands the failing arm reddens: assert its ROWS then.
func TestFDB_AWrapperOverAnUnsynthesisableRecordFailsTheQuery(t *testing.T) {
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
	// and every arm below succeeds vacuously.
	mwjoMustExec(t, db, ctx, "INSERT INTO t VALUES (1)")

	// answerOneRow requires exactly one row, scans it, and checks the terminal
	// error — because "Next() returned true once" also holds for a query that
	// then fails mid-stream or returns the wrong value.
	answerOneRow := func(t *testing.T, query string) any {
		t.Helper()
		rows, qErr := db.QueryContext(ctx, query)
		if qErr != nil {
			t.Fatalf("%s: %v", query, qErr)
		}
		defer rows.Close()
		var got []any
		for rows.Next() {
			var col any
			if scanErr := rows.Scan(&col); scanErr != nil {
				t.Fatalf("%s: scan: %v", query, scanErr)
			}
			got = append(got, col)
		}
		if rErr := rows.Err(); rErr != nil {
			t.Fatalf("%s: rows.Err after %d row(s): %v", query, len(got), rErr)
		}
		if len(got) != 1 {
			t.Fatalf("%s returned %d rows, want exactly 1", query, len(got))
		}
		return got[0]
	}

	// BOTH halves: the failing shape.
	const bothHalves = `SELECT ([(1 AS "$lead"), (2 AS A)] AS CH) FROM t`
	rows, err := db.QueryContext(ctx, bothHalves)
	if err == nil {
		rows.Close()
		t.Fatalf("%s answered — the wrapper-hidden constructor is stamped now, so TODO.md's "+
			"booking has closed: assert the ROWS here instead of the failure", bothHalves)
	}
	if !strings.Contains(err.Error(), "cannot store") || !strings.Contains(err.Error(), "in message field") {
		t.Fatalf("%s failed with %v, want the cannot-store-in-message-field refusal: a different "+
			"error means the query is rejected somewhere else now and this pin no longer describes "+
			"the defect it names", bothHalves, err)
	}

	// WRAPPER ONLY. Two DIFFERENT record shapes, so the promotion is still
	// inserted, with names protobuf can carry. If this ever fails, the wrapper
	// alone became sufficient and the conjunction above is not the mechanism.
	const wrapperOnly = `SELECT ([(1 AS B), (2 AS A)] AS CH) FROM t`
	if got := answerOneRow(t, wrapperOnly); got == nil {
		t.Fatalf("%s returned a nil value, want a struct: the wrapper alone must stay harmless "+
			"or the failure above is not attributable to the unsynthesisable name", wrapperOnly)
	} else if _, isMap := got.(map[string]any); isMap {
		t.Fatalf("%s = %#v, want a struct: with synthesisable names the promotion still stamps, "+
			"and a raw map here would mean the wrapper alone defeats the bake", wrapperOnly, got)
	}

	// NAME ONLY. The same unsynthesisable name in BOTH elements, so the shapes
	// agree and no promotion is inserted. This is the ordinary documented cost —
	// a raw map, values intact, query answers — and it is what makes the failure
	// above attributable to the CONJUNCTION rather than to the name.
	const nameOnly = `SELECT ([(1 AS "$lead"), (2 AS "$lead")] AS CH) FROM t`
	got := answerOneRow(t, nameOnly)
	asMap, isMap := got.(map[string]any)
	if !isMap {
		t.Fatalf("%s = %#v, want the raw map an unstamped constructor forces: if this is a struct "+
			"now, `$lead` has become synthesisable and neither half of the conjunction holds",
			nameOnly, got)
	}
	elems, isSlice := asMap["CH"].([]any)
	if !isSlice || len(elems) != 2 {
		t.Fatalf("%s CH = %#v, want a two-element array: the values must survive the lost type, "+
			"which is the whole difference between this arm and the failing one", nameOnly, asMap["CH"])
	}
	for i, want := range []string{"1", "2"} {
		elem, elemIsMap := elems[i].(map[string]any)
		// Compared as text: the driver's integer width is not what this arm is
		// about, and pinning it here would redden on an unrelated widening.
		if !elemIsMap || fmt.Sprint(elem["$lead"]) != want {
			t.Fatalf("%s CH[%d] = %#v, want {$lead:%s}: the type is lost, the VALUES are not",
				nameOnly, i, elems[i], want)
		}
	}
}
