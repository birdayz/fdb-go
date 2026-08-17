package query

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/relational/core/query/logical"
)

// nestedSortFixture builds the single-table layout `t1(id BIGINT, n nst)` that
// every arm below sorts against, plus the two nested keys `n.sk` and `n.co` that
// share a struct ROOT and differ only in their leaf accessor.
//
// This is the shape the whole file is about: `sortKeyFieldRef` renders BOTH of
// these as the root `N` (fv.Child is nil, so the ToUpper(fv.Field) arm fires),
// so any identity derived from that rendering conflates them.
func nestedSortFixture(t testing.TB) (src sortSource, keySK, keyCO logical.SortKey, owner values.QuantifiedObjectValue) {
	t.Helper()
	nestedType := &values.RecordType{Fields: []values.Field{
		{Name: "SK", Ordinal: 0, FieldType: values.NotNullLong},
		{Name: "CO", Ordinal: 1, FieldType: values.NotNullLong},
	}}
	rowType := values.NewRecordType("T1", false, []values.Field{
		{Name: "ID", Ordinal: 0, FieldType: values.NotNullLong},
		{Name: "N", Ordinal: 1, FieldType: nestedType},
		{Name: "V", Ordinal: 2, FieldType: values.NotNullLong},
	})
	owner = exactTestQOV(t, "T1", rowType)
	nested := func(leaf string, leafOrdinal int) logical.SortKey {
		return logical.SortKey{
			Expr:  "N." + leaf,
			Value: exactTestField(t, owner, 1, leafOrdinal),
		}
	}
	return sortSource{isJoin: false, singleType: rowType}, nested("SK", 0), nested("CO", 1), owner
}

// outputFieldsWithoutTheKeys is a folded projection for
// `SELECT id, EXISTS(...) AS h FROM t1 ORDER BY ...` — neither nested key is
// projected, so both must be appended as hidden columns.
func outputFieldsWithoutTheKeys(t testing.TB, owner values.Value) []values.RecordConstructorField {
	t.Helper()
	return []values.RecordConstructorField{
		{Name: "ID", Value: exactTestField(t, owner, 0)},
		{Name: "H", Value: values.LiteralValue(true)},
	}
}

func sortChain(keys ...logical.SortKey) []logical.LogicalOperator {
	return []logical.LogicalOperator{&logical.LogicalSort{Keys: keys}}
}

// TestExtraSortColumnsKeepBothMembersOfOneStructRoot drives the defect this
// file's fix was written for, at the unit level and in BOTH key orders.
//
// Two nested ORDER BY keys of the same struct root must produce TWO hidden
// columns. They used to produce one: both keys render as the root `N`, that
// rendering keyed the dedup, and the second key's column was dropped — after
// which the re-applied sort's second key read a slot the folded record did not
// carry and contributed nothing to the order. The rows that fell out were a
// silent wrong answer, not an error.
//
// Driving both orders separately is deliberate. The defect is direction-blind:
// whichever key came second was the one lost, so a repair that satisfied only
// one order would look like a fix and be half of one.
func TestExtraSortColumnsKeepBothMembersOfOneStructRoot(t *testing.T) {
	t.Parallel()

	src, keySK, keyCO, owner := nestedSortFixture(t)

	for _, tc := range []struct {
		name      string
		keys      []logical.SortKey
		wantNames []string
	}{
		{"co-then-sk", []logical.SortKey{keyCO, keySK}, []string{"T1.N.CO", "T1.N.SK"}},
		{"sk-then-co", []logical.SortKey{keySK, keyCO}, []string{"T1.N.SK", "T1.N.CO"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			extra := collectExtraSortColumns(sortChain(tc.keys...),
				outputFieldsWithoutTheKeys(t, owner), src)

			if len(extra) != 2 {
				var got []string
				for _, e := range extra {
					got = append(got, e.name)
				}
				t.Fatalf("collectExtraSortColumns returned %d hidden column(s) %v, want 2 %v.\n"+
					"Two nested keys of ONE struct root must each get their own column. At 1, "+
					"the second key has no column to resolve against and the re-applied sort "+
					"silently drops it — which is wrong ROWS, not an error. Check whether the "+
					"dedup went back to keying on a rendered name (both keys render `N`), or "+
					"whether it was 'upgraded' to an asymmetric derivation test.",
					len(extra), got, tc.wantNames)
			}
			for i, want := range tc.wantNames {
				if extra[i].name != want {
					t.Errorf("hidden column %d is named %q, want %q. A column named for the "+
						"struct ROOT rather than the resolved PATH is the spelling that let "+
						"the two collapse.", i, extra[i].name, want)
				}
			}
			// The names being distinct is hygiene; the VALUES being distinct is the
			// correctness claim. Assert it directly so a future naming scheme cannot
			// make this test pass while both columns read the same member.
			if values.SemanticEqualsUnderAliasMap(extra[0].val, extra[1].val, values.EmptyAliasMap()) {
				t.Errorf("the two hidden columns carry SEMANTICALLY EQUAL values, so both "+
					"read the same struct member and one of the sort keys is a no-op. "+
					"values: %v / %v", extra[0].val, extra[1].val)
			}
		})
	}
}

// TestExtraSortColumnsDedupExactDuplicates is the other side of the same
// decision, and it must be driven or the repair reads as "the dedup was
// deleted". `ORDER BY n.co, n.co` names one column twice; it must still collapse
// to ONE, because the two keys read the identical value.
//
// Java performs no self-dedup at all here (Expressions.difference removes only
// what is derivable from the OUTPUT, Expressions.java:124-146), so this collapse
// is a Go narrowing of the intermediate row. It is sound because equal values
// induce the identical ordering property and the cleanup projection truncates
// positionally either way — but it is a decision, and an undriven decision is an
// untested branch.
func TestExtraSortColumnsDedupExactDuplicates(t *testing.T) {
	t.Parallel()

	src, _, keyCO, owner := nestedSortFixture(t)

	extra := collectExtraSortColumns(sortChain(keyCO, keyCO),
		outputFieldsWithoutTheKeys(t, owner), src)

	if len(extra) != 1 {
		t.Fatalf("collectExtraSortColumns returned %d hidden column(s), want 1: "+
			"`ORDER BY n.co, n.co` reads ONE value twice, so it needs ONE column. "+
			"More than one means the dedup stopped recognising equal values and the "+
			"folded row is widening for nothing.", len(extra))
	}
	if extra[0].name != "T1.N.CO" {
		t.Errorf("hidden column named %q, want the exact owner-qualified path %q", extra[0].name, "T1.N.CO")
	}
}

// TestExtraSortColumnsDedupOnTheValueNotTheSpelling is the arm that separates a
// VALUE-keyed dedup from a NAME-keyed one, and it exists because the first
// version of this file did not have it: reverting the value-keying half left
// every other test in this file GREEN, so that half shipped unpinned until a
// mutation run said so.
//
// The discriminator is two keys that SPELL DIFFERENTLY and READ THE SAME SLOT.
// `ORDER BY t1.v, v` over a single-table source renders `T1.V` and `V` — two
// names — while both resolve to the same bare source column, because a
// single-table fold's flowed row carries columns under bare keys and the
// qualifier is stripped. A name-keyed dedup sees two names and appends two
// columns; the value-keyed dedup sees one read and appends one.
//
// Widening the folded row is not a wrong ANSWER — both columns hold the same
// datum and the sort is unchanged — which is exactly why it needs a test. It is
// the kind of regression that never shows up as a wrong row and so never gets
// found by the row tests that guard the rest of this file.
func TestExtraSortColumnsDedupOnTheValueNotTheSpelling(t *testing.T) {
	t.Parallel()

	src, _, _, owner := nestedSortFixture(t)
	column := exactTestField(t, owner, 2)

	qualified := logical.SortKey{
		Expr:  "T1.V",
		Value: column,
	}
	bare := logical.SortKey{
		Expr:  "V",
		Value: column,
	}

	// The two keys really do spell differently — assert it, or a change to the
	// rendering could make this test pass by collapsing the premise instead of
	// the columns.
	if qualified.Expr == bare.Expr {
		t.Fatal("fixture no longer supplies two distinct SQL spellings")
	}

	extra := collectExtraSortColumns(sortChain(qualified, bare),
		outputFieldsWithoutTheKeys(t, owner), src)

	if len(extra) != 1 {
		var got []string
		for _, e := range extra {
			got = append(got, e.name)
		}
		t.Fatalf("collectExtraSortColumns returned %d hidden column(s) %v, want 1.\n"+
			"`ORDER BY t1.v, v` names ONE column twice: over a single-table source the "+
			"qualifier is stripped, so both keys read the same bare slot. Two columns "+
			"here means the dedup went back to keying on the rendered NAME — which is "+
			"the mechanism that let two nested keys of one struct root collapse, seen "+
			"from its other side.", len(extra), got)
	}
}

// TestExtraSortColumnDedupEqualityIsSymmetric pins the condition that keeps the
// repaired dedup from recreating the very defect it repaired.
//
// The dedup uses SYMMETRIC semantic equality. Java's membership test is
// canBeDerivedFrom (Expression.java:254-264), which is ASYMMETRIC and is sound
// only in the direction Java applies it: an order-by expression against the
// OUTPUT. Applied among the extras themselves it inverts — a struct MEMBER is
// derivable from its ROOT — so `ORDER BY n, n.sk` would drop `n.sk`'s column and
// reproduce the collapse, with a Java citation attached to make it look like an
// improvement.
//
// This is pinned as a unit fact rather than as rows because the shape is not
// reachable through SQL: `ORDER BY n` over a whole struct fails at runtime with
// `no ordering defined between *dynamicpb.Message and *dynamicpb.Message`, there
// being no struct comparator. Pinning a reachable cousin instead would pin
// something else. If a struct comparator ever lands, this shape becomes
// row-reachable and the guard below is what stands between it and wrong rows.
func TestExtraSortColumnDedupEqualityIsSymmetric(t *testing.T) {
	t.Parallel()

	src, keySK, _, owner := nestedSortFixture(t)

	// The struct ROOT as a sort key: a single-accessor read of `N` itself.
	keyRoot := logical.SortKey{
		Expr:  "N",
		Value: exactTestField(t, owner, 1),
	}

	extra := collectExtraSortColumns(sortChain(keyRoot, keySK),
		outputFieldsWithoutTheKeys(t, owner), src)

	if len(extra) != 2 {
		t.Fatalf("collectExtraSortColumns returned %d hidden column(s), want 2 for "+
			"`ORDER BY n, n.sk`.\nThe root and one of its members are DIFFERENT reads and "+
			"need different columns. Collapsing to 1 is the signature of the dedup having "+
			"been switched to an ASYMMETRIC derivation test (canBeDerivedFrom), under which "+
			"a member is derivable from its root. That test is correct against the OUTPUT "+
			"and wrong here; the equality among extras must stay symmetric.", len(extra))
	}

	// State the underlying fact too, so the reason survives even if
	// collectExtraSortColumns is restructured around it.
	rootVal := src.sortKeySourceValue(keyRoot)
	memberVal := src.sortKeySourceValue(keySK)
	if rootVal == nil || memberVal == nil {
		t.Fatalf("fixture no longer produces both source values (root=%v member=%v) — "+
			"the assertion below would hold vacuously", rootVal, memberVal)
	}
	if values.SemanticEqualsUnderAliasMap(rootVal, memberVal, values.EmptyAliasMap()) {
		t.Error("a struct ROOT read and a MEMBER read compare SEMANTICALLY EQUAL. The " +
			"extras dedup rests on them being distinct; if this holds, `ORDER BY n, n.sk` " +
			"loses a column.")
	}
}
