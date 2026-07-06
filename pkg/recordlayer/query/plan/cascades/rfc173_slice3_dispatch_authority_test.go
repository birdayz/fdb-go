package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// buildOrdinalChainSelect builds the ORDINAL-model analog of buildChainSelect:
// the same n-table chain T1—T2—…—Tn, each link Ti.NEXT_ID = T(i+1).ID, but
// SEEDED with the flat N-leg ORDINAL join RC — one values.NewFieldValueOfOrdinal
// per leg column over each leg's typed QOV, the exact shape the translator's
// buildOrdinalJoinResultValue produces for a gated maximal inner-join cluster
// (RFC-173 Slice 2/3) — instead of the name-model NewAnchoredJoinRecord that
// buildChainSelect seeds. An ordinal seed is a RAW (non-AnchoredJoin) RC, so
// isAnchoredJoinResult reports parentIsMerge=false and PartitionSelectRule
// routes the ≥2-live merge to the POSITIONAL arm (positionalMergeCase), never
// the anchored re-enumeration arm. This is the corpus the Slice-3 dispatch-
// authority pin certifies: the positional arm is the SOLE producer for it.
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

// planMergeArmHits plans sel through the full SQL pipeline and returns how many
// times PartitionSelectRule took the anchored re-enumeration arm
// (Memo.MergeArmHits) plus the total task count.
func planMergeArmHits(t *testing.T, sel *expressions.SelectExpression) (hits, tasks int) {
	t.Helper()
	ref := expressions.InitialOf(sel)
	planner := fullChainPlanner()
	_, tasks, err := planner.Plan(ref)
	if err != nil {
		t.Fatalf("Plan: %v (tasks=%d)", err, tasks)
	}
	return planner.Memo().MergeArmHits(), tasks
}

// TestRFC173S3_OrdinalSeedDispatchAuthority is the RFC-173 Slice-3
// DISPATCH-AUTHORITY pin (the load-bearing certification of the reframed
// slice). The flip makes the positional merge arm the SOLE producer for every
// ordinal-seeding shape; the anchored re-enumeration arm survives only for the
// name-model RESIDUAL (scalar-subquery / multi-source-unnest seeds), retired
// with the trio in Slice 4. This pins the boundary two ways:
//
//   - CONTROL: the name-model ANCHORED chain MUST hit the anchored arm
//     (MergeArmHits > 0). Without this the ordinal assertion below could pass
//     vacuously against a counter that never fires — a bare "must-not-regress"
//     against a dead counter is a vibe, not a gate.
//   - PIN: the ORDINAL chain must take the anchored arm ZERO times. A non-zero
//     count means an ordinal seed leaked into the name-model dispatch — the
//     authority the flip establishes is broken.
//
// Both drive the SAME chain topology through the SAME full planner as the
// task-count baseline (TestPartitionSelect_ChainInterningBaseline), so a
// dispatch regression and an interning regression are caught on the same corpus
// by complementary metrics (this pin: which arm; that pin: how many tasks).
func TestRFC173S3_OrdinalSeedDispatchAuthority(t *testing.T) {
	t.Parallel()

	// CONTROL — the anchored (name-model) seed exercises the anchored arm.
	for _, n := range []int{3, 4} {
		anchoredHits, _ := planMergeArmHits(t, buildChainSelect(n))
		if anchoredHits == 0 {
			t.Fatalf("%d-table ANCHORED chain: MergeArmHits=0 — the counter never fired; "+
				"the ordinal assertion below would be vacuously green", n)
		}
		t.Logf("control: %d-table ANCHORED chain took the anchored arm %d times", n, anchoredHits)
	}

	// PIN — the ordinal seed routes wholly through the positional arm.
	for _, n := range []int{2, 3, 4} {
		ordinalHits, _ := planMergeArmHits(t, buildOrdinalChainSelect(n))
		if ordinalHits != 0 {
			t.Errorf("%d-table ORDINAL chain: MergeArmHits=%d, want 0 — an ordinal seed "+
				"leaked into the anchored (name-model) dispatch arm; the positional arm "+
				"must be the SOLE producer for ordinal-seeding shapes (RFC-173 Slice 3)", n, ordinalHits)
		}
		t.Logf("pin: %d-table ORDINAL chain took the anchored arm %d times (want 0)", n, ordinalHits)
	}
}

// buildOrdinalBoxChainSelect is buildOrdinalChainSelect with the LAST leg a
// dissolved-LEFT BOX: its QOV flows the box's CONCAT type (two buried tables'
// columns with RecordType.Legs boundaries, the RIGHT-normalized shape whose
// preserved leg names the box). The seed stays pristine (every column baked
// ordinal over its leg QOV, runs 0..width-1), so the values/executor twins
// accept it — the exact ordinal-seeded-box corpus amendment I certifies must
// NEVER reach the anchored re-enumeration arm (the RIGHT-variant panic class:
// NewReEnumerationAnchoredRecord over a positional box row).
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

// TestRFC173Item3_OrdinalBoxSeedDispatchAuthority extends the Slice-3
// dispatch-authority corpus with the ordinal-seeded BOX shape (amendment I):
// a chain whose last leg carries a dissolved box's concat type (Legs
// boundaries) must route wholly through the positional merge arm —
// MergeArmHits == 0 — because the anchored re-enumeration arm cannot anchor a
// positional box row (the RIGHT-variant panic at the re-enumeration RC).
func TestRFC173Item3_OrdinalBoxSeedDispatchAuthority(t *testing.T) {
	t.Parallel()
	for _, n := range []int{2, 3} {
		hits, _ := planMergeArmHits(t, buildOrdinalBoxChainSelect(n))
		if hits != 0 {
			t.Errorf("%d-leg ORDINAL BOX chain: MergeArmHits=%d, want 0 — an ordinal-seeded box "+
				"leaked into the anchored (name-model) dispatch arm (amendment I)", n, hits)
		}
	}
}
