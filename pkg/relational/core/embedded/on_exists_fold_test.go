package embedded

import (
	"errors"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/core/parser"
	"fdb.dev/pkg/relational/core/query/logical"
)

func onExistsFoldMarker(t *testing.T, alias string) *predicates.ExistentialValuePredicate {
	t.Helper()
	marker, err := predicates.NewExistentialAlias(values.NamedCorrelationIdentifier(alias), values.NullableLong)
	if err != nil {
		t.Fatalf("NewExistentialAlias(%q): %v", alias, err)
	}
	return marker
}

func onExistsFoldSubquery(alias string) logical.ExistsSubquery {
	return logical.ExistsSubquery{Alias: values.NamedCorrelationIdentifier(alias), Plan: logical.NewScan("D", "d")}
}

func onExistsFoldCmp(t *testing.T, field string) *predicates.ComparisonPredicate {
	t.Helper()
	rowType := &values.RecordType{Fields: []values.Field{{Name: field, Ordinal: 0, FieldType: values.NotNullLong}}}
	qov, err := values.NewQuantifiedObjectValue(values.NamedCorrelationIdentifier("P"), rowType)
	if err != nil {
		t.Fatalf("qov: %v", err)
	}
	fv, err := values.ResolveFieldOrdinals(qov, []int{0})
	if err != nil {
		t.Fatalf("field: %v", err)
	}
	return predicates.NewComparisonPredicate(fv, predicates.NewLiteralComparison(predicates.ComparisonEquals, int64(1)))
}

func onExistsFoldJoin(l, r logical.LogicalOperator, on predicates.QueryPredicate, subqueries ...logical.ExistsSubquery) *logical.LogicalJoin {
	j := logical.NewJoinWithPredicate(l, r, logical.JoinInner, on)
	j.OnExistsSubqueries = subqueries
	return j
}

// TestLiftClusterOnExists pins the lift (on_exists_fold.go): which joins it
// lifts from, what stays on the join, that it copies rather than mutates,
// what it refuses, and that it is the identity with nothing to lift.
func TestLiftClusterOnExists(t *testing.T) {
	t.Parallel()
	scan := logical.NewScan

	t.Run("root_join_lifts_its_marker_and_keeps_the_other_conjuncts", func(t *testing.T) {
		t.Parallel()
		marker := onExistsFoldMarker(t, "q$d")
		eq := onExistsFoldCmp(t, "X")
		j := onExistsFoldJoin(scan("A", "a"), scan("C", "c"), predicates.NewAnd(eq, marker), onExistsFoldSubquery("q$d"))

		lifted, markers, subqueries, err := liftClusterOnExists(j)
		if err != nil {
			t.Fatal(err)
		}
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
		if len(j.OnExistsSubqueries) != 1 || j.OnPredicate == predicates.QueryPredicate(eq) {
			t.Fatal("liftClusterOnExists mutated its input")
		}
	})

	t.Run("on_that_is_only_the_exists_leaves_an_untyped_nil_predicate", func(t *testing.T) {
		t.Parallel()
		marker := onExistsFoldMarker(t, "q$d")
		j := onExistsFoldJoin(scan("A", "a"), scan("C", "c"), marker, onExistsFoldSubquery("q$d"))
		lifted, _, subqueries, err := liftClusterOnExists(j)
		if err != nil {
			t.Fatal(err)
		}
		if len(subqueries) != 1 {
			t.Fatalf("subqueries = %v, want one", subqueries)
		}
		if lifted.OnPredicate != nil {
			t.Fatalf("an ON that was only the EXISTS must leave OnPredicate an untyped nil, got %#v", lifted.OnPredicate)
		}
	})

	t.Run("two_exists_in_one_on_both_lift", func(t *testing.T) {
		t.Parallel()
		m1 := onExistsFoldMarker(t, "q$d")
		m2 := onExistsFoldMarker(t, "q$e")
		eq := onExistsFoldCmp(t, "X")
		j := onExistsFoldJoin(scan("A", "a"), scan("C", "c"), predicates.NewAnd(eq, m1, m2),
			onExistsFoldSubquery("q$d"), onExistsFoldSubquery("q$e"))
		lifted, markers, subqueries, err := liftClusterOnExists(j)
		if err != nil {
			t.Fatal(err)
		}
		if len(markers) != 2 || markers[0] != predicates.QueryPredicate(m1) || markers[1] != predicates.QueryPredicate(m2) {
			t.Fatalf("markers = %v, want [m1 m2]", markers)
		}
		if len(subqueries) != 2 || lifted.OnPredicate != predicates.QueryPredicate(eq) {
			t.Fatalf("subqueries=%d OnPredicate=%v, want 2 and the equality alone", len(subqueries), lifted.OnPredicate)
		}
	})

	t.Run("nested_join_lifts_in_from_order_and_the_root_is_copied_on_the_path", func(t *testing.T) {
		t.Parallel()
		nestedMarker := onExistsFoldMarker(t, "q$d")
		rootMarker := onExistsFoldMarker(t, "q$e")
		eq := onExistsFoldCmp(t, "X")
		nested := onExistsFoldJoin(scan("A", "a"), scan("C", "c"), predicates.NewAnd(eq, nestedMarker), onExistsFoldSubquery("q$d"))
		rootEq := onExistsFoldCmp(t, "Y")
		root := onExistsFoldJoin(nested, scan("E", "e"), predicates.NewAnd(rootEq, rootMarker), onExistsFoldSubquery("q$e"))

		lifted, markers, subqueries, err := liftClusterOnExists(root)
		if err != nil {
			t.Fatal(err)
		}
		if lifted == root {
			t.Fatal("a root on the path to a lifted nested join must be copied")
		}
		if len(markers) != 2 || markers[0] != predicates.QueryPredicate(nestedMarker) || markers[1] != predicates.QueryPredicate(rootMarker) {
			t.Fatalf("markers = %v, want FROM order: the nested join's before the root's", markers)
		}
		if len(subqueries) != 2 || subqueries[0].Alias != values.NamedCorrelationIdentifier("q$d") || subqueries[1].Alias != values.NamedCorrelationIdentifier("q$e") {
			t.Fatalf("subqueries = %v, want [q$d q$e]", subqueries)
		}
		if lifted.OnPredicate != predicates.QueryPredicate(rootEq) || len(lifted.OnExistsSubqueries) != 0 {
			t.Fatal("the root must keep its equality alone")
		}
		liftedNested, ok := lifted.Left.(*logical.LogicalJoin)
		if !ok || liftedNested == nested || liftedNested.OnPredicate != predicates.QueryPredicate(eq) || len(liftedNested.OnExistsSubqueries) != 0 {
			t.Fatal("the nested join must be a copy keeping its equality alone")
		}
		if lifted.Right != root.Right {
			t.Fatal("an untouched leg must be shared, not copied")
		}
	})

	t.Run("stops_at_an_outer_join_and_at_a_lateral_unnest", func(t *testing.T) {
		t.Parallel()
		marker := onExistsFoldMarker(t, "q$d")
		// An OUTER join carrying ON-EXISTS is rejected by the builder; were one
		// to arrive, its ON is not a WHERE and must not lift.
		outer := logical.NewJoinWithPredicate(scan("A", "a"), scan("C", "c"), logical.JoinLeft, marker)
		outer.OnExistsSubqueries = []logical.ExistsSubquery{onExistsFoldSubquery("q$d")}
		root := logical.NewJoinWithPredicate(outer, scan("E", "e"), logical.JoinInner, onExistsFoldCmp(t, "Y"))
		if lifted, _, subqueries, err := liftClusterOnExists(root); err != nil || lifted != root || len(subqueries) != 0 {
			t.Fatal("an ON-EXISTS below an OUTER join must not lift")
		}
		if lifted, _, subqueries, err := liftClusterOnExists(outer); err != nil || lifted != outer || len(subqueries) != 0 {
			t.Fatal("an OUTER root must not lift")
		}
		unnestJoin := onExistsFoldJoin(scan("A", "a"), &logical.LogicalUnnest{Segments: []string{"A", "ARR"}, Alias: "x"}, marker, onExistsFoldSubquery("q$d"))
		if lifted, _, subqueries, err := liftClusterOnExists(unnestJoin); err != nil || lifted != unnestJoin || len(subqueries) != 0 {
			t.Fatal("a lateral-unnest join is a cluster boundary and must not lift")
		}
	})

	t.Run("exists_under_or_is_refused_not_half_lifted", func(t *testing.T) {
		t.Parallel()
		marker := onExistsFoldMarker(t, "q$d")
		eq := onExistsFoldCmp(t, "X")
		other := onExistsFoldCmp(t, "Y")
		j := onExistsFoldJoin(scan("A", "a"), scan("C", "c"),
			predicates.NewOr(predicates.NewAnd(eq, marker), other), onExistsFoldSubquery("q$d"))
		_, _, _, err := liftClusterOnExists(j)
		var apiErr *api.Error
		if err == nil || !errors.As(err, &apiErr) || apiErr.Code != api.ErrCodeUnsupportedOperation {
			t.Fatalf("EXISTS under OR in ON must be refused with %s, got %v", api.ErrCodeUnsupportedOperation, err)
		}
		// The refusal reaches through a parent: a clean root over the OR join.
		root := logical.NewJoinWithPredicate(j, scan("E", "e"), logical.JoinInner, onExistsFoldCmp(t, "Z"))
		if _, _, _, err := liftClusterOnExists(root); err == nil {
			t.Fatal("the refusal must propagate through the cluster")
		}
	})

	t.Run("subquery_without_a_conjunct_marker_is_refused", func(t *testing.T) {
		t.Parallel()
		// Two subqueries, one marker in conjunct position: the other EXISTS
		// sits somewhere the lift cannot see (a scalar expression).
		marker := onExistsFoldMarker(t, "q$d")
		j := onExistsFoldJoin(scan("A", "a"), scan("C", "c"), marker,
			onExistsFoldSubquery("q$d"), onExistsFoldSubquery("q$e"))
		_, _, _, err := liftClusterOnExists(j)
		var apiErr *api.Error
		if err == nil || !errors.As(err, &apiErr) || apiErr.Code != api.ErrCodeUnsupportedQuery {
			t.Fatalf("a subquery with no conjunct marker must be refused with %s, got %v", api.ErrCodeUnsupportedQuery, err)
		}
	})

	t.Run("nothing_to_lift_is_the_identity", func(t *testing.T) {
		t.Parallel()
		nested := logical.NewJoinWithPredicate(scan("A", "a"), scan("C", "c"), logical.JoinInner, onExistsFoldCmp(t, "X"))
		root := logical.NewJoinWithPredicate(nested, scan("E", "e"), logical.JoinInner, onExistsFoldCmp(t, "Y"))
		lifted, markers, subqueries, err := liftClusterOnExists(root)
		if err != nil || lifted != root || markers != nil || subqueries != nil {
			t.Fatal("a cluster with no ON-EXISTS must come back untouched")
		}
	})
}

// TestFoldInnerOnExistsIntoWhere pins the block-level fold: the WHERE filter
// directly above the join takes the markers (before its own conjuncts) and
// the subqueries (before its own); a bare join gets a filter synthesized in
// its position, at the root and under a unary shell alike; a text-only WHERE
// is left alone; nothing else on the spine is touched.
func TestFoldInnerOnExistsIntoWhere(t *testing.T) {
	t.Parallel()
	scan := logical.NewScan

	t.Run("folds_into_the_where_filter_above_the_join", func(t *testing.T) {
		t.Parallel()
		onMarker := onExistsFoldMarker(t, "q$d")
		whereMarker := onExistsFoldMarker(t, "q$f")
		eq := onExistsFoldCmp(t, "X")
		j := onExistsFoldJoin(scan("A", "a"), scan("C", "c"), predicates.NewAnd(eq, onMarker), onExistsFoldSubquery("q$d"))
		whereEq := onExistsFoldCmp(t, "Y")
		f := &logical.LogicalFilter{
			Input:            j,
			Predicate:        predicates.NewAnd(whereEq, whereMarker),
			ExistsSubqueries: []logical.ExistsSubquery{onExistsFoldSubquery("q$f")},
		}
		proj := logical.NewProject(f, []string{"a.id"}, []string{""})

		out, err := foldInnerOnExistsIntoWhere(proj)
		if err != nil {
			t.Fatal(err)
		}
		if out != logical.LogicalOperator(proj) {
			t.Fatal("the root must be unchanged when a WHERE filter exists")
		}
		and, ok := f.Predicate.(*predicates.AndPredicate)
		if !ok || len(and.SubPredicates) != 3 ||
			and.SubPredicates[0] != predicates.QueryPredicate(onMarker) ||
			and.SubPredicates[1] != predicates.QueryPredicate(whereEq) ||
			and.SubPredicates[2] != predicates.QueryPredicate(whereMarker) {
			t.Fatalf("folded predicate = %v, want AND(onMarker, whereEq, whereMarker)", f.Predicate)
		}
		if len(f.ExistsSubqueries) != 2 ||
			f.ExistsSubqueries[0].Alias != values.NamedCorrelationIdentifier("q$d") ||
			f.ExistsSubqueries[1].Alias != values.NamedCorrelationIdentifier("q$f") {
			t.Fatalf("folded subqueries = %v, want [q$d q$f]", f.ExistsSubqueries)
		}
		liftedJoin, ok := f.Input.(*logical.LogicalJoin)
		if !ok || liftedJoin == j || len(liftedJoin.OnExistsSubqueries) != 0 || liftedJoin.OnPredicate != predicates.QueryPredicate(eq) {
			t.Fatal("the filter's input must be the lifted join copy")
		}
	})

	t.Run("synthesizes_the_filter_under_a_shell_and_at_the_root", func(t *testing.T) {
		t.Parallel()
		marker := onExistsFoldMarker(t, "q$d")
		eq := onExistsFoldCmp(t, "X")
		j := onExistsFoldJoin(scan("A", "a"), scan("C", "c"), predicates.NewAnd(eq, marker), onExistsFoldSubquery("q$d"))
		proj := logical.NewProject(j, []string{"a.id"}, []string{""})
		out, err := foldInnerOnExistsIntoWhere(proj)
		if err != nil {
			t.Fatal(err)
		}
		if out != logical.LogicalOperator(proj) {
			t.Fatal("the shell must stay the root")
		}
		f, ok := proj.Input.(*logical.LogicalFilter)
		if !ok {
			t.Fatalf("a filter must be synthesized under the projection, got %T", proj.Input)
		}
		if f.Predicate != predicates.QueryPredicate(marker) || len(f.ExistsSubqueries) != 1 {
			t.Fatalf("synthesized filter = %v / %d subqueries, want the marker and one subquery", f.Predicate, len(f.ExistsSubqueries))
		}
		if lj, ok := f.Input.(*logical.LogicalJoin); !ok || len(lj.OnExistsSubqueries) != 0 {
			t.Fatal("the synthesized filter must sit over the lifted join")
		}

		bare := onExistsFoldJoin(scan("A", "a"), scan("C", "c"), marker, onExistsFoldSubquery("q$d"))
		out, err = foldInnerOnExistsIntoWhere(bare)
		if err != nil {
			t.Fatal(err)
		}
		rootFilter, ok := out.(*logical.LogicalFilter)
		if !ok || rootFilter.Predicate != predicates.QueryPredicate(marker) || len(rootFilter.ExistsSubqueries) != 1 {
			t.Fatalf("a bare join root must become a filter root, got %T", out)
		}
	})

	t.Run("text_only_where_is_left_alone_and_the_join_unfolded", func(t *testing.T) {
		t.Parallel()
		marker := onExistsFoldMarker(t, "q$d")
		j := onExistsFoldJoin(scan("A", "a"), scan("C", "c"), marker, onExistsFoldSubquery("q$d"))
		f := logical.NewFilter(j, "a.id > 0")
		out, err := foldInnerOnExistsIntoWhere(f)
		if err != nil {
			t.Fatal(err)
		}
		if out != logical.LogicalOperator(f) || f.Predicate != nil || len(f.ExistsSubqueries) != 0 || f.Input != logical.LogicalOperator(j) {
			t.Fatal("a text-only WHERE must be left untouched (the translator refuses it)")
		}
	})

	t.Run("no_join_or_nothing_to_lift_is_untouched", func(t *testing.T) {
		t.Parallel()
		f := &logical.LogicalFilter{Input: scan("A", "a"), Predicate: onExistsFoldCmp(t, "Y")}
		if out, err := foldInnerOnExistsIntoWhere(f); err != nil || out != logical.LogicalOperator(f) || len(f.ExistsSubqueries) != 0 {
			t.Fatal("a filter over a scan must be untouched")
		}
		j := logical.NewJoinWithPredicate(scan("A", "a"), scan("C", "c"), logical.JoinInner, onExistsFoldCmp(t, "X"))
		g := &logical.LogicalFilter{Input: j, Predicate: onExistsFoldCmp(t, "Y")}
		if out, err := foldInnerOnExistsIntoWhere(g); err != nil || out != logical.LogicalOperator(g) || g.Input != logical.LogicalOperator(j) {
			t.Fatal("a filter over a cluster with no ON-EXISTS must be untouched")
		}
		outer := logical.NewJoinWithPredicate(scan("A", "a"), scan("C", "c"), logical.JoinLeft, onExistsFoldCmp(t, "X"))
		outer.OnExistsSubqueries = []logical.ExistsSubquery{onExistsFoldSubquery("q$d")}
		h := &logical.LogicalFilter{Input: outer, Predicate: onExistsFoldCmp(t, "Y")}
		if out, err := foldInnerOnExistsIntoWhere(h); err != nil || out != logical.LogicalOperator(h) || len(h.ExistsSubqueries) != 0 {
			t.Fatal("a filter over an OUTER join must be untouched")
		}
	})

	t.Run("refusal_propagates", func(t *testing.T) {
		t.Parallel()
		marker := onExistsFoldMarker(t, "q$d")
		j := onExistsFoldJoin(scan("A", "a"), scan("C", "c"),
			predicates.NewOr(marker, onExistsFoldCmp(t, "Y")), onExistsFoldSubquery("q$d"))
		f := &logical.LogicalFilter{Input: j, Predicate: onExistsFoldCmp(t, "Z")}
		if _, err := foldInnerOnExistsIntoWhere(f); err == nil {
			t.Fatal("an EXISTS under OR in ON must be refused by the fold")
		}
	})
}

const onExistsFoldDDL = `
CREATE TABLE A (id BIGINT, PRIMARY KEY (id))
CREATE TABLE C (id BIGINT, a_id BIGINT, PRIMARY KEY (id))
CREATE TABLE D (id BIGINT, PRIMARY KEY (id))
CREATE TABLE G (id BIGINT, c_id BIGINT, PRIMARY KEY (id))
CREATE TABLE H (id BIGINT, g_id BIGINT, PRIMARY KEY (id))
`

// TestOnExistsFold_NoJoinLeavesTheBuilderUnfolded pins the builder-exit
// invariant the translator asserts: whatever the spelling — binary or
// three-leg, the EXISTS in the root's or the nested join's ON, with a WHERE,
// with a WHERE-EXISTS, or with none, under ORDER BY / LIMIT — the logical
// plan that leaves the builder has NO join carrying OnExistsSubqueries, and
// the block's WHERE filter carries every existential with its marker in
// conjunct position.
func TestOnExistsFold_NoJoinLeavesTheBuilderUnfolded(t *testing.T) {
	t.Parallel()
	tmpl, err := buildSchemaTemplateFromDDL(onExistsFoldDDL)
	if err != nil {
		t.Fatalf("schema DDL: %v", err)
	}
	md := tmpl.Underlying()
	const existsD = " AND EXISTS (SELECT 1 FROM d WHERE d.id = a.id)"
	for _, tc := range []struct {
		name          string
		sql           string
		wantSubs      int
		wantOnConj    int // non-EXISTS ON conjuncts left on the cluster's joins, summed
		wantWhereConj int // conjuncts on the WHERE filter (markers + WHERE's own)
	}{
		{"binary_on_exists_no_where", "SELECT a.id FROM a JOIN c ON c.a_id = a.id" + existsD, 1, 1, 1},
		{"binary_on_exists_plus_where", "SELECT a.id FROM a JOIN c ON c.a_id = a.id" + existsD + " WHERE a.id > 0", 1, 1, 2},
		{"binary_on_exists_plus_where_exists", "SELECT a.id FROM a JOIN c ON c.a_id = a.id" + existsD + " WHERE EXISTS (SELECT 1 FROM g WHERE g.c_id = c.id)", 2, 1, 2},
		{"binary_two_exists_in_on", "SELECT a.id FROM a JOIN c ON c.a_id = a.id" + existsD + " AND EXISTS (SELECT 1 FROM g WHERE g.c_id = c.id)", 2, 1, 2},
		{"binary_on_only_exists", "SELECT a.id FROM a JOIN c ON EXISTS (SELECT 1 FROM d WHERE d.id = a.id)", 1, 0, 1},
		{"threeway_root_on_exists_under_order_by_limit", "SELECT a.id FROM a JOIN c ON c.a_id = a.id JOIN g ON g.c_id = c.id" + existsD + " ORDER BY a.id LIMIT 5", 1, 2, 1},
		{"threeway_nested_on_exists_plus_where_exists", "SELECT a.id FROM a JOIN c ON c.a_id = a.id" + existsD + " JOIN g ON g.c_id = c.id WHERE EXISTS (SELECT 1 FROM h WHERE h.g_id = g.id)", 2, 2, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			root, err := parser.Parse(tc.sql)
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			q := root.Statements().AllStatement()[0].SelectStatement().Query()
			op, err := NewPlanVisitor(md).VisitQuery(q)
			if err != nil {
				t.Fatalf("build: %v", err)
			}
			var filters []*logical.LogicalFilter
			joins := 0
			onConj := 0
			var walk func(op logical.LogicalOperator)
			walk = func(op logical.LogicalOperator) {
				switch o := op.(type) {
				case *logical.LogicalJoin:
					joins++
					if len(o.OnExistsSubqueries) != 0 {
						t.Fatalf("a join left the builder carrying %d ON subqueries:\n%s", len(o.OnExistsSubqueries), op.Explain(""))
					}
					if qp, ok := o.OnPredicate.(predicates.QueryPredicate); ok {
						onConj += len(conjunctsOf(qp))
						if len(extractExistsMarkers(qp)) != 0 {
							t.Fatalf("a join left the builder with an EXISTS marker in its ON:\n%s", op.Explain(""))
						}
					}
				case *logical.LogicalFilter:
					filters = append(filters, o)
				}
				for _, ch := range op.Children() {
					walk(ch)
				}
			}
			walk(op)
			if joins == 0 {
				t.Fatal("no join in the plan")
			}
			if len(filters) != 1 {
				t.Fatalf("want exactly one filter (the block's WHERE), got %d:\n%s", len(filters), op.Explain(""))
			}
			f := filters[0]
			if _, overJoin := f.Input.(*logical.LogicalJoin); !overJoin {
				t.Fatalf("the WHERE filter must sit directly over the join, got %T", f.Input)
			}
			if len(f.ExistsSubqueries) != tc.wantSubs {
				t.Fatalf("filter carries %d existentials, want %d", len(f.ExistsSubqueries), tc.wantSubs)
			}
			if got := len(extractExistsMarkers(f.Predicate)); got != tc.wantSubs {
				t.Fatalf("filter predicate has %d EXISTS markers in conjunct position, want %d: %v", got, tc.wantSubs, f.Predicate)
			}
			if got := len(conjunctsOf(f.Predicate)); got != tc.wantWhereConj {
				t.Fatalf("filter predicate has %d conjuncts, want %d: %v", got, tc.wantWhereConj, f.Predicate)
			}
			if onConj != tc.wantOnConj {
				t.Fatalf("the cluster's joins keep %d non-EXISTS ON conjuncts, want %d", onConj, tc.wantOnConj)
			}
		})
	}

	t.Run("exists_under_or_in_on_is_refused_by_the_builder", func(t *testing.T) {
		t.Parallel()
		root, err := parser.Parse("SELECT a.id FROM a JOIN c ON (c.a_id = a.id" + existsD + ") OR c.id > 100")
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		q := root.Statements().AllStatement()[0].SelectStatement().Query()
		_, err = NewPlanVisitor(md).VisitQuery(q)
		var apiErr *api.Error
		if err == nil || !errors.As(err, &apiErr) || apiErr.Code != api.ErrCodeUnsupportedOperation {
			t.Fatalf("want %s from the builder, got %v", api.ErrCodeUnsupportedOperation, err)
		}
	})
}
