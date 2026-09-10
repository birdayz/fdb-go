package query

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/relational/core/query/logical"
)

// onExistsMarker builds the ON-clause EXISTS marker the builder installs:
// the ExistentialValuePredicate over the subquery alias.
func onExistsMarker(t *testing.T, alias string) *predicates.ExistentialValuePredicate {
	t.Helper()
	marker, err := predicates.NewExistentialAlias(values.NamedCorrelationIdentifier(alias), values.NullableLong)
	if err != nil {
		t.Fatalf("NewExistentialAlias(%q): %v", alias, err)
	}
	return marker
}

func onExistsSubquery(alias string) logical.ExistsSubquery {
	return logical.ExistsSubquery{Alias: values.NamedCorrelationIdentifier(alias), Plan: scan("D", "d")}
}

func literalCmp(t *testing.T, field string) *predicates.ComparisonPredicate {
	t.Helper()
	return predicates.NewComparisonPredicate(
		exactTestNamedField(t, "P", field, values.NotNullLong),
		predicates.NewLiteralComparison(predicates.ComparisonEquals, int64(1)),
	)
}

// TestLiftClusterOnExists pins the ON-EXISTS lift (on_exists_fold.go): which
// joins it lifts from, what stays on the join, that it copies rather than
// mutates, and that it is the identity when there is nothing to lift.
func TestLiftClusterOnExists(t *testing.T) {
	t.Parallel()

	t.Run("root_join_lifts_its_marker_and_keeps_the_other_conjuncts", func(t *testing.T) {
		t.Parallel()
		marker := onExistsMarker(t, "q$d")
		eq := literalCmp(t, "X")
		j := logical.NewJoinWithPredicate(scan("A", "a"), scan("C", "c"), logical.JoinInner, predicates.NewAnd(eq, marker))
		j.OnExistsSubqueries = []logical.ExistsSubquery{onExistsSubquery("q$d")}

		lifted, markers, subqueries := liftClusterOnExists(j)
		if lifted == j {
			t.Fatal("a join with an ON-EXISTS must come back as a COPY")
		}
		if len(markers) != 1 || markers[0] != predicates.QueryPredicate(marker) {
			t.Fatalf("markers = %v, want the one ON marker", markers)
		}
		if len(subqueries) != 1 || subqueries[0].Alias != values.NamedCorrelationIdentifier("q$d") {
			t.Fatalf("subqueries = %v, want the one ON subquery", subqueries)
		}
		if lifted.OnPredicate != predicates.QueryPredicate(eq) {
			t.Fatalf("the lifted join must keep exactly the non-EXISTS conjunct, got %v", lifted.OnPredicate)
		}
		if len(lifted.OnExistsSubqueries) != 0 {
			t.Fatal("the lifted join must carry no ON subqueries")
		}
		// The original is untouched: the logical tree is shared with the
		// generator's guards.
		if len(j.OnExistsSubqueries) != 1 || j.OnPredicate == predicates.QueryPredicate(eq) {
			t.Fatal("liftClusterOnExists mutated its input")
		}
	})

	t.Run("on_that_is_only_the_exists_leaves_an_untyped_nil_predicate", func(t *testing.T) {
		t.Parallel()
		marker := onExistsMarker(t, "q$d")
		j := logical.NewJoinWithPredicate(scan("A", "a"), scan("C", "c"), logical.JoinInner, marker)
		j.OnExistsSubqueries = []logical.ExistsSubquery{onExistsSubquery("q$d")}
		lifted, _, subqueries := liftClusterOnExists(j)
		if len(subqueries) != 1 {
			t.Fatalf("subqueries = %v, want one", subqueries)
		}
		if lifted.OnPredicate != nil {
			t.Fatalf("an ON that was only the EXISTS must leave OnPredicate an untyped nil, got %#v", lifted.OnPredicate)
		}
	})

	t.Run("nested_inner_join_lifts_through_and_a_clean_parent_is_copied_on_the_path", func(t *testing.T) {
		t.Parallel()
		marker := onExistsMarker(t, "q$d")
		eq := literalCmp(t, "X")
		nested := logical.NewJoinWithPredicate(scan("A", "a"), scan("C", "c"), logical.JoinInner, predicates.NewAnd(eq, marker))
		nested.OnExistsSubqueries = []logical.ExistsSubquery{onExistsSubquery("q$d")}
		rootEq := literalCmp(t, "Y")
		root := logical.NewJoinWithPredicate(nested, scan("E", "e"), logical.JoinInner, rootEq)

		lifted, markers, subqueries := liftClusterOnExists(root)
		if lifted == root {
			t.Fatal("a root on the path to a lifted nested join must be copied")
		}
		if lifted.OnPredicate != predicates.QueryPredicate(rootEq) || len(lifted.OnExistsSubqueries) != 0 {
			t.Fatal("the root's own ON must be unchanged")
		}
		liftedNested, ok := lifted.Left.(*logical.LogicalJoin)
		if !ok || liftedNested == nested {
			t.Fatal("the nested join must be a copy on the lifted tree")
		}
		if liftedNested.OnPredicate != predicates.QueryPredicate(eq) || len(liftedNested.OnExistsSubqueries) != 0 {
			t.Fatalf("nested join after lift: OnPredicate=%v subqueries=%d, want the equality alone and none", liftedNested.OnPredicate, len(liftedNested.OnExistsSubqueries))
		}
		if len(markers) != 1 || len(subqueries) != 1 {
			t.Fatalf("markers=%d subqueries=%d, want 1/1", len(markers), len(subqueries))
		}
		if lifted.Right != root.Right {
			t.Fatal("an untouched leg must be shared, not copied")
		}
		// After the lift the cluster gathers as three flat legs — the walk is
		// the one gatherInnerClusterLegs makes.
		tr := newChainedSpineTranslator(t)
		if legs := tr.gatherInnerClusterLegs(lifted); len(legs) != 3 {
			t.Fatalf("lifted cluster gathers %d legs, want 3", len(legs))
		}
		if legs := tr.gatherInnerClusterLegs(root); len(legs) != 2 {
			t.Fatalf("un-lifted cluster gathers %d legs, want 2 (the ON-EXISTS join is an opaque leg)", len(legs))
		}
	})

	t.Run("stops_at_an_outer_join_and_at_a_lateral_unnest", func(t *testing.T) {
		t.Parallel()
		marker := onExistsMarker(t, "q$d")
		// An OUTER join carrying ON-EXISTS is rejected by the builder; were one
		// to arrive, its ON is not a WHERE and must not lift.
		outer := logical.NewJoinWithPredicate(scan("A", "a"), scan("C", "c"), logical.JoinLeft, marker)
		outer.OnExistsSubqueries = []logical.ExistsSubquery{onExistsSubquery("q$d")}
		root := logical.NewJoinWithPredicate(outer, scan("E", "e"), logical.JoinInner, literalCmp(t, "Y"))
		if lifted, _, subqueries := liftClusterOnExists(root); lifted != root || len(subqueries) != 0 {
			t.Fatal("an ON-EXISTS below an OUTER join must not lift")
		}
		if lifted, _, subqueries := liftClusterOnExists(outer); lifted != outer || len(subqueries) != 0 {
			t.Fatal("an OUTER root must not lift")
		}
		unnestJoin := logical.NewJoinWithPredicate(scan("A", "a"), &logical.LogicalUnnest{Segments: []string{"A", "ARR"}, Alias: "x"}, logical.JoinInner, marker)
		unnestJoin.OnExistsSubqueries = []logical.ExistsSubquery{onExistsSubquery("q$d")}
		if lifted, _, subqueries := liftClusterOnExists(unnestJoin); lifted != unnestJoin || len(subqueries) != 0 {
			t.Fatal("a lateral-unnest join is a cluster boundary and must not lift")
		}
	})

	t.Run("nothing_to_lift_is_the_identity", func(t *testing.T) {
		t.Parallel()
		nested := logical.NewJoinWithPredicate(scan("A", "a"), scan("C", "c"), logical.JoinInner, literalCmp(t, "X"))
		root := logical.NewJoinWithPredicate(nested, scan("E", "e"), logical.JoinInner, literalCmp(t, "Y"))
		lifted, markers, subqueries := liftClusterOnExists(root)
		if lifted != root || markers != nil || subqueries != nil {
			t.Fatal("a cluster with no ON-EXISTS must come back untouched")
		}
	})
}

// TestFoldInnerOnExistsIntoFilter pins the filter-level fold: the markers
// join the WHERE conjunction, the subqueries join the filter's list AFTER
// the WHERE's own, the join loses them, and the filter is copied.
func TestFoldInnerOnExistsIntoFilter(t *testing.T) {
	t.Parallel()

	t.Run("folds_into_an_existing_where", func(t *testing.T) {
		t.Parallel()
		onMarker := onExistsMarker(t, "q$d")
		whereMarker := onExistsMarker(t, "q$f")
		eq := literalCmp(t, "X")
		j := logical.NewJoinWithPredicate(scan("A", "a"), scan("C", "c"), logical.JoinInner, predicates.NewAnd(eq, onMarker))
		j.OnExistsSubqueries = []logical.ExistsSubquery{onExistsSubquery("q$d")}
		whereEq := literalCmp(t, "Y")
		f := &logical.LogicalFilter{
			Input:            j,
			Predicate:        predicates.NewAnd(whereEq, whereMarker),
			ExistsSubqueries: []logical.ExistsSubquery{onExistsSubquery("q$f")},
		}

		folded := foldInnerOnExistsIntoFilter(f)
		if folded == f {
			t.Fatal("a filter with an ON-EXISTS below it must come back as a COPY")
		}
		and, ok := folded.Predicate.(*predicates.AndPredicate)
		if !ok || len(and.SubPredicates) != 3 ||
			and.SubPredicates[0] != predicates.QueryPredicate(whereEq) ||
			and.SubPredicates[1] != predicates.QueryPredicate(whereMarker) ||
			and.SubPredicates[2] != predicates.QueryPredicate(onMarker) {
			t.Fatalf("folded predicate = %v, want AND(whereEq, whereMarker, onMarker)", folded.Predicate)
		}
		if len(folded.ExistsSubqueries) != 2 ||
			folded.ExistsSubqueries[0].Alias != values.NamedCorrelationIdentifier("q$f") ||
			folded.ExistsSubqueries[1].Alias != values.NamedCorrelationIdentifier("q$d") {
			t.Fatalf("folded subqueries = %v, want [q$f q$d]", folded.ExistsSubqueries)
		}
		liftedJoin, ok := folded.Input.(*logical.LogicalJoin)
		if !ok || liftedJoin == j || len(liftedJoin.OnExistsSubqueries) != 0 || liftedJoin.OnPredicate != predicates.QueryPredicate(eq) {
			t.Fatal("the folded filter's input must be the lifted join copy")
		}
		if len(f.ExistsSubqueries) != 1 || f.Input != logical.LogicalOperator(j) {
			t.Fatal("foldInnerOnExistsIntoFilter mutated its input")
		}
	})

	t.Run("where_with_no_exists_of_its_own_becomes_an_existential_filter", func(t *testing.T) {
		t.Parallel()
		onMarker := onExistsMarker(t, "q$d")
		j := logical.NewJoinWithPredicate(scan("A", "a"), scan("C", "c"), logical.JoinInner, onMarker)
		j.OnExistsSubqueries = []logical.ExistsSubquery{onExistsSubquery("q$d")}
		whereEq := literalCmp(t, "Y")
		f := &logical.LogicalFilter{Input: j, Predicate: whereEq}
		folded := foldInnerOnExistsIntoFilter(f)
		and, ok := folded.Predicate.(*predicates.AndPredicate)
		if !ok || len(and.SubPredicates) != 2 || and.SubPredicates[0] != predicates.QueryPredicate(whereEq) || and.SubPredicates[1] != predicates.QueryPredicate(onMarker) {
			t.Fatalf("folded predicate = %v, want AND(whereEq, onMarker)", folded.Predicate)
		}
		if len(folded.ExistsSubqueries) != 1 {
			t.Fatalf("folded subqueries = %d, want 1", len(folded.ExistsSubqueries))
		}
		// findExistsFilterUnderUnaryChain sees the folded filter: a WHERE whose
		// only existential rides the ON clause is an existential filter.
		found, _ := findExistsFilterUnderUnaryChain(f)
		if found == nil || len(found.ExistsSubqueries) != 1 {
			t.Fatal("findExistsFilterUnderUnaryChain must find the ON-EXISTS-folded filter")
		}
	})

	t.Run("identity_when_nothing_lifts", func(t *testing.T) {
		t.Parallel()
		j := logical.NewJoinWithPredicate(scan("A", "a"), scan("C", "c"), logical.JoinInner, literalCmp(t, "X"))
		f := &logical.LogicalFilter{Input: j, Predicate: literalCmp(t, "Y")}
		if foldInnerOnExistsIntoFilter(f) != f {
			t.Fatal("a filter over a cluster with no ON-EXISTS must come back untouched")
		}
		outer := logical.NewJoinWithPredicate(scan("A", "a"), scan("C", "c"), logical.JoinLeft, literalCmp(t, "X"))
		outer.OnExistsSubqueries = []logical.ExistsSubquery{onExistsSubquery("q$d")}
		fo := &logical.LogicalFilter{Input: outer, Predicate: literalCmp(t, "Y")}
		if foldInnerOnExistsIntoFilter(fo) != fo {
			t.Fatal("a filter over an OUTER join must come back untouched")
		}
		fs := &logical.LogicalFilter{Input: scan("A", "a"), Predicate: literalCmp(t, "Y")}
		if foldInnerOnExistsIntoFilter(fs) != fs {
			t.Fatal("a filter over a scan must come back untouched")
		}
	})
}
