package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// buildOrdinalChainSelect builds an n-table chain T1—T2—…—Tn, each link
// Ti.NEXT_ID = T(i+1).ID, SEEDED with the flat N-leg ORDINAL join RC — one
// values.NewFieldValueOfOrdinal per leg column over each leg's typed QOV, the
// exact shape the translator's buildOrdinalJoinResultValue produces for a gated
// maximal inner-join cluster. This is the SOLE seed shape the name-model
// producer has been retired in favor of; PartitionSelectRule routes its
// ≥2-live merges through the positional arm (positionalMergeCase) —
// structurally the only arm now. The chain corpus feeds the interning/task
// baselines and the leg-row-type derivations.
func buildOrdinalChainSelect(n int) *expressions.SelectExpression {
	var quants []expressions.Quantifier
	var aliases []string
	var preds []predicates.QueryPredicate
	var fields []values.RecordConstructorField
	for i := 1; i <= n; i++ {
		quants = append(quants, scanQuantifier(tName(i)))
		aliases = append(aliases, tName(i))
		legType := values.NewRecordType(tName(i), false, []values.Field{
			{Name: "ID", FieldType: values.NotNullLong, Ordinal: 0},
			{Name: "NEXT_ID", FieldType: values.NotNullLong, Ordinal: 1},
		})
		qov := values.NewQuantifiedObjectValueOfType(values.NamedCorrelationIdentifier(tName(i)), legType)
		for col := range legType.Fields {
			fv, err := values.NewFieldValueOfOrdinal(qov, col)
			if err != nil {
				// Impossible by construction (col ranges over the type's own
				// fields) — loud, matching the translator seed and the assert.
				panic("buildOrdinalChainSelect: " + err.Error())
			}
			fields = append(fields, values.RecordConstructorField{Name: fv.Field, Value: fv})
		}
	}
	for i := 1; i < n; i++ {
		preds = append(preds, chainEqPred(tName(i), "NEXT_ID", tName(i+1), "ID"))
	}
	seed := values.NewRawRecordConstructorValue(fields...)
	// An ordinal seed that lands without AssertOrdinalJoinSeed is a bug on
	// sight — this corpus asserts the same pristine shape the translator
	// guarantees.
	values.AssertOrdinalJoinSeed(seed)
	return expressions.NewSelectExpressionWithAliases(seed, quants, preds, aliases)
}

// buildOrdinalBoxChainSelect is buildOrdinalChainSelect with the LAST leg a
// dissolved-LEFT BOX: its QOV flows the box's CONCAT type (two buried tables'
// columns with RecordType.Legs boundaries, the RIGHT-normalized shape whose
// preserved leg names the box). The seed stays pristine (every column baked
// ordinal over its leg QOV, runs 0..width-1), so the values/executor twins
// accept it — the ordinal-seeded-box corpus.
func buildOrdinalBoxChainSelect(n int) *expressions.SelectExpression {
	var quants []expressions.Quantifier
	var aliases []string
	var preds []predicates.QueryPredicate
	var fields []values.RecordConstructorField
	for i := 1; i < n; i++ {
		quants = append(quants, scanQuantifier(tName(i)))
		aliases = append(aliases, tName(i))
		legType := values.NewRecordType(tName(i), false, []values.Field{
			{Name: "ID", FieldType: values.NotNullLong, Ordinal: 0},
			{Name: "NEXT_ID", FieldType: values.NotNullLong, Ordinal: 1},
		})
		qov := values.NewQuantifiedObjectValueOfType(values.NamedCorrelationIdentifier(tName(i)), legType)
		for col := range legType.Fields {
			fv, err := values.NewFieldValueOfOrdinal(qov, col)
			if err != nil {
				panic("buildOrdinalBoxChainSelect: " + err.Error())
			}
			fields = append(fields, values.RecordConstructorField{Name: fv.Field, Value: fv})
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
			{Name: "B", Start: 0, Width: 2},
			{Name: tName(n), Start: 2, Width: 2},
		},
	}
	quants = append(quants, scanQuantifier(tName(n)))
	aliases = append(aliases, tName(n))
	boxQOV := values.NewQuantifiedObjectValueOfType(values.NamedCorrelationIdentifier(tName(n)), boxTyp)
	for col := range boxTyp.Fields {
		fv, err := values.NewFieldValueOfOrdinal(boxQOV, col)
		if err != nil {
			panic("buildOrdinalBoxChainSelect: " + err.Error())
		}
		fields = append(fields, values.RecordConstructorField{Name: fv.Field, Value: fv})
	}
	for i := 1; i < n; i++ {
		preds = append(preds, chainEqPred(tName(i), "NEXT_ID", tName(i+1), "ID"))
	}
	seed := values.NewRawRecordConstructorValue(fields...)
	values.AssertOrdinalJoinSeed(seed)
	return expressions.NewSelectExpressionWithAliases(seed, quants, preds, aliases)
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
		ref := expressions.InitialOf(buildOrdinalBoxChainSelect(n))
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
		ref := expressions.InitialOf(buildOrdinalChainSelect(n))
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
