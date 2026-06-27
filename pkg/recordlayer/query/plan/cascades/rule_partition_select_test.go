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

// TestLowerAliasesConnected pins the union-find connectivity check that gates
// the degenerate cross-product skip in PartitionSelectRule. A disconnected
// lower (no predicate links its quantifiers, e.g. {A,C} for chain A—B—C or
// {XX,YY} for a star) flows a multi-alias RecordConstructorValue the executor
// cannot resolve, so it must be reported as NOT connected → skipped.
func TestLowerAliasesConnected(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		aliases    map[values.CorrelationIdentifier]struct{}
		predicates []predicates.QueryPredicate
		want       bool
	}{
		{
			name:    "single alias is trivially connected",
			aliases: aliasSet("A"),
			want:    true,
		},
		{
			name:    "single alias ignores predicates",
			aliases: aliasSet("A"),
			predicates: []predicates.QueryPredicate{
				joinPred("A", "B"), // B not in lower; still connected (size 1)
			},
			want: true,
		},
		{
			name:       "two aliases, no predicate → disconnected",
			aliases:    aliasSet("A", "B"),
			predicates: nil,
			want:       false,
		},
		{
			name:    "two aliases linked by one predicate → connected",
			aliases: aliasSet("A", "B"),
			predicates: []predicates.QueryPredicate{
				joinPred("A", "B"),
			},
			want: true,
		},
		{
			name:    "two aliases, predicate touches only one (spans to upper) → disconnected",
			aliases: aliasSet("A", "C"),
			predicates: []predicates.QueryPredicate{
				// A—B and C—B: each intersects {A,C} in exactly one alias, so
				// neither links A to C. This is the chain A—B—C lower {A,C}.
				joinPred("A", "B"),
				joinPred("C", "B"),
			},
			want: false,
		},
		{
			name:    "three aliases in a chain → connected",
			aliases: aliasSet("A", "B", "C"),
			predicates: []predicates.QueryPredicate{
				joinPred("A", "B"),
				joinPred("B", "C"),
			},
			want: true,
		},
		{
			name:    "three aliases, one isolated → disconnected",
			aliases: aliasSet("A", "B", "C"),
			predicates: []predicates.QueryPredicate{
				joinPred("A", "B"), // C never linked
			},
			want: false,
		},
		{
			name:    "star lower {XX,YY} with hub in upper → disconnected",
			aliases: aliasSet("XX", "YY"),
			predicates: []predicates.QueryPredicate{
				joinPred("HUB", "XX"),
				joinPred("HUB", "YY"),
			},
			want: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := lowerAliasesConnected(tc.aliases, tc.predicates)
			if got != tc.want {
				t.Errorf("lowerAliasesConnected(%v) = %v, want %v", tc.name, got, tc.want)
			}
		})
	}
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
