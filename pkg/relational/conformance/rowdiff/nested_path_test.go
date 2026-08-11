package rowdiff_test

import (
	"strings"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/relational/conformance/rowdiff"
)

// TestNestedPathIsNotADot is the pin for the distinction three separate readers
// used to get wrong in the same way.
//
// "Contains a dot" and "reaches into a struct" are different questions, and a
// JOIN projection entry is where they come apart: every entry is
// alias-qualified, so the keys-only projection {"L.ID","R.ID"} is dotted end to
// end and entirely FLAT. The two directions are pinned separately because they
// fail separately — a helper that strips too eagerly loses real nesting, and one
// that strips nothing counts every join entry as nested.
func TestNestedPathIsNotADot(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		entry    string
		stripped string
		nested   bool
		segments int
	}{
		// Unqualified.
		{"ID", "ID", false, 1},
		{"A", "A", false, 1},
		{"N.A", "N.A", true, 2},
		{"N.DP.A", "N.DP.A", true, 3},
		// Alias-qualified: the leading dot is the TABLE, not a struct step.
		{"L.ID", "ID", false, 1},
		{"M.ID", "ID", false, 1},
		{"R.ID", "ID", false, 1},
		{"L.A", "A", false, 1},
		{"L.N.A", "N.A", true, 2},
		{"M.N.B", "N.B", true, 2},
		{"R.N.DP.A", "N.DP.A", true, 3},
		// Only a LEADING qualifier is stripped, and only once: `L.L.A` is a
		// qualified column named `L.A`, not a doubly-qualified `A`.
		{"L.L.A", "L.A", true, 2},
	} {
		tc := tc
		t.Run(tc.entry, func(t *testing.T) {
			t.Parallel()
			if got := rowdiff.StripJoinQualifier(tc.entry); got != tc.stripped {
				t.Errorf("StripJoinQualifier(%q) = %q, want %q", tc.entry, got, tc.stripped)
			}
			if got := rowdiff.IsNestedPath(tc.entry); got != tc.nested {
				t.Errorf("IsNestedPath(%q) = %v, want %v", tc.entry, got, tc.nested)
			}
			if got := rowdiff.PathSegments(tc.entry); got != tc.segments {
				t.Errorf("PathSegments(%q) = %d, want %d", tc.entry, got, tc.segments)
			}
		})
	}
}

// TestJoinQualifiersAreNotColumnNames is the shelf-life guard on the strip.
//
// StripJoinQualifier is only unambiguous while no table has a column whose
// FIRST segment is L, M or R: the day one does, the helper silently eats a real
// column's root and every reader of it starts calling a nested path flat. That
// is the silent direction, so it is measured against both generated schemas
// rather than assumed.
func TestJoinQualifiersAreNotColumnNames(t *testing.T) {
	t.Parallel()
	quals := map[string]bool{}
	for _, q := range rowdiff.JoinQualifiers() {
		quals[q] = true
	}
	if len(quals) == 0 {
		t.Fatal("no join qualifiers: StripJoinQualifier strips nothing and every join entry reads as nested")
	}
	for _, tc := range []struct {
		name  string
		table rowdiff.TableDef
	}{
		{"flat", rowdiff.Generate(1).Table},
		{"nested", rowdiff.GenerateNested(1).Table},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cols := append([]rowdiff.ColumnDef{{Name: "ID"}}, tc.table.Cols...)
			for _, c := range cols {
				root, _, _ := strings.Cut(c.Name, ".")
				if quals[root] {
					t.Errorf("table %s has column %q whose root %q is also a join qualifier — StripJoinQualifier "+
						"now eats it and every nesting decision about that column is wrong",
						tc.table.Name, c.Name, root)
				}
			}
		})
	}
}

// TestEveryProjectionEntryResolvesAfterStripping is the other half of the same
// guard, taken from the generator's real output rather than from the schema.
//
// It asserts the strip is TOTAL: after removing a leading qualifier, every
// projection entry the generator emits names a column the table actually has.
// An entry that does not is either a generator bug or a qualifier the strip
// list has not learned about, and both make the nesting verdict meaningless.
func TestEveryProjectionEntryResolvesAfterStripping(t *testing.T) {
	t.Parallel()
	var checked, qualified, nested int
	for seed := uint64(1); seed <= 40; seed++ {
		c := rowdiff.GenerateNested(seed)
		known := map[string]bool{"ID": true}
		for _, col := range c.Table.Cols {
			known[col.Name] = true
		}
		for _, q := range c.Queries {
			for _, proj := range c.ProjectionsFor(q) {
				for _, p := range proj {
					checked++
					stripped := rowdiff.StripJoinQualifier(p)
					if stripped != p {
						qualified++
					}
					if rowdiff.IsNestedPath(p) {
						nested++
					}
					if !known[stripped] {
						t.Fatalf("seed %d: projection entry %q strips to %q, which table %s does not have",
							seed, p, stripped, c.Table.Name)
					}
				}
			}
		}
	}
	// All three populations must be non-empty or the check above ran over a set
	// that cannot express the failure: no qualified entries means the strip was
	// never exercised, and no nested entries means the nesting verdict was
	// uniformly false and would pass for any helper at all.
	if checked == 0 || qualified == 0 || nested == 0 {
		t.Fatalf("this gate measured nothing: %d entries, %d qualified, %d nested", checked, qualified, nested)
	}
	t.Logf("projection entries over 40 nested seeds: %d total, %d alias-qualified, %d nested paths", checked, qualified, nested)
}

// TestPredicateReadsANestedPathWalksTheSpec pins the WHERE-side decision on the
// typed spec rather than on rendered SQL.
//
// Every column-bearing field of Pred gets its own case. A field the walker
// forgets reports "this predicate does not nest", which is the silent direction:
// the census loses a `where` axis and reads as if the generator never put a
// nested path in a predicate.
func TestPredicateReadsANestedPathWalksTheSpec(t *testing.T) {
	t.Parallel()
	leaf := func(p *rowdiff.Pred) *rowdiff.BoolNode { return &rowdiff.BoolNode{Leaf: p} }
	eq := predicates.ComparisonEquals
	for _, tc := range []struct {
		name string
		node *rowdiff.BoolNode
		want bool
	}{
		{"nil tree", nil, false},
		{"flat col", leaf(&rowdiff.Pred{Col: "A", Op: eq, Lit: int64(1)}), false},
		{"nested col", leaf(&rowdiff.Pred{Col: "N.A", Op: eq, Lit: int64(1)}), true},
		{"depth-3 col", leaf(&rowdiff.Pred{Col: "N.DP.A", Op: eq, Lit: int64(1)}), true},
		{"qualified flat col", leaf(&rowdiff.Pred{Col: "A", Qual: "L", Op: eq, Lit: int64(1)}), false},
		{"qualified nested col", leaf(&rowdiff.Pred{Col: "N.A", Qual: "L", Op: eq, Lit: int64(1)}), true},
		{"nested rhs col", leaf(&rowdiff.Pred{Col: "A", RhsCol: "N.A", Op: eq}), true},
		{"nested arith operand", leaf(&rowdiff.Pred{Col: "A", HasArith: true, ArithCol2: "N.A", Op: eq, Lit: int64(1)}), true},
		{"nested bitwise operand", leaf(&rowdiff.Pred{Col: "A", Bitwise: true, BitOp: "BITAND", BitCol2: "N.A", Op: eq, Lit: int64(1)}), true},
		{"nested strfn col", leaf(&rowdiff.Pred{StrFn: &rowdiff.StrFnSpec{Col: "N.S"}, Op: eq, Lit: "x"}), true},
		{"nested numfn col", leaf(&rowdiff.Pred{NumFn: &rowdiff.NumFnSpec{Col: "N.A"}, Op: eq, Lit: int64(1)}), true},
		{"nested cast col", leaf(&rowdiff.Pred{Cast: &rowdiff.CastSpec{Col: "N.A", FromInt: true}, Op: eq, Lit: "1"}), true},
		{"nested case WHEN col", leaf(&rowdiff.Pred{
			Case: &rowdiff.CaseSpec{When: &rowdiff.Pred{Col: "N.A", Op: eq, Lit: int64(1)}, ThenLit: 1, ElseLit: 0},
			Op:   eq, Lit: int64(1),
		}), true},
		{"nested case THEN col", leaf(&rowdiff.Pred{
			Case: &rowdiff.CaseSpec{When: &rowdiff.Pred{Col: "A", Op: eq, Lit: int64(1)}, ThenCol: "N.A", ElseLit: 0},
			Op:   eq, Lit: int64(1),
		}), true},
		{"nested case ELSE col", leaf(&rowdiff.Pred{
			Case: &rowdiff.CaseSpec{When: &rowdiff.Pred{Col: "A", Op: eq, Lit: int64(1)}, ThenLit: 1, ElseCol: "N.A"},
			Op:   eq, Lit: int64(1),
		}), true},
		{"all-flat CASE", leaf(&rowdiff.Pred{
			Case: &rowdiff.CaseSpec{When: &rowdiff.Pred{Col: "A", Op: eq, Lit: int64(1)}, ThenCol: "B", ElseCol: "C"},
			Op:   eq, Lit: int64(1),
		}), false},
		{"nesting buried in an AND kid", &rowdiff.BoolNode{And: true, Kids: []*rowdiff.BoolNode{
			leaf(&rowdiff.Pred{Col: "A", Op: eq, Lit: int64(1)}),
			leaf(&rowdiff.Pred{Col: "N.DP.S", Op: eq, Lit: "x"}),
		}}, true},
		{"all-flat OR tree", &rowdiff.BoolNode{Kids: []*rowdiff.BoolNode{
			leaf(&rowdiff.Pred{Col: "A", Op: eq, Lit: int64(1)}),
			leaf(&rowdiff.Pred{Col: "C", Op: eq, Lit: int64(2)}),
		}}, false},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := rowdiff.PredicateReadsANestedPath(tc.node); got != tc.want {
				t.Errorf("PredicateReadsANestedPath = %v, want %v", got, tc.want)
			}
		})
	}
}
