package sqldriver_test

// An ARRAY column can BE the IN list: `b IN xs`, with no brackets.
//
// That is a distinct alternative of the grammar's inList rule, and the brackets
// are what tell it apart from everything else:
//
//	b IN (10, 20)   expressions      a value list
//	b IN (xs)       expressions      a ONE-ITEM list whose item happens to be
//	                                 an array — a whole-value comparison, not a
//	                                 membership test over the array's elements
//	b IN xs         fullColumnName   the array IS the list
//
// Java resolves the third by a TYPE test: an ARRAY column is accepted and
// anything else is refused with "IN list with column reference must be of array
// type, but got: %s" (ExpressionVisitor.java:641-643). Go refused all three
// column spellings with one blanket "IN with a column reference is not
// supported", and the comment justifying that said Java rejects the syntax —
// a claim measurement contradicted (conformance/in_list_shapes_java_probe_test.go:
// Java answers `b IN xs`).
//
// The bracketed middle row is pinned here as hard as the working one, because
// it is the shape most likely to be "fixed" by someone reading this file
// quickly: `b IN (xs)` must NOT quietly become a membership test. It is a
// comparison against the array as a single value, and Go refuses it for the
// same reason it refuses any record/composite comparand.

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
)

func openArrayInDB(t *testing.T) *sql.DB {
	t.Helper()
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_in_arraycol")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_in_arraycol")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA TEMPLATE inarr_t "+
		"CREATE TABLE t (id BIGINT, b BIGINT, s STRING, xs BIGINT ARRAY, ss STRING ARRAY, "+
		"PRIMARY KEY (id))")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_in_arraycol/s WITH TEMPLATE inarr_t")
	db, err := sql.Open("fdbsql",
		fmt.Sprintf("fdbsql:///testdb_in_arraycol?cluster_file=%s&schema=s", clusterFilePath))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// openArrayUUIDDB pairs a UUID column with a STRING ARRAY, the shape where the
// probe's type and the element type differ by a CONVERSION rather than by a
// promotion cmpAny can do at runtime.
func openArrayUUIDDB(t *testing.T) *sql.DB {
	t.Helper()
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_in_arruuid")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_in_arruuid")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA TEMPLATE inarruuid_t "+
		"CREATE TABLE u (id BIGINT, uu UUID, s STRING, us STRING ARRAY, PRIMARY KEY (id))")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_in_arruuid/s WITH TEMPLATE inarruuid_t")
	db, err := sql.Open("fdbsql",
		fmt.Sprintf("fdbsql:///testdb_in_arruuid?cluster_file=%s&schema=s", clusterFilePath))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestFDB_InListIsAnArrayColumn(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	db := openArrayInDB(t)
	//  id=1: b=10  xs=[10,20]  b IS in xs          s='x'  ss=['x','y']  in ss
	//  id=2: b=20  xs=[30]     b is NOT in xs      s='q'  ss=['z']      not in ss
	//  id=3: b=30  xs=[30,40]  b IS in xs (first)  s='a'  ss=[]         empty
	//  id=4: b=40  xs=[]       EMPTY array         s='a'  ss=['a']      in ss
	//  id=5: b=50  xs=[50]     single element      s='b'  ss=['b']      in ss
	//
	// There are deliberately NO NULL elements here: an array literal carrying
	// one is rejected outright, which the arm at the end of this file pins.
	mwjoMustExec(t, db, ctx, "INSERT INTO t (id, b, s, xs, ss) VALUES "+
		"(1, 10, 'x', [10,20], ['x','y']), "+
		"(2, 20, 'q', [30], ['z']), "+
		"(3, 30, 'a', [30,40], []), "+
		"(4, 40, 'a', [], ['a']), "+
		"(5, 50, 'b', [50], ['b'])")

	t.Run("membership over the array's elements", func(t *testing.T) {
		cases := []struct {
			name, pred string
			want       []string
		}{
			{"bigint column in a bigint array", "b IN xs", []string{"1", "3", "5"}},
			{"string column in a string array", "s IN ss", []string{"1", "4", "5"}},
			// A LITERAL against the array column — the left operand need not be
			// a column for the branch to apply.
			{"literal in an array column", "30 IN xs", []string{"2", "3"}},
			// An expression on the left, which must be evaluated per row like
			// any other left operand.
			{"expression in an array column", "b + 0 IN xs", []string{"1", "3", "5"}},
			// The left operand parenthesized — composes with the one-field
			// record flatten.
			{"parenthesized left operand", "(b) IN xs", []string{"1", "3", "5"}},
		}
		for _, c := range cases {
			got, err := mmRows(t, ctx, db,
				fmt.Sprintf("SELECT id FROM t WHERE %s ORDER BY id", c.pred))
			if err != nil {
				t.Errorf("%s: %v\n  pred: %s", c.name, err, c.pred)
				continue
			}
			if !mmEqRows(got, c.want) {
				t.Errorf("%s\n  pred: %s\n  got  %v\n  want %v\n  %s",
					c.name, c.pred, got, c.want, mmFirstDiff(got, c.want))
			}
		}
	})

	// IN and NOT IN must PARTITION the rows here, and that they do is a fact
	// about this fixture rather than about membership in general.
	//
	// SQL membership is three-valued: an array holding a NULL element makes
	// non-membership UNKNOWN rather than TRUE, so a row whose value is absent
	// would appear in NEITHER answer. No element here is NULL, so every row's
	// membership is TRUE or FALSE and the two answers must cover all five rows
	// exactly once. The partition is asserted as well as the two lists, because
	// a defect that dropped a row from BOTH would otherwise need someone to
	// notice a missing id.
	t.Run("IN and NOT IN partition the rows when no element is NULL", func(t *testing.T) {
		in, err := mmRows(t, ctx, db, "SELECT id FROM t WHERE b IN xs ORDER BY id")
		if err != nil {
			t.Fatalf("IN: %v", err)
		}
		notIn, err := mmRows(t, ctx, db, "SELECT id FROM t WHERE b NOT IN xs ORDER BY id")
		if err != nil {
			t.Fatalf("NOT IN: %v", err)
		}
		if !mmEqRows(in, []string{"1", "3", "5"}) {
			t.Errorf("IN over an array column\n  got  %v\n  want [1 3 5]", in)
		}
		// id=4's array is EMPTY, and membership in an empty set is FALSE, not
		// UNKNOWN — so it belongs to the negation. An empty array confused with
		// a NULL one would drop it from both sides and break the partition.
		if !mmEqRows(notIn, []string{"2", "4"}) {
			t.Errorf("NOT IN over an array column\n  got  %v\n  want [2 4]\n"+
				"  (missing id=4 means an EMPTY array was treated as UNKNOWN rather than as "+
				"a set with no members)", notIn)
		}
		for _, id := range in {
			if slicesContains(notIn, id) {
				t.Errorf("id=%s is in BOTH IN and NOT IN, which no truth value allows", id)
			}
		}
		if len(in)+len(notIn) != 5 {
			t.Errorf("IN (%v) and NOT IN (%v) cover %d of 5 rows. With no NULL element in any "+
				"array every membership is TRUE or FALSE, so a row in neither answer is a "+
				"membership that evaluated to UNKNOWN when it could not",
				in, notIn, len(in)+len(notIn))
		}
	})

	// WHY THE THREE-VALUED CASE IS NOT TESTED ABOVE, pinned rather than left as
	// an absence a reader has to rediscover.
	//
	// The interesting 3VL shape — an array holding a NULL element, where
	// non-membership is UNKNOWN and NOT IN must NOT return the row — cannot be
	// built here at all: an array literal carrying a NULL is rejected outright.
	// The hazard is therefore unreachable through this door, and that is a
	// property of the LITERAL rather than of the membership test.
	//
	// This arm makes the fact checkable. If array literals ever admit NULL
	// elements it fails and says that the 3VL arm now needs writing — which is
	// exactly the moment the hazard becomes reachable and nobody would
	// otherwise be looking for it.
	t.Run("a NULL array element is rejected, which is what makes 3VL unreachable here",
		func(t *testing.T) {
			_, err := db.ExecContext(ctx,
				"INSERT INTO t (id, b, s, xs, ss) VALUES (99, 99, 'z', [null, 99], ['z'])")
			if err == nil {
				t.Fatal("an array literal with a NULL element was ACCEPTED. Three-valued " +
					"membership is now reachable: add an arm asserting that `b NOT IN xs` does " +
					"NOT return a row whose array holds a NULL, because non-membership there " +
					"is UNKNOWN rather than TRUE")
			}
			if !strings.Contains(err.Error(), "NULL as elements of a collection") {
				t.Errorf("the array-literal NULL rejection changed shape: %v\n"+
					"  (this arm reads that rejection as the reason 3VL membership is "+
					"untestable here, so a different refusal needs re-reading)", err)
			}
		})

	// A probe whose type needs CONVERTING to reach the elements is refused,
	// loudly, rather than answered wrongly.
	//
	// An explicit value list can convert per item — each one is a Value that
	// PromoteValue can wrap, which is how `u IN ('<uuid text>')` works. A bare
	// ARRAY COLUMN is ONE value and PromoteValue is scalar, so there is nothing
	// to wrap and no element-wise mode to reach for. Left unchecked, a UUID
	// probe against a STRING ARRAY compares [16]byte with string, cmpAny
	// declines the pair rather than erroring, and the matching row is silently
	// dropped while NOT IN admits it.
	//
	// The refusal is the honest answer: the missing capability is element-wise
	// promotion of a runtime array, and saying so beats answering as though it
	// were there. This arm is what stops the answer from being silent.
	t.Run("a probe needing element conversion is refused, not silently wrong", func(t *testing.T) {
		udb := openArrayUUIDDB(t)
		mwjoMustExec(t, udb, ctx, "INSERT INTO u (id, uu, s, us) VALUES "+
			"(1, '11111111-1111-1111-1111-111111111111', 'hit', ['hit', 'other'])")

		for _, pred := range []string{"uu IN us", "uu NOT IN us"} {
			_, err := udb.QueryContext(ctx,
				fmt.Sprintf("SELECT id FROM u WHERE %s ORDER BY id", pred))
			if err == nil {
				t.Errorf("`%s` was ACCEPTED. A UUID probe cannot be compared against STRING "+
					"array elements without per-element promotion, which this path does not "+
					"have — so accepting it means cmpAny is declining the pair and the answer "+
					"is silently wrong in whichever direction the query asked", pred)
				continue
			}
			if !strings.Contains(err.Error(), "42804") {
				t.Errorf("`%s` was refused for an unexpected reason: %v\n"+
					"  (expected the 42804 type-incompatibility gate)", pred, err)
			}
		}

		// The SAME element type is fine and must stay fine — the gate refuses a
		// conversion it cannot perform, not every array whose elements are not
		// the probe's exact type.
		got, err := mmRows(t, ctx, udb, "SELECT id FROM u WHERE s IN us ORDER BY id")
		if err != nil {
			t.Fatalf("a STRING probe against a STRING ARRAY must work: %v", err)
		}
		if !mmEqRows(got, []string{"1"}) {
			t.Errorf("STRING in STRING ARRAY: got %v, want [1]", got)
		}
	})

	// The rejections, which are the other half of Java's type test.
	t.Run("a NON-array column is refused, naming the type", func(t *testing.T) {
		for _, pred := range []string{"b IN s", "b IN id", "s IN b"} {
			_, err := db.QueryContext(ctx,
				fmt.Sprintf("SELECT id FROM t WHERE %s ORDER BY id", pred))
			if err == nil {
				t.Errorf("a non-array column was accepted as an IN list: %s", pred)
				continue
			}
			if !strings.Contains(err.Error(), "must be of array type") {
				t.Errorf("wrong rejection for %s: %v\n  (Java names the offending TYPE here, "+
					"and Go carries that sentence verbatim — a generic refusal loses the "+
					"distinction between 'unsupported' and 'wrong type')", pred, err)
			}
		}
	})

	// THE BRACKETED SPELLING IS A DIFFERENT QUERY and must stay one. `b IN (xs)`
	// is a one-item value list whose item is an array, so it compares b against
	// the array AS A VALUE — not against its elements. Go refuses it as a
	// composite comparand, and that refusal is what stops the two spellings
	// silently converging.
	t.Run("the bracketed spelling is not a membership test", func(t *testing.T) {
		_, err := db.QueryContext(ctx, "SELECT id FROM t WHERE b IN (xs) ORDER BY id")
		if err == nil {
			t.Fatal("`b IN (xs)` was accepted. With brackets this is a ONE-ITEM list whose " +
				"item is an array, i.e. a comparison of a BIGINT against an ARRAY — not a " +
				"membership test. Accepting it means the two spellings have converged and one " +
				"of them is now answering a question nobody asked")
		}
		// MEASURED: the refusal is 42804 "The operands of a comparison operator
		// are not compatible", from the LHS-versus-element type gate — which
		// fires before the composite-comparand check this arm was first written
		// expecting. That is the better of the two refusals and the reason is
		// worth stating: with brackets the item IS the array, so the engine is
		// being asked to compare a BIGINT with a BIGINT ARRAY, and naming the
		// type incompatibility says more than "complex type" would.
		if !strings.Contains(err.Error(), "42804") {
			t.Errorf("`b IN (xs)` was refused for an unexpected reason: %v\n"+
				"  (expected the 42804 type-incompatibility gate — a BIGINT compared against a "+
				"BIGINT ARRAY. A different refusal means the bracketed form is now taking some "+
				"other path, and whether that path is a membership test needs checking)", err)
		}
	})
}

// slicesContains is a local helper so this file does not depend on the Go
// version's slices package being wired into the test target's deps.
func slicesContains(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}
