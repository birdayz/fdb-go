package sqldriver_test

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"

	"fdb.dev/pkg/relational/api"
)

// TestFDB_ArrayOfRecordLiteralsDescriptorOutcomes pins, as a TABLE, what an
// array literal of record constructors does today — including two shapes that
// FAIL outright.
//
// It is a table rather than a witness-and-control pair on purpose. Two earlier
// rounds each named a mechanism, built a control around it, and had the
// mechanism refuted by a row nobody had run: first "a type-changing wrapper is
// enough" (it is not — with synthesisable names and equal field types the query
// answers), then "a wrapper over an unsynthesisable child is enough" (also not —
// when BOTH elements carry the SAME bad name the promotion target keeps it, so
// the parent fails to synthesise as well and everything degrades together). A
// control is built by removing what you BELIEVE is the cause, so it inherits
// whatever the belief got wrong and passes either way. The rows below are the
// measurement; the prose is downstream of them.
//
// What the rows show, structurally:
//
//   - A record constructor whose own type cannot be synthesised evaluates to a
//     name-keyed map. A STAMPED parent builds a protobuf message and cannot
//     store a map in a message field, so the query fails; when the parent is
//     unstamped too, everything degrades to maps together and the query ANSWERS
//     in a weaker type. Which of the two happens turns on whether the promotion
//     target is itself synthesisable, because the target is what the parent's
//     type carries.
//   - Unifying elements of differing shape ANONYMISES the fields (`_0`), which
//     ERASES an offending name — so two DIFFERENT bad names fail, while the
//     SAME bad name twice survives into the target and answers.
//   - Separately, unifying differing numeric widths fails descriptor synthesis
//     outright, with a different error and no map involved at all.
//
// Both failures reproduce identically at the merge-base `36b97f1e9`, so neither
// is a regression of the work this ships with. TODO.md carries their closures:
// "A stamped record constructor over a wrapper-hidden child fails the query" and
// "Unifying two record literals of differing numeric width fails to synthesise".
// When either lands, its rows redden: assert the ROWS then.
func TestFDB_ArrayOfRecordLiteralsDescriptorOutcomes(t *testing.T) {
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
	// and every row below succeeds vacuously.
	mwjoMustExec(t, db, ctx, "INSERT INTO t VALUES (1)")

	const (
		mapInMessage  = "cannot store map[string]interface {} in message field"
		widthMismatch = "but double in the target"
	)

	for _, tc := range []struct {
		why string
		// elements of the array literal, spliced into SELECT ([…] AS CH) FROM t
		elems string
		// exactly one of these three is set
		failsWith string
		// Per element: the field name, and the value it must still carry. Both
		// are written out per row rather than derived from the index, because a
		// derived expectation is how a wrong value passes unnoticed.
		wantFields []string
		wantVals   []float64
		wantStruct bool
	}{
		{
			why:       "one unsynthesisable name beside a good one: differing shapes anonymise the target, so the target IS synthesisable, the parent stamps, and the child's map is refused",
			elems:     `(1 AS "$lead"), (2 AS A)`,
			failsWith: mapInMessage,
		},
		{
			why:       "the offending element second, so the failure is not about position",
			elems:     `(1 AS A), (2 AS "$lead")`,
			failsWith: mapInMessage,
		},
		{
			why:       "TWO different unsynthesisable names: the target anonymises both, so it is still synthesisable and the parent still stamps alone",
			elems:     `(1 AS "$lead"), (2 AS "$tail")`,
			failsWith: mapInMessage,
		},
		{
			why:       "a leading digit, not a dollar sign: the rule is what protobuf will carry, not one prefix",
			elems:     `(1 AS "1x"), (2 AS A)`,
			failsWith: mapInMessage,
		},
		{
			why:        "the SAME unsynthesisable name twice with differing value types: a promotion IS inserted, but the target keeps the bad name, so the parent cannot synthesise either and the whole thing degrades together",
			elems:      `(1 AS "$lead"), (2.5 AS "$lead")`,
			wantFields: []string{"$lead", "$lead"},
			wantVals:   []float64{1, 2.5},
		},
		{
			why:        "the same unsynthesisable name twice with equal types: no promotion at all, and the ordinary documented cost — a weaker type, values intact",
			elems:      `(1 AS "$lead"), (2 AS "$lead")`,
			wantFields: []string{"$lead", "$lead"},
			wantVals:   []float64{1, 2},
		},
		{
			why:        "a promotion with synthesisable names and equal field types: everything stamps",
			elems:      `(1 AS B), (2 AS A)`,
			wantStruct: true,
		},
		{
			why:        "neither factor: no promotion, synthesisable names",
			elems:      `(1 AS A), (2 AS A)`,
			wantStruct: true,
		},
		{
			why:       "SYNTHESISABLE names but differing numeric widths: a separate defect, a different error, and no map anywhere in it",
			elems:     `(1 AS A), (2.5 AS A)`,
			failsWith: widthMismatch,
		},
	} {
		query := fmt.Sprintf(`SELECT ([%s] AS CH) FROM t`, tc.elems)
		rows, qErr := db.QueryContext(ctx, query)
		if tc.failsWith != "" {
			if qErr == nil {
				rows.Close()
				t.Errorf("%s answered, want the failure this row pins (%s). %s. If the closure "+
					"landed, assert the rows here instead", query, tc.failsWith, tc.why)
				continue
			}
			if !strings.Contains(qErr.Error(), tc.failsWith) {
				t.Errorf("%s failed with %v, want a failure containing %q: a different error means "+
					"this shape is refused somewhere else now and the row no longer describes it",
					query, qErr, tc.failsWith)
			}
			continue
		}
		if qErr != nil {
			t.Errorf("%s: %v — this row must ANSWER. %s", query, qErr, tc.why)
			continue
		}
		var got []any
		for rows.Next() {
			var col any
			if scanErr := rows.Scan(&col); scanErr != nil {
				t.Errorf("%s: scan: %v", query, scanErr)
				break
			}
			got = append(got, col)
		}
		if rErr := rows.Err(); rErr != nil {
			t.Errorf("%s: rows.Err after %d row(s): %v", query, len(got), rErr)
		}
		rows.Close()
		if len(got) != 1 {
			t.Errorf("%s returned %d rows, want exactly 1", query, len(got))
			continue
		}
		if tc.wantStruct {
			if _, isStruct := got[0].(api.Struct); !isStruct {
				t.Errorf("%s = %T, want an api.Struct. %s — a map or any other carrier here would "+
					"mean the bake stopped stamping this shape", query, got[0], tc.why)
			}
			continue
		}
		asMap, isMap := got[0].(map[string]any)
		if !isMap {
			t.Errorf("%s = %T, want the raw map an unstamped constructor forces. %s",
				query, got[0], tc.why)
			continue
		}
		elems, isSlice := asMap["CH"].([]any)
		if !isSlice || len(elems) != len(tc.wantFields) {
			t.Errorf("%s CH = %#v, want %d element(s): the VALUES must survive the lost type, "+
				"which is the whole difference between this row and the failing ones",
				query, asMap["CH"], len(tc.wantFields))
			continue
		}
		for i, field := range tc.wantFields {
			elem, elemIsMap := elems[i].(map[string]any)
			if !elemIsMap {
				t.Errorf("%s CH[%d] = %T, want a map", query, i, elems[i])
				continue
			}
			// Compared NUMERICALLY: the driver's integer width is not what this
			// row is about, but a value arriving as a STRING would be a
			// different defect and must not pass as "values intact".
			n, isNumber := asFloat(elem[field])
			if !isNumber || n != tc.wantVals[i] {
				t.Errorf("%s CH[%d][%q] = %#v, want the number %v: the type identity is lost, "+
					"the values are not", query, i, field, elem[field], tc.wantVals[i])
			}
		}
	}
}

// asFloat accepts any of the integer widths a driver may hand back and refuses
// anything that is not a number, so "values intact" cannot be satisfied by a
// string that happens to print the same.
func asFloat(v any) (float64, bool) {
	switch n := v.(type) {
	case int:
		return float64(n), true
	case int32:
		return float64(n), true
	case int64:
		return float64(n), true
	case float32:
		return float64(n), true
	case float64:
		return n, true
	default:
		return 0, false
	}
}
