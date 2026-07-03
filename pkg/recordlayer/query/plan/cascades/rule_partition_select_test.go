package cascades

import (
	"strings"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// aliasSet builds a lowerAliases set from alias names.
func aliasSet(names ...string) map[values.CorrelationIdentifier]struct{} {
	s := make(map[values.CorrelationIdentifier]struct{}, len(names))
	for _, n := range names {
		s[values.NamedCorrelationIdentifier(n)] = struct{}{}
	}
	return s
}

// joinPred builds an equi-predicate `a.col = b.col` whose
// GetCorrelatedToOfPredicate is {a, b} — the shape PartitionSelectRule
// classifies. Each side is a FieldValue over a QuantifiedObjectValue(alias).
func joinPred(a, b string) predicates.QueryPredicate {
	left := values.NewFieldValue(
		values.NewQuantifiedObjectValue(values.NamedCorrelationIdentifier(a)),
		"col", values.UnknownType,
	)
	right := values.NewFieldValue(
		values.NewQuantifiedObjectValue(values.NamedCorrelationIdentifier(b)),
		"col", values.UnknownType,
	)
	return predicates.NewComparisonPredicate(
		left,
		predicates.Comparison{Type: predicates.ComparisonEquals, Operand: right},
	)
}

// TestRebaseBuriedLowerReferences pins the RFC-069 correctness fix: a spanning
// upper predicate referencing a lower table COLLAPSED INTO THE MERGE QUANTIFIER
// must be rewritten so its column access flows through the merge quantifier by
// qualified ALIAS.COL name. Emitting it unrebased (referencing the bare buried
// alias the upper select no longer binds) makes an INVALID memo member that a
// later re-partition mis-classifies → resolves to null → 0 rows.
func TestRebaseBuriedLowerReferences(t *testing.T) {
	t.Parallel()

	t3 := values.NamedCorrelationIdentifier("T3")
	t2 := values.NamedCorrelationIdentifier("T2")
	// rebaseBuriedLowerReferences treats the merge alias opaquely (it never parses
	// the name), so a plain identifier is sufficient — the synthetic "$m_…" string
	// scheme was retired in RFC-077 7.5 (merge quantifiers now carry a uniqueId).
	merge := values.UniqueCorrelationIdentifier()

	// Spanning predicate t3.t2_id = t2.id, where T3 is collapsed into the merge
	// and T2 is an upper table.
	pred := predicates.NewComparisonPredicate(
		values.NewFieldValue(values.NewQuantifiedObjectValue(t3), "t2_id", values.UnknownType),
		predicates.Comparison{
			Type:    predicates.ComparisonEquals,
			Operand: values.NewFieldValue(values.NewQuantifiedObjectValue(t2), "id", values.UnknownType),
		},
	)

	buried := map[values.CorrelationIdentifier]struct{}{t3: {}}
	got := rebaseBuriedLowerReferences(pred, buried, merge)

	// After rebasing, the predicate must NOT reference the bare buried alias T3.
	corr := predicates.GetCorrelatedToOfPredicate(got)
	if _, stillT3 := corr[t3]; stillT3 {
		t.Fatalf("rebased predicate still references buried alias T3: corr=%v", corr)
	}
	// It MUST reference the merge quantifier (which the upper select binds) and
	// still the upper table T2.
	if _, hasMerge := corr[merge]; !hasMerge {
		t.Errorf("rebased predicate does not reference the merge quantifier %q: corr=%v", merge.Name(), corr)
	}
	if _, hasT2 := corr[t2]; !hasT2 {
		t.Errorf("rebased predicate dropped the upper reference T2: corr=%v", corr)
	}

	// The collapsed side must read the buried column through the merge by the
	// qualified key T3.T2_ID (matching the source-anchored join RC's ALIAS.COL keys).
	cp := got.(*predicates.ComparisonPredicate)
	lhs := cp.Operand.(*values.FieldValue)
	lhsQOV, ok := lhs.Child.(*values.QuantifiedObjectValue)
	if !ok || lhsQOV.Correlation != merge {
		t.Fatalf("collapsed side does not route through the merge quantifier: %#v", lhs)
	}
	if lhs.Field != "T3.T2_ID" {
		t.Errorf("collapsed side field = %q, want qualified %q", lhs.Field, "T3.T2_ID")
	}

	// The upper side (T2) is untouched.
	rhs := cp.Comparison.Operand.(*values.FieldValue)
	rhsQOV := rhs.Child.(*values.QuantifiedObjectValue)
	if rhsQOV.Correlation != t2 || rhs.Field != "id" {
		t.Errorf("upper side wrongly rewritten: %#v", rhs)
	}

	// Empty buried set ⇒ identity (case 1 / case 2 path).
	if rebaseBuriedLowerReferences(pred, nil, merge) != pred {
		t.Errorf("empty buried set must be identity")
	}
}

// scanQuantifier builds a named ForEach quantifier over a fresh base scan,
// standing in for a SQL table source aliased `name`.
func scanQuantifier(name string) expressions.Quantifier {
	scan := &expressions.FullUnorderedScanExpression{}
	tf := expressions.NewLogicalTypeFilterExpression([]string{strings.ToUpper(name)}, pbForEachOf(scan))
	return expressions.NamedForEachQuantifier(
		values.NamedCorrelationIdentifier(name),
		expressions.InitialOf(tf),
	)
}

// chainEqPred builds the join predicate `a.aCol = b.bCol` as QOV-rooted
// FieldValues, so GetCorrelatedToOfPredicate = {a, b} — the spanning shape
// PartitionSelectRule routes to the upper level.
func chainEqPred(a, aCol, b, bCol string) predicates.QueryPredicate {
	return predicates.NewComparisonPredicate(
		values.NewFieldValue(values.NewQuantifiedObjectValue(values.NamedCorrelationIdentifier(a)), aCol, values.UnknownType),
		predicates.Comparison{
			Type:    predicates.ComparisonEquals,
			Operand: values.NewFieldValue(values.NewQuantifiedObjectValue(values.NamedCorrelationIdentifier(b)), bCol, values.UnknownType),
		},
	)
}

// TestPartitionSelect_SeedMergeRestampedOverMergeQuantifier is the unit-level
// regression for the deeply-nested-FlatMap projection bug. The flat 3-way seed
// SELECT t1.id FROM t3,t2,t1 WHERE t3.t2_id=t2.id AND t2.t1_id=t1.id carries the
// translator-built SEED merge value (RFC-074: the sole canonical merge value, now
// the source-anchored join RC). A seed names only its two immediate source aliases but hides
// the real projection (in the Project above), so the rule must keep ALL lowers
// live. When PartitionSelectRule collapses ≥2 tables into a single merge
// quantifier ($m), those original aliases are no longer bound at the upper level —
// they live inside $m's merged row under qualified ALIAS.COL keys. Flowing the
// seed merge UNCHANGED then looked up correlations the upper never binds, so the
// top FlatMap's resultValue evaluated to nil and the projected (deeply-nested)
// T1.ID came back NULL → 200 rows with t1.id != 1.
//
// The fix re-stamps the upper result as a source-anchored join RC over the upper's
// IMMEDIATE quantifiers (merge alias + upper tables) in the merge case. This pins
// that: a merge-case upper must NEVER carry a result value naming an alias it does
// not bind — it must be a source-anchored join RC keyed on bound aliases.
func TestPartitionSelect_SeedMergeRestampedOverMergeQuantifier(t *testing.T) {
	t.Parallel()

	t1, t2, t3 := scanQuantifier("T1"), scanQuantifier("T2"), scanQuantifier("T3")

	// Seed result value: the source-anchored join RESULT value over all three
	// tables — the structure the real flat N-quantifier select carries once
	// SelectMergeRule flattens the binary seeds (RFC-077 7.6). isAnchoredJoinResult
	// marks it so the rule keeps ALL lowers live (the genuine projection lives in
	// the Project above); the leg QOVs are re-exposed at partition time.
	seed := values.NewAnchoredJoinRecord([]values.AnchoredJoinLeg{
		{Alias: values.NamedCorrelationIdentifier("T1"), Columns: []values.Field{{Name: "ID"}, {Name: "T1_ID"}, {Name: "T2_ID"}}},
		{Alias: values.NamedCorrelationIdentifier("T2"), Columns: []values.Field{{Name: "ID"}, {Name: "T1_ID"}, {Name: "T2_ID"}}},
		{Alias: values.NamedCorrelationIdentifier("T3"), Columns: []values.Field{{Name: "ID"}, {Name: "T1_ID"}, {Name: "T2_ID"}}},
	})
	sel := expressions.NewSelectExpressionWithAliases(
		seed,
		[]expressions.Quantifier{t3, t2, t1},
		[]predicates.QueryPredicate{
			chainEqPred("T3", "T2_ID", "T2", "ID"),
			chainEqPred("T2", "T1_ID", "T1", "ID"),
		},
		[]string{"T3", "T2", "T1"},
	)

	ref := expressions.InitialOf(sel)
	yielded := FireExpressionRuleWithMemo(NewPartitionSelectRule(), ref, EmptyPlanContext(), nil)

	if len(yielded) == 0 {
		t.Fatal("PartitionSelectRule yielded no partitions for the 3-way chain seed")
	}

	sawMergeCaseUpper := false
	for i, y := range yielded {
		upper, ok := y.(*expressions.SelectExpression)
		if !ok {
			t.Fatalf("yield[%d]: expected *SelectExpression, got %T", i, y)
		}
		rv := upper.GetResultValue()

		// The bug signature: an upper that still carries a result value naming an
		// unbound alias after collapsing a lower into a merge quantifier. Detect a
		// merge-collapsing partition STRUCTURALLY: it has a quantifier ranging over
		// a lower SelectExpression whose result value is a source-anchored join RC
		// (RecordConstructorValue with AnchoredJoin, the collapsed ≥2-live merge). The merge quantifier now carries a plain
		// uniqueId alias (RFC-077 7.5 retired the synthetic "$m…" string), so the
		// old name-prefix check is gone — the collapsed-merge child is the honest,
		// rename-stable signal.
		hasMergeQuant := false
		for _, q := range upper.GetQuantifiers() {
			childRef := q.GetRangesOver()
			if childRef == nil {
				continue
			}
			for _, m := range childRef.Members() {
				lsel, ok := m.(*expressions.SelectExpression)
				if !ok {
					continue
				}
				if rc, ok := lsel.GetResultValue().(*values.RecordConstructorValue); ok && rc.AnchoredJoin {
					hasMergeQuant = true
					break
				}
			}
			if hasMergeQuant {
				break
			}
		}

		if hasMergeQuant {
			sawMergeCaseUpper = true
			rc, ok := rv.(*values.RecordConstructorValue)
			if !ok || !rc.AnchoredJoin {
				t.Errorf("yield[%d]: merge-case upper result = %T (anchored=%v), want source-anchored *RecordConstructorValue", i, rv, ok && rc.AnchoredJoin)
				continue
			}
			// The re-stamped anchored RC must anchor exactly to the upper's bound
			// aliases: the merge quantifier plus the single upper table. Every leg QOV
			// correlation must be one the upper select actually binds (no dangling
			// original alias).
			bound := make(map[values.CorrelationIdentifier]struct{})
			for _, q := range upper.GetQuantifiers() {
				bound[q.GetAlias()] = struct{}{}
			}
			for a := range values.GetCorrelatedToOfAnchoredJoinLegs(rc) {
				if _, ok := bound[a]; !ok {
					t.Errorf("yield[%d]: re-stamped anchored RC anchors to alias %q the upper does not bind: bound=%v",
						i, a.Name(), bound)
				}
			}
		}
	}

	if !sawMergeCaseUpper {
		t.Fatal("no merge-collapsing partition was generated for the chain seed — " +
			"the (T1⋈T2)⋈T3 associativity (the one the cost model selects) was not explored")
	}
}

// TestMergeQuantifierAlias_Injective REMOVED (RFC-077 7.5): the synthetic
// stable merge-alias encoding (mergeQuantifierAlias) it pinned is gone — merge
// quantifiers now carry a plain uniqueId and intern STRUCTURALLY via alias-aware
// Reference.Insert/InsertFinal (MemoEqual). The interning the alias encoding
// protected is now pinned by the chain task-count gate
// (TestPartitionSelect_ChainInterningBaseline) instead — a far stronger probe
// (it measures the actual exploration sharing, not a string-encoding property).

// orPred builds `a.col = b.col OR a.col = c.col` — one N-ary conjunct whose
// GetCorrelatedToOfPredicate is {a, b, c}.
func orPred(a, b, c string) predicates.QueryPredicate {
	return predicates.NewOr(joinPred(a, b), joinPred(a, c))
}

// TestAliasesConnectedByPredicates pins the union-find connectivity check that
// gates the disconnected-lower skip in PartitionSelectRule: judged over the
// select's FULL conjunct list, so a spanning N-ary predicate connects the
// aliases it touches (a lower-resident-only reading starved
// `WHERE a.x = b.y OR a.x = c.y` of every bipartition → 0AF00), while a
// genuinely predicate-less pair stays disconnected (the chain {A,C} /
// star {XX,YY} pruning that holds the task baseline).
func TestAliasesConnectedByPredicates(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		aliases    map[values.CorrelationIdentifier]struct{}
		predicates []predicates.QueryPredicate
		want       bool
	}{
		{"single alias is trivially connected", aliasSet("A"), nil, true},
		{"two aliases, no predicate", aliasSet("A", "B"), nil, false},
		{
			"two aliases linked by one predicate", aliasSet("A", "B"),
			[]predicates.QueryPredicate{joinPred("A", "B")},
			true,
		},
		{
			"chain lower {A,C}: binary preds touch one each", aliasSet("A", "C"),
			[]predicates.QueryPredicate{joinPred("A", "B"), joinPred("C", "B")},
			false,
		},
		{
			"three aliases in a chain", aliasSet("A", "B", "C"),
			[]predicates.QueryPredicate{joinPred("A", "B"), joinPred("B", "C")},
			true,
		},
		{
			"three aliases, one isolated", aliasSet("A", "B", "C"),
			[]predicates.QueryPredicate{joinPred("A", "B")},
			false,
		},
		{
			"star lower {XX,YY} with hub upper", aliasSet("XX", "YY"),
			[]predicates.QueryPredicate{joinPred("HUB", "XX"), joinPred("HUB", "YY")},
			false,
		},
		{
			"spanning OR connects the pair it touches", aliasSet("B", "C"),
			[]predicates.QueryPredicate{orPred("A", "B", "C")},
			true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := aliasesConnectedByPredicates(tc.aliases, tc.predicates)
			if got != tc.want {
				t.Errorf("aliasesConnectedByPredicates(%v) = %v, want %v", tc.name, got, tc.want)
			}
		})
	}
}

// TestTransitiveCorrelationOrder_RangesOverEdges pins the recovered
// quantifier→sibling correlation edges: Go's Quantifier.GetCorrelatedTo is
// empty (registered divergence), so computeTransitiveCorrelationOrder must
// read the rangesOver Reference's transitive correlation set — a lateral
// unnest's Explode correlates to its array source with NO predicate between
// them, and without this edge every bipartition check (components, cycle,
// lower-depends-on-upper) treats the pair as independent: the cross-product
// paths then tear them apart and the unnest's AS/AT columns silently NULL.
func TestTransitiveCorrelationOrder_RangesOverEdges(t *testing.T) {
	t.Parallel()
	src := scanQuantifier("PB")
	// A quantifier whose EXPRESSION references PB — the Explode shape (any
	// correlated member works; a filter carrying a QOV(PB) predicate is the
	// simplest constructible stand-in).
	correlated := expressions.NewLogicalFilterExpression(
		[]predicates.QueryPredicate{joinPred("PB", "X")},
		pbForEachOf(&expressions.FullUnorderedScanExpression{}),
	)
	x := expressions.NamedForEachQuantifier(
		values.NamedCorrelationIdentifier("X"),
		expressions.InitialOf(correlated),
	)

	order := computeTransitiveCorrelationOrder([]expressions.Quantifier{src, x})
	if _, ok := order[x.GetAlias()][src.GetAlias()]; !ok {
		t.Fatalf("X's rangesOver correlation to PB missing from the correlation order: %v", order[x.GetAlias()])
	}
	if len(order[src.GetAlias()]) != 0 {
		t.Fatalf("PB must not depend on X: %v", order[src.GetAlias()])
	}
}
