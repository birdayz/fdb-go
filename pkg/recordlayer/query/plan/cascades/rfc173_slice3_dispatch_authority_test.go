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
// maximal inner-join cluster (RFC-173 Slice 2/3). This is the SOLE seed shape
// since the name-model anchored producer's deletion (RFC-173 S4 item B);
// PartitionSelectRule routes its ≥2-live merges through the positional arm
// (positionalMergeCase) — structurally the only arm now. The chain corpus feeds
// the interning/task baselines and the leg-row-type derivations.
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
	// The standing W3b review condition: an ordinal seed that lands without
	// AssertOrdinalJoinSeed is a NAK on sight. This corpus asserts the same
	// pristine shape the translator guarantees.
	values.AssertOrdinalJoinSeed(seed)
	return expressions.NewSelectExpressionWithAliases(seed, quants, preds, aliases)
}

// buildOrdinalBoxChainSelect is buildOrdinalChainSelect with the LAST leg a
// dissolved-LEFT BOX: its QOV flows the box's CONCAT type (two buried tables'
// columns with RecordType.Legs boundaries, the RIGHT-normalized shape whose
// preserved leg names the box). The seed stays pristine (every column baked
// ordinal over its leg QOV, runs 0..width-1), so the values/executor twins
// accept it — the ordinal-seeded-box corpus (RFC-173 item 3 amendment I).
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

// TestRFC173Item3_OrdinalBoxSeedChainConverges pins that a chain whose last leg
// carries a dissolved box's concat type (Legs boundaries) plans through the
// full pipeline. (Its former companion assertion — MergeArmHits == 0, the
// Slice-3 dispatch-authority pin — is now STRUCTURAL: the anchored
// re-enumeration arm was deleted with the name-model producer, RFC-173 S4 item
// B, so the positional arm is the only merge dispatch that exists.)
func TestRFC173Item3_OrdinalBoxSeedChainConverges(t *testing.T) {
	t.Parallel()
	for _, n := range []int{2, 3} {
		ref := expressions.InitialOf(buildOrdinalBoxChainSelect(n))
		if _, tasks, err := fullChainPlanner().Plan(ref); err != nil {
			t.Errorf("%d-leg ORDINAL BOX chain did not plan: %v (tasks=%d)", n, err, tasks)
		}
	}
}
