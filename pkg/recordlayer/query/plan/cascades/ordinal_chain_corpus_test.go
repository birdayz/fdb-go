package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// buildOrdinalChainSelect builds an n-table chain T1—T2—…—Tn, each link
// Ti.NEXT_ID = T(i+1).ID, SEEDED with the flat N-leg ORDINAL join RC — one
// values.ResolveOrdinalSeedField per leg column over each leg's exact QOV, the
// exact shape the translator's buildOrdinalJoinResultValue produces for a gated
// maximal inner-join cluster. This is the SOLE seed shape the name-model
// producer has been retired in favor of; PartitionSelectRule routes its
// ≥2-live merges through the positional arm (positionalMergeCase) —
// structurally the only arm now. The chain corpus feeds the interning/task
// baselines and the leg-row-type derivations.
func buildOrdinalChainSelect(t testing.TB, n int) *expressions.SelectExpression {
	t.Helper()
	var quants []expressions.Quantifier
	var aliases []string
	var preds []predicates.QueryPredicate
	var fields []values.RecordConstructorField
	legRoots := make(map[string]values.QuantifiedObjectValue, n)
	for i := 1; i <= n; i++ {
		alias := tName(i)
		legType := values.NewRecordType(alias, false, []values.Field{
			{Name: "ID", FieldType: values.NotNullLong, Ordinal: 0},
			{Name: "NEXT_ID", FieldType: values.NotNullLong, Ordinal: 1},
		})
		quants = append(quants, typedPartitionScanQuantifier(alias, legType))
		aliases = append(aliases, alias)
		qovValue, qovErr := values.NewQuantifiedObjectValue(
			values.NamedCorrelationIdentifier(alias), legType)
		qov := mustConstruct(t, qovValue, qovErr)
		legRoots[alias] = qov
		for col := range legType.Fields {
			fv := mustOrdinalSeedField(t, qov, col)
			fields = append(fields, values.RecordConstructorField{Name: fv.DisplayName(), Value: fv})
		}
	}
	for i := 1; i < n; i++ {
		preds = append(preds, ordinalFieldEquality(
			t, legRoots[tName(i)], 1, legRoots[tName(i+1)], 0))
	}
	seed := values.NewRawRecordConstructorValue(fields...)
	// An ordinal seed that lands without AssertOrdinalJoinSeed is a bug on
	// sight — this corpus asserts the same pristine shape the translator
	// guarantees.
	values.AssertOrdinalJoinSeed(seed)
	selectExpression, selectErr := expressions.NewSelectExpressionWithAliases(seed, quants, preds, aliases)
	return mustConstruct(t, selectExpression, selectErr)
}

func mustOrdinalSeedField(t testing.TB, root values.Value, ordinal int) values.FieldValue {
	t.Helper()
	resolvedValue, resolveErr := values.ResolveOrdinalSeedField(root, ordinal)
	resolved := mustConstruct(t, resolvedValue, resolveErr)
	field, ok := values.AsFieldValue(resolved)
	if !ok {
		t.Fatalf("ordinal seed field %d resolved to %T, want exact FieldValue", ordinal, resolved)
	}
	return field
}

func ordinalFieldEquality(
	t testing.TB,
	left values.Value,
	leftOrdinal int,
	right values.Value,
	rightOrdinal int,
) predicates.QueryPredicate {
	t.Helper()
	return predicates.NewComparisonPredicate(
		mustOrdinalSeedField(t, left, leftOrdinal),
		predicates.Comparison{
			Type:    predicates.ComparisonEquals,
			Operand: mustOrdinalSeedField(t, right, rightOrdinal),
		},
	)
}

// buildOrdinalBoxChainSelect is buildOrdinalChainSelect with the LAST leg a
// dissolved-LEFT BOX: its QOV flows the box's CONCAT type (two buried tables'
// columns with RecordType.Legs boundaries, the RIGHT-normalized shape whose
// preserved leg names the box). The seed stays pristine (every column baked
// ordinal over its leg QOV, runs 0..width-1), so the values/executor twins
// accept it — the ordinal-seeded-box corpus.
func buildOrdinalBoxChainSelect(t testing.TB, n int) *expressions.SelectExpression {
	t.Helper()
	var quants []expressions.Quantifier
	var aliases []string
	var preds []predicates.QueryPredicate
	var fields []values.RecordConstructorField
	legRoots := make(map[string]values.QuantifiedObjectValue, n)
	for i := 1; i < n; i++ {
		alias := tName(i)
		legType := values.NewRecordType(alias, false, []values.Field{
			{Name: "ID", FieldType: values.NotNullLong, Ordinal: 0},
			{Name: "NEXT_ID", FieldType: values.NotNullLong, Ordinal: 1},
		})
		quants = append(quants, typedPartitionScanQuantifier(alias, legType))
		aliases = append(aliases, alias)
		qovValue, qovErr := values.NewQuantifiedObjectValue(
			values.NamedCorrelationIdentifier(alias), legType)
		qov := mustConstruct(t, qovValue, qovErr)
		legRoots[alias] = qov
		for col := range legType.Fields {
			fv := mustOrdinalSeedField(t, qov, col)
			fields = append(fields, values.RecordConstructorField{Name: fv.DisplayName(), Value: fv})
		}
	}
	// The box leg: buried B (2 cols) + rightmost leaf Tn (2 cols), named Tn.
	boxTyp := &values.RecordType{
		Fields: []values.Field{
			{Name: "BID", FieldType: values.NotNullLong, Ordinal: 0},
			{Name: "BREF", FieldType: values.NotNullLong, Ordinal: 1},
			{Name: "ID", FieldType: values.NotNullLong, Ordinal: 2},
			{Name: "NEXT_ID", FieldType: values.NotNullLong, Ordinal: 3},
		},
		Legs: []values.RecordTypeLeg{
			values.NewRecordTypeLeg(
				values.LegKindFlatRun, values.NamedCorrelationIdentifier("B"), "B", 0, 2),
			values.NewRecordTypeLeg(
				values.LegKindFlatRun, values.NamedCorrelationIdentifier(tName(n)), tName(n), 2, 2),
		},
	}
	quants = append(quants, typedPartitionScanQuantifier(tName(n), boxTyp))
	aliases = append(aliases, tName(n))
	boxQOVValue, boxQOVErr := values.NewQuantifiedObjectValue(
		values.NamedCorrelationIdentifier(tName(n)), boxTyp)
	boxQOV := mustConstruct(t, boxQOVValue, boxQOVErr)
	legRoots[tName(n)] = boxQOV
	for col := range boxTyp.Fields {
		fv := mustOrdinalSeedField(t, boxQOV, col)
		fields = append(fields, values.RecordConstructorField{Name: fv.DisplayName(), Value: fv})
	}
	for i := 1; i < n; i++ {
		rightOrdinal := 0
		if i+1 == n {
			// The named rightmost leaf begins after the buried B run.
			rightOrdinal = 2
		}
		preds = append(preds, ordinalFieldEquality(
			t, legRoots[tName(i)], 1, legRoots[tName(i+1)], rightOrdinal))
	}
	seed := values.NewRawRecordConstructorValue(fields...)
	values.AssertOrdinalJoinSeed(seed)
	selectExpression, selectErr := expressions.NewSelectExpressionWithAliases(seed, quants, preds, aliases)
	return mustConstruct(t, selectExpression, selectErr)
}

// TestOrdinalBoxSeedChainConverges pins that a chain whose last leg carries a
// dissolved box's concat type (Legs boundaries) plans through the full
// pipeline. (Its former companion assertion — MergeArmHits == 0 — is now
// STRUCTURAL: the name-model re-enumeration arm was deleted along with the
// name-model producer, so the positional arm is the only merge dispatch that
// exists.)
func TestOrdinalBoxSeedChainConverges(t *testing.T) {
	t.Parallel()
	for _, n := range []int{2, 3} {
		ref := expressions.InitialOf(buildOrdinalBoxChainSelect(t, n))
		if _, tasks, err := fullChainPlanner().Plan(ref); err != nil {
			t.Errorf("%d-leg ORDINAL BOX chain did not plan: %v (tasks=%d)", n, err, tasks)
		}
	}
}

// TestExplorationRounds_EvidenceUnderCap is the observability half of the
// WS-P round-cap amendment ("evidence, not a silent raise"): the per-Ref
// exploration-round cap is 100 (unified_tasks.go maxRoundsPerRef), and the
// raise is only justified while REAL plans stay far under it. This pin
// PLANS the chain corpus and asserts the observed maximum keeps ≥2×
// headroom — if exploration ever creeps past 50 rounds, this fails before
// the cap starts silently truncating exploration in production.
func TestExplorationRounds_EvidenceUnderCap(t *testing.T) {
	t.Parallel()
	for _, n := range []int{2, 3, 4} {
		p := fullChainPlanner()
		ref := expressions.InitialOf(buildOrdinalChainSelect(t, n))
		if _, tasks, err := p.Plan(ref); err != nil {
			t.Fatalf("%d-leg chain did not plan: %v (tasks=%d)", n, err, tasks)
		}
		got := p.MaxObservedExplorationRounds()
		t.Logf("%d-leg chain: max observed exploration rounds = %d (cap 100)", n, got)
		if got < 1 {
			t.Errorf("%d-leg chain: observed %d rounds — the counter is not being populated", n, got)
		}
		if got > 50 {
			t.Errorf("%d-leg chain: observed %d rounds — creeping toward the 100-round cap; re-justify the cap before raising it", n, got)
		}
	}
}
