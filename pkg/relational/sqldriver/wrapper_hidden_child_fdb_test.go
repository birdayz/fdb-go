package sqldriver_test

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"testing"

	"fdb.dev/pkg/relational/api"
)

// TestFDB_ArrayOfRecordLiteralsDescriptorOutcomes pins, as a TABLE, what an
// array literal of record constructors does today — including two shapes that
// FAIL outright.
//
// It is a table rather than a witness-and-control pair on purpose. THREE
// successive rounds each named a mechanism, built a control around it, and had
// the mechanism refuted by a row nobody had run — "a type-changing wrapper is
// enough", then "a wrapper over an unsynthesisable child is enough", then a
// three-condition account that the last four rows below break. A control is
// built by removing what you BELIEVE is the cause, so it inherits whatever the
// belief got wrong and passes either way; a table has no belief in it. The rows
// are the measurement and the prose is downstream of them, which is why this
// comment stops at what the rows show and does not generalise past it.
//
// What the rows show. Unifying two record shapes ANONYMISES the fields whose
// names disagree, and three sites then treat that anonymised target differently.
// Two of the three do not depend on the field name being one protobuf can carry
// at all — the controls that establish that are in the table, and each earlier
// account went wrong by not running them.
//
//   - ARRAY element. Here the bad name IS the variable. An element whose own
//     type protobuf cannot carry never stamps and evaluates to a name-keyed map.
//     If the anonymised target erased that name the parent stamps, is handed the
//     map, and REFUSES it — the query does not answer. If both elements carry
//     the SAME bad name the target keeps it, the parent cannot stamp either, and
//     the query ANSWERS as a uniform map with its values. And with nothing
//     stamped above the array at all, it answers RAGGED: one element a message,
//     one a map. That last is the worst of them, because nothing reports it.
//   - UNION. A loud 42F65 — but not about the name and not about records. It
//     fails IDENTICALLY for two synthesisable names, answers when the names
//     agree even with differing WIDTHS, and still fails when only ONE of two
//     fields disagrees. So what this site refuses is a target carrying an
//     ANONYMOUS field, and refusing it makes a legal SQL union unrunnable.
//   - CASE. With ONE field per branch it answers flattened: a bare leaf under
//     the outer alias, no nested record, the same for good names and bad. With
//     TWO fields a record DOES survive and IS coerced, the disagreeing field
//     arriving as `_1`. So this site coerces records where the array site does
//     not, which is a lead for the port rather than a curiosity — an earlier
//     round read the one-field row alone and concluded the opposite. Why a
//     single-field branch flattens at all is still unexplained.
//
// TestUnificationErasesAFieldNameOnlyWhenTheNamesDisagree pins the erasure and
// TestWhichRecordTypesCanBeGivenADescriptor the stamping predicate, both without
// Docker.
//
// Every outcome here reproduces identically at the merge-base `36b97f1e9`, so
// none is a regression of the work this ships with. TODO.md carries the
// closures. When one lands, its rows redden: assert the ROWS then.
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
		notPromotable = "is not promotable to"
	)

	for _, tc := range []struct {
		why string
		// elements of the array literal, spliced into SELECT ([…] AS CH) FROM t
		elems string
		// query overrides elems entirely, for the rows that vary the SITE the
		// record literal lands in rather than the literal itself. Those rows are
		// what showed the outcome is not a property of the literal at all.
		query string
		// wantRagged: the query ANSWERS with an array whose elements are of
		// MIXED representation — some stamped, some raw maps — which is neither
		// a failure nor a uniform degradation.
		wantRagged bool
		// exactly one of these three is set
		failsWith string
		// Per element: the field name, and the value it must still carry. Both
		// are written out per row rather than derived from the index, because a
		// derived expectation is how a wrong value passes unnoticed.
		wantFields []string
		wantVals   []float64
		wantStruct bool
		// For a wantStruct row: the leaf field names and numbers, in walk order.
		// Names matter as much as values — unification ANONYMISES disagreeing
		// fields, so a leaf still called `A` would mean the erasure this whole
		// account rests on did not happen.
		wantLeafNames []string
		wantLeafVals  []float64
		// wantRows defaults to 1. A UNION row legitimately returns two.
		wantRows int
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
			why:        "a promotion with synthesisable names and equal field types: everything stamps, and the leaves come back named `_0` — the anonymisation, visible end-to-end, which is the erasure the failing rows depend on",
			elems:      `(1 AS B), (2 AS A)`,
			wantStruct: true,
			// `_0` is what an ANONYMOUS field becomes in the synthesised
			// descriptor. Seeing the element's own `A`/`B` here instead would
			// mean unification kept the names and the account is wrong.
			wantLeafNames: []string{"_0", "_0"},
			wantLeafVals:  []float64{1, 2},
		},
		{
			why:           "neither factor: no promotion at all, so the agreeing name SURVIVES — the contrast that shows the row above is really measuring the erasure",
			elems:         `(1 AS A), (2 AS A)`,
			wantStruct:    true,
			wantLeafNames: []string{"A", "A"},
			wantLeafVals:  []float64{1, 2},
		},
		{
			why:       "SYNTHESISABLE names but differing numeric widths: the same site refusing a wrong-KIND message instead of a map, and the error text's synthesis prefix is ProtoTypeError's stock wording, not where it happened",
			elems:     `(1 AS A), (2.5 AS A)`,
			failsWith: widthMismatch,
		},
		{
			why:       "the same widths with DIFFERING names, so the target is anonymised as well: the width refusal does not depend on the names agreeing, which is what keeps it a separate axis from the erasure",
			elems:     `(1 AS A), (2.5 AS B)`,
			failsWith: widthMismatch,
		},
		{
			why:        "the SAME array, with no record wrapped around it: nothing above the array is stamped, so nothing refuses the map, and the array comes back RAGGED — one element a message, the other a map. It ANSWERS, which is worse than the failure above, and it is why the outcome is not a property of the literal",
			query:      `SELECT [(1 AS "$lead"), (2 AS A)] FROM t`,
			wantRagged: true,
		},
		{
			why:        "the same two record literals unified by a CASE rather than an array: ONE field per branch: it answers, but the leaf is a bare number under the outer alias and no record survives as a record. Read the two-field row below before concluding anything about coercion from this one",
			query:      `SELECT (CASE WHEN id=1 THEN (1 AS "$lead") ELSE (2 AS A) END AS CH) FROM t`,
			wantStruct: true,
			// One leaf, named for the OUTER alias: with a SINGLE field per branch
			// the record does not survive as a nested record, it arrives as the
			// value under `CH`. That flattening is unexplained, and it is why an
			// earlier round read this row as evidence that no record is ever
			// coerced here. The two-field row below shows one is.
			wantLeafNames: []string{"CH"},
			wantLeafVals:  []float64{1},
		},
		{
			why:       "the same two literals unified by a UNION: a LOUD refusal — but read the next two rows before attributing it to the bad name",
			query:     `SELECT (1 AS "$lead") AS C FROM t UNION ALL SELECT (2 AS A) AS C FROM t`,
			failsWith: notPromotable,
		},
		{
			why:       "the UNION control that matters: TWO SYNTHESISABLE names, disagreeing, and it fails IDENTICALLY. So this site has nothing to do with what protobuf can carry — it refuses to promote a record to the anonymised record unification produces, and that is a legal SQL union Go will not run",
			query:     `SELECT (1 AS A) AS C FROM t UNION ALL SELECT (2 AS B) AS C FROM t`,
			failsWith: notPromotable,
		},
		{
			why:           "and the same union with AGREEING names answers, BOTH legs, so the refusal is the anonymisation and not unions of records in general",
			query:         `SELECT (1 AS A) AS C FROM t UNION ALL SELECT (2 AS A) AS C FROM t`,
			wantStruct:    true,
			wantRows:      2,
			wantLeafNames: []string{"A", "A"},
			wantLeafVals:  []float64{1, 2},
		},
		{
			why:           "the CASE control: SYNTHESISABLE disagreeing names give the same one-field result as the bad-name CASE row above, so nothing at this site varies with the name",
			query:         `SELECT (CASE WHEN id=1 THEN (1 AS A) ELSE (2 AS B) END AS CH) FROM t`,
			wantStruct:    true,
			wantLeafNames: []string{"CH"},
			wantLeafVals:  []float64{1},
		},
		{
			why:           "TWO fields per CASE branch, and now a record DOES survive as a record and IS coerced — the disagreeing field comes back `_1`, anonymised exactly as unification does. So the CASE site coerces records; the rows above are flattened only because a single-field branch is. Something on this path does what the array path does not, and finding out what is the first step of the port",
			query:         `SELECT (CASE WHEN id=1 THEN (1 AS A, 3 AS Z) ELSE (2 AS A, 4 AS Y) END AS CH) FROM t`,
			wantStruct:    true,
			wantLeafNames: []string{"A", "_1"},
			wantLeafVals:  []float64{1, 3},
		},
		{
			why:           "a UNION whose names AGREE but whose widths differ: it ANSWERS, both legs. So record promotion in a union is not refused in general — only a target carrying an ANONYMOUS field is",
			query:         `SELECT (1 AS A) AS C FROM t UNION ALL SELECT (2.5 AS A) AS C FROM t`,
			wantStruct:    true,
			wantRows:      2,
			wantLeafNames: []string{"A", "A"},
			wantLeafVals:  []float64{1, 2.5},
		},
		{
			why:       "PARTIAL anonymisation in a union: `A` agrees and `Z`/`Y` do not, so the target is `RECORD<A INT, INT>` and it is still refused. One anonymous field is enough, which is what makes the refusal about the anonymous field rather than about the record",
			query:     `SELECT (1 AS A, 3 AS Z) AS C FROM t UNION ALL SELECT (2 AS A, 4 AS Y) AS C FROM t`,
			failsWith: notPromotable,
		},
	} {
		query := tc.query
		if query == "" {
			query = fmt.Sprintf(`SELECT ([%s] AS CH) FROM t`, tc.elems)
		}
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
		wantRows := tc.wantRows
		if wantRows == 0 {
			wantRows = 1
		}
		if len(got) != wantRows {
			t.Errorf("%s returned %d rows, want exactly %d", query, len(got), wantRows)
			continue
		}
		if tc.wantRagged {
			elems, isSlice := got[0].([]any)
			if !isSlice || len(elems) != 2 {
				t.Errorf("%s = %#v, want a two-element array. %s", query, got[0], tc.why)
				continue
			}
			_, firstIsMap := elems[0].(map[string]any)
			_, secondIsMap := elems[1].(map[string]any)
			if firstIsMap == secondIsMap {
				t.Errorf("%s returned an array whose elements are both %s (%T, %T), want them "+
					"MIXED. %s — if they agree now, this shape either answers uniformly or fails, "+
					"and either way the ragged outcome this row exists to record is gone",
					query, map[bool]string{true: "raw maps", false: "stamped"}[firstIsMap],
					elems[0], elems[1], tc.why)
			}
			continue
		}
		if tc.wantStruct {
			if _, isStruct := got[0].(api.Struct); !isStruct {
				t.Errorf("%s = %T, want an api.Struct. %s — a map or any other carrier here would "+
					"mean the bake stopped stamping this shape", query, got[0], tc.why)
				continue
			}
			// The outer carrier is not enough: a struct with an EMPTY `CH`, or
			// with the element's original field name where unification should
			// have anonymised it, is still an api.Struct. Walk to the leaves and
			// assert the names and the numbers.
			if len(tc.wantLeafNames) == 0 {
				t.Errorf("%s is a wantStruct row with no leaf expectation, so the assertion below "+
					"passes on anything: declare wantLeafNames/wantLeafVals or drop the row", query)
				continue
			}
			// Every row, not just the first: a multi-row expectation that
			// examines one row leaves the rest unmeasured. Sorted by value,
			// because UNION ALL leg order is not pinned here and asserting it
			// would make this row about something else.
			var names, others []string
			var vals []float64
			for _, row := range got {
				rowNames, rowVals, rowOthers := leaves(t, row)
				names = append(names, rowNames...)
				vals = append(vals, rowVals...)
				others = append(others, rowOthers...)
			}
			sortLeavesByValue(names, vals)
			if len(others) != 0 {
				t.Errorf("%s reached %d non-numeric leaf/leaves %q: this row asserts numbers, and a "+
					"leaf the walker cannot read would otherwise be invisible", query, len(others), others)
			}
			if !leafNamesEqual(names, tc.wantLeafNames) || !leafValsEqual(vals, tc.wantLeafVals) {
				t.Errorf("%s leaves = %q/%v, want %q/%v. %s", query, names, vals,
					tc.wantLeafNames, tc.wantLeafVals, tc.why)
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

// leaves walks a driver value to its numeric leaves, collecting each leaf's
// FIELD NAME beside its value in walk order.
//
// The names are the point as much as the values: unification anonymises fields
// whose names disagree, so a leaf still carrying the element's original name is
// evidence the erasure this account rests on did not happen. Structs, arrays and
// raw maps are all walked, because which of the three a row produces is exactly
// what the table is measuring — a walker that handled only one would decide the
// answer before looking.
func leaves(t *testing.T, v any) (names []string, vals []float64, others []string) {
	t.Helper()
	var walk func(name string, node any)
	walk = func(name string, node any) {
		switch n := node.(type) {
		case api.Struct:
			md := n.MetaData()
			for i := 1; i <= n.AttributeCount(); i++ {
				attr, err := n.Attribute(i)
				if err != nil {
					t.Fatalf("attribute %d: %v", i, err)
				}
				field, err := md.AttributeName(i)
				if err != nil {
					t.Fatalf("attribute %d name: %v", i, err)
				}
				walk(field, attr)
			}
		case []any:
			for _, elem := range n {
				walk(name, elem)
			}
		case map[string]any:
			// Sorted so the walk order is the map's CONTENT, not Go's
			// randomised iteration — otherwise this helper would flake.
			keys := make([]string, 0, len(n))
			for k := range n {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				walk(k, n[k])
			}
		default:
			if f, isNumber := asFloat(node); isNumber {
				names = append(names, name)
				vals = append(vals, f)
				return
			}
			// NOT dropped. A leaf this walker cannot read — a NULL, a string, a
			// carrier nobody anticipated — would otherwise be invisible to every
			// assertion built on it, which is the failure this whole file is about.
			others = append(others, fmt.Sprintf("%s=%T(%v)", name, node, node))
		}
	}
	walk("", v)
	return names, vals, others
}

func leafNamesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func leafValsEqual(a, b []float64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// sortLeavesByValue orders the collected leaves by value, keeping each name with
// its number.
//
// Multi-row rows in this table are UNION ALL, whose leg order is not pinned
// anywhere. Asserting the order here would quietly make those rows a test of leg
// ordering, and they would then redden for a reason that has nothing to do with
// what they measure.
func sortLeavesByValue(names []string, vals []float64) {
	// Sorted as PAIRS. Sorting the two slices independently would reorder names
	// and values against each other, and every assertion built on them would
	// then be about a pairing the query never produced.
	sort.Stable(sortableLeaves{names: names, vals: vals})
}

type sortableLeaves struct {
	names []string
	vals  []float64
}

func (s sortableLeaves) Len() int { return len(s.vals) }
func (s sortableLeaves) Swap(i, j int) {
	s.names[i], s.names[j] = s.names[j], s.names[i]
	s.vals[i], s.vals[j] = s.vals[j], s.vals[i]
}

func (s sortableLeaves) Less(i, j int) bool {
	if s.vals[i] != s.vals[j] {
		return s.vals[i] < s.vals[j]
	}
	return s.names[i] < s.names[j]
}
