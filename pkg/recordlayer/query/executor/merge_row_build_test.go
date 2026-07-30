package executor

import (
	"testing"

	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

// Pins the NESTED positional merge-row build. PartitionBinarySelectRule
// lowers an N-way join into nested binary joins; the intermediate merge
// level's result value is a record OF records (`_i` = leg i's WHOLE row,
// mirroring Java's Column.unnamedOf), unlike a plain ordinal-join seed's flat
// inline concat. The fixtures hand-build the two physical shapes the rule
// produces: the lowest merge level (ALL bare `_i` QOVs, no baked refs) and a
// translated MIXED upper (ofOrdinal refs over the inner merge alongside a
// bare leg QOV).

// s3MergeRC builds RC(_0: QOV(a), _1: QOV(b)) — the lowest merge level.
func s3MergeRC(qovA, qovB *values.QuantifiedObjectValue) *values.RecordConstructorValue {
	return values.NewRawRecordConstructorValue(
		values.RecordConstructorField{Name: values.OrdinalFieldName(0), Value: qovA},
		values.RecordConstructorField{Name: values.OrdinalFieldName(1), Value: qovB},
	)
}

// TestMergeBuild_Constructor pins the second build trigger: the
// all-bare positional-merge RC enables the build (no baked refs anywhere —
// ContainsBakedOrdinal is false, so the plain baked-ordinal trigger alone
// would miss it), WindowsOK is false (no flat spans), and LegTypes come from
// the bare QOVs' flowed types. A plain named RC of QOVs (not `_i`-named)
// stays name-model.
func TestMergeBuild_Constructor(t *testing.T) {
	t.Parallel()
	legA, legB, qovA, qovB, _ := ojWiringLegs(t)

	build, err := newOrdinalJoinBuild(s3MergeRC(qovA, qovB), nil)
	if err != nil {
		t.Fatalf("merge-RC build: %v", err)
	}
	if !build.enabled() || build.WindowsOK {
		t.Fatalf("merge RC must build WITHOUT windows (enabled=%v windows=%v)", build.enabled(), build.WindowsOK)
	}
	// Structural compare — qov.Type() returns a nullability-wrapped instance,
	// not the raw pointer.
	gotA, gotB := build.LegTypes[qovA.Correlation], build.LegTypes[qovB.Correlation]
	if gotA == nil || gotB == nil || len(gotA.Fields) != len(legA.Fields) || len(gotB.Fields) != len(legB.Fields) ||
		gotA.Fields[1].Name != "V" || gotB.Fields[1].Name != "W" {
		t.Fatalf("LegTypes must come from the bare QOVs' flowed types, got %v", build.LegTypes)
	}
	if len(build.OutputType.Fields) != 2 || build.OutputType.Fields[0].Name != "_0" {
		t.Fatalf("OutputType = %v, want the 2-slot nested merge type", build.OutputType)
	}

	// A NAMED RC of QOVs is a projection of whole rows, not the merge shape —
	// no planner rule produces it as a join level, so it must stay name-model.
	named := values.NewRecordConstructorValue(
		values.RecordConstructorField{Name: "LEFT", Value: qovA},
		values.RecordConstructorField{Name: "RIGHT", Value: qovB},
	)
	if b, err := newOrdinalJoinBuild(named, nil); err != nil || b.enabled() {
		t.Fatalf("named QOV RC must stay name-model, got (%v, %v)", b, err)
	}
}

// TestMergeBuild_NLJCursor_Nested drives the merge RC through the
// real NLJ cursor: each emitted row's Positional slot `_i` holds leg i's own
// nested positional row, and a fused two-step reference (the shape
// PartitionBinarySelectRule's TranslationMap produces for a buried column
// once its leg has been merged away) reads through it end-to-end.
func TestMergeBuild_NLJCursor_Nested(t *testing.T) {
	t.Parallel()
	legA, legB, qovA, qovB, _ := ojWiringLegs(t)

	outerRows := []QueryResult{
		ojLegQR(t, legA, int64(1), int64(10)),
		ojLegQR(t, legA, int64(2), int64(20)),
	}
	innerRows := []QueryResult{
		ojLegQR(t, legB, int64(1), int64(100)),
	}
	pred := ojEqPred(
		values.NewCorrelatedFieldValueWithResolvedOrdinal(qovA, "ID", 0, values.NotNullLong),
		values.NewCorrelatedFieldValueWithResolvedOrdinal(qovB, "ID", 0, values.NotNullLong),
	)
	c := mustNLJCursor(t, recordlayer.FromList(outerRows), innerRows, plans.JoinInner,
		values.NamedCorrelationIdentifier("A"), values.NamedCorrelationIdentifier("B"), []predicates.QueryPredicate{pred}, s3MergeRC(qovA, qovB), EmptyEvaluationContext(), nil)
	defer c.Close()
	results := collectCursor(t, c)
	if len(results) != 1 {
		t.Fatalf("got %d rows, want 1 (A.ID=1 ⋈ B.ID=1)", len(results))
	}

	// The nested positional row: slot i is leg i's WHOLE positional row.
	pos := results[0].Positional
	if pos == nil || len(pos.Slots) != 2 {
		t.Fatalf("Positional = %v, want the 2-slot nested merge row", pos)
	}
	legARow, isRow := pos.Slots[0].(values.OrdinalRow)
	if !isRow {
		t.Fatalf("slot 0 = %T, want leg A's positional row (nested, not flat)", pos.Slots[0])
	}
	if v, ok := legARow.Get(1); !ok || v != int64(10) {
		t.Fatalf("nested leg A slot 1 = (%v, %v), want (10, true)", v, ok)
	}

	// A fused two-step reference over the merge quantifier — the exact shape
	// TranslationMap produces for a buried `a.V` reference — evaluates
	// through the nested row.
	mergedType := values.NewRecordType("", false, []values.Field{
		{Name: values.OrdinalFieldName(0), FieldType: legA, Ordinal: 0},
		{Name: values.OrdinalFieldName(1), FieldType: legB, Ordinal: 1},
	})
	upperQOV := values.NewQuantifiedObjectValueOfType(values.NamedCorrelationIdentifier("m"), mergedType)
	step0, err := values.NewFieldValueOfOrdinal(upperQOV, 0)
	if err != nil {
		t.Fatalf("bake _0: %v", err)
	}
	fused, err := values.NewFieldValueOfOrdinal(step0, 1) // _0#0.V#1, fused by SimplifyValue below
	if err != nil {
		t.Fatalf("bake V over _0: %v", err)
	}
	twoStep := values.SimplifyValue(fused) // compose rule fuses the baked chain
	got, err := twoStep.Evaluate(&values.RowEvalContext{Correlations: &ordinalBindingStub{id: upperQOV.Correlation, row: pos}})
	if err != nil || got != int64(10) {
		t.Fatalf("fused two-step read over the nested merge row = (%v, %v), want (10, nil)", got, err)
	}
}

// TestMergeBuild_MixedUpper pins the translated MIXED upper shape:
// RC(_0: ofOrdinal(innerMerge, 0), _1: ofOrdinal(innerMerge, 1), _2: QOV(c))
// — baked refs flatten the inner nesting one level, the bare leg rides
// whole. Exercised at the primitive level (evaluateOrdinalJoinRow over a
// two-leg binder) — the exact evaluation the cursor performs for this shape.
func TestMergeBuild_MixedUpper(t *testing.T) {
	t.Parallel()
	legA, legB, _, _, _ := ojWiringLegs(t)
	legC := values.NewRecordType("", false, []values.Field{
		{Name: "X", FieldType: values.NotNullLong, Ordinal: 0},
	})
	innerMergedType := values.NewRecordType("", false, []values.Field{
		{Name: values.OrdinalFieldName(0), FieldType: legA, Ordinal: 0},
		{Name: values.OrdinalFieldName(1), FieldType: legB, Ordinal: 1},
	})
	innerQOV := values.NewQuantifiedObjectValueOfType(values.NamedCorrelationIdentifier("m"), innerMergedType)
	qovC := values.NewQuantifiedObjectValueOfType(values.NamedCorrelationIdentifier("c"), legC)

	f0, err := values.NewFieldValueOfOrdinal(innerQOV, 0)
	if err != nil {
		t.Fatalf("bake _0: %v", err)
	}
	f1, err := values.NewFieldValueOfOrdinal(innerQOV, 1)
	if err != nil {
		t.Fatalf("bake _1: %v", err)
	}
	mixed := values.NewRawRecordConstructorValue(
		values.RecordConstructorField{Name: values.OrdinalFieldName(0), Value: f0},
		values.RecordConstructorField{Name: values.OrdinalFieldName(1), Value: f1},
		values.RecordConstructorField{Name: values.OrdinalFieldName(2), Value: qovC},
	)

	build, err := newOrdinalJoinBuild(mixed, nil)
	if err != nil || !build.enabled() || build.WindowsOK {
		t.Fatalf("mixed upper build = (%v, %v), want enabled without windows", build, err)
	}
	// The bare leg's type must be collected from the QOV directly, not only
	// from baked references. Structural compare (nullability wrapping).
	if gotC := build.LegTypes[qovC.Correlation]; gotC == nil || len(gotC.Fields) != 1 || gotC.Fields[0].Name != "X" {
		t.Fatalf("bare-QOV leg type missing: %v", build.LegTypes)
	}

	// Evaluate over the two bindings: the inner merge's nested row + c's row.
	legARow := &fakeExecOrdinalRow{names: []string{"ID", "V"}, slots: []any{int64(1), int64(10)}}
	legBRow := &fakeExecOrdinalRow{names: []string{"ID", "W"}, slots: []any{int64(1), int64(100)}}
	innerRow := &fakeExecOrdinalRow{names: []string{"_0", "_1"}, slots: []any{legARow, legBRow}}
	cRow := &fakeExecOrdinalRow{names: []string{"X"}, slots: []any{int64(7)}}
	binder := &twoLegBinder{
		outerID: innerQOV.Correlation, innerID: qovC.Correlation,
		outer: innerRow, inner: cRow,
	}
	pos, err := evaluateOrdinalJoinRow(mixed, build.OutputType, binder)
	if err != nil {
		t.Fatalf("mixed-upper row build: %v", err)
	}
	// _0/_1 flatten the inner nesting one level; _2 is c's whole row.
	if pos.Slots[0] != values.OrdinalRow(legARow) || pos.Slots[1] != values.OrdinalRow(legBRow) {
		t.Fatalf("translated slots must flatten one nesting level: %v", pos.Slots)
	}
	if pos.Slots[2] != values.OrdinalRow(cRow) {
		t.Fatalf("bare leg slot = %v, want c's whole row", pos.Slots[2])
	}
}

// TestMergeBuild_FlatMapBuilds pins the merge row build over the FlatMap
// (correlated) path: the all-bare merge RC builds there too, because
// PartitionBinarySelectRule's merge arm produces live merge-RC FlatMaps whose
// inner plans carry baked leg SARGs — those baked operands need a positional
// binding (a name-keyed one would die loud with BakedNameContextError at
// scan-range build). Each emitted row's Positional slot `_i` is leg i's own
// positional row, nested rather than flattened.
func TestMergeBuild_FlatMapBuilds(t *testing.T) {
	t.Parallel()
	legA, legB, qovA, qovB, _ := ojWiringLegs(t)
	c, err := newFlatMapCursor(
		recordlayer.FromList([]QueryResult{}), nil, nil, nil, EmptyEvaluationContext(),
		qovA.Correlation, qovB.Correlation,
		s3MergeRC(qovA, qovB), recordlayer.ExecuteProperties{},
	)
	if err != nil {
		t.Fatalf("newFlatMapCursor: %v", err)
	}
	defer c.Close()
	if !c.build.enabled() || c.build.WindowsOK {
		t.Fatalf("the merge RC must build on the FlatMap path without windows (enabled=%v windows=%v)", c.build.enabled(), c.build.WindowsOK)
	}

	outerQR := ojLegQR(t, legA, int64(1), int64(10))
	innerQR := ojLegQR(t, legB, int64(2), int64(20))

	t.Run("both legs", func(t *testing.T) {
		t.Parallel()
		got, err := c.computeResult(outerQR, innerQR)
		if err != nil {
			t.Fatalf("computeResult: %v", err)
		}
		// Nested positional row: slot i is leg i's WHOLE positional row.
		if got.Positional == nil || len(got.Positional.Slots) != 2 {
			t.Fatalf("Positional = %v, want the 2-slot nested merge row", got.Positional)
		}
		legRow, isRow := got.Positional.Slots[0].(values.OrdinalRow)
		if !isRow {
			t.Fatalf("slot 0 = %T, want leg A's positional row", got.Positional.Slots[0])
		}
		if v, ok := legRow.Get(1); !ok || v != int64(10) {
			t.Fatalf("nested leg A slot 1 = (%v, %v), want (10, true)", v, ok)
		}
	})

	t.Run("nil inner is the null leg", func(t *testing.T) {
		t.Parallel()
		got, err := c.computeResultLegs(outerQR, nil)
		if err != nil {
			t.Fatalf("computeResultLegs: %v", err)
		}
		if got.Positional == nil || len(got.Positional.Slots) != 2 || got.Positional.Slots[1] != nil {
			t.Fatalf("Positional = %v, want [legA-row, nil]", got.Positional)
		}
	})
}

// ordinalBindingStub binds one correlation to one positional row.
type ordinalBindingStub struct {
	id  values.CorrelationIdentifier
	row values.OrdinalRow
}

func (s *ordinalBindingStub) GetCorrelationBinding(id values.CorrelationIdentifier) (any, bool) {
	if id == s.id {
		return s.row, true
	}
	return nil, false
}

// fakeExecOrdinalRow is a minimal OrdinalRow for hand-built nested fixtures.
type fakeExecOrdinalRow struct {
	names []string
	slots []any
}

func (r *fakeExecOrdinalRow) Get(ord int) (any, bool) {
	if ord < 0 || ord >= len(r.slots) {
		return nil, false
	}
	return r.slots[ord], true
}

// s3FusedRef builds the fused two-step reference TranslationMap's rebuild
// produces for a buried leg column: ofOrdinal(merge, slot) composed with the
// leg-local ordinal (SimplifyValue fires the compose/fuse arm).
func s3FusedRef(t *testing.T, mergeQOV *values.QuantifiedObjectValue, slot, legOrd int) *values.FieldValue {
	t.Helper()
	step0, err := values.NewFieldValueOfOrdinal(mergeQOV, slot)
	if err != nil {
		t.Fatalf("bake _%d: %v", slot, err)
	}
	inner, err := values.NewFieldValueOfOrdinal(step0, legOrd)
	if err != nil {
		t.Fatalf("bake leg ordinal %d over _%d: %v", legOrd, slot, err)
	}
	fused, isFV := values.SimplifyValue(inner).(*values.FieldValue)
	if !isFV || fused.Resolved == nil || len(fused.Resolved.Accessors) != 2 {
		t.Fatalf("compose must fuse into a two-accessor path, got %T", values.SimplifyValue(inner))
	}
	return fused
}

// TestTranslatedTopSpans pins span recovery for a TRANSLATED top result
// value — the shape PartitionBinarySelectRule leaves behind after merging
// some legs away: pinned single-accessor refs over the remaining leg mixed
// with FUSED two-step refs over the merge quantifier. The merged-away legs'
// aliases survive only in the merge quantifier's child RV (the positional-merge
// RC), so the probe resolves through legRVs; without it (the plain seed probe,
// no legRVs) the shape must decline — fail-safe, never mis-windowed.
func TestTranslatedTopSpans(t *testing.T) {
	t.Parallel()
	_, legB, qovA, qovB, _ := ojWiringLegs(t)
	legC := values.NewRecordType("", false, []values.Field{
		{Name: "X", FieldType: values.NotNullLong, Ordinal: 0},
	})
	qovC := values.NewQuantifiedObjectValueOfType(values.NamedCorrelationIdentifier("C"), legC)

	mergedType := values.NewRecordType("", false, []values.Field{
		{Name: values.OrdinalFieldName(0), FieldType: legB, Ordinal: 0},
		{Name: values.OrdinalFieldName(1), FieldType: legC, Ordinal: 1},
	})
	mergeQOV := values.NewQuantifiedObjectValueOfType(values.NamedCorrelationIdentifier("m"), mergedType)

	a0, err := values.NewFieldValueOfOrdinal(qovA, 0)
	if err != nil {
		t.Fatalf("bake A#0: %v", err)
	}
	a1, err := values.NewFieldValueOfOrdinal(qovA, 1)
	if err != nil {
		t.Fatalf("bake A#1: %v", err)
	}
	top := values.NewRawRecordConstructorValue(
		values.RecordConstructorField{Name: "ID", Value: a0},
		values.RecordConstructorField{Name: "V", Value: a1},
		values.RecordConstructorField{Name: "ID", Value: s3FusedRef(t, mergeQOV, 0, 0)},
		values.RecordConstructorField{Name: "W", Value: s3FusedRef(t, mergeQOV, 0, 1)},
		values.RecordConstructorField{Name: "X", Value: s3FusedRef(t, mergeQOV, 1, 0)},
	)

	// The plain seed probe (no legRVs) must DECLINE the fused shape.
	if _, _, ok := ordinalJoinSpansOf(top, nil); ok {
		t.Fatal("fused refs must decline without legRVs (no alias to resolve through)")
	}

	legRVs := map[values.CorrelationIdentifier]values.Value{
		mergeQOV.Correlation: s3MergeRC(qovB, qovC),
	}
	spans, _, ok := ordinalJoinSpansOf(top, legRVs)
	if !ok {
		t.Fatal("translated top must yield spans through the merge RC")
	}
	want := []struct {
		alias         string
		offset, width int
	}{{"A", 0, 2}, {"B", 2, 2}, {"C", 4, 1}}
	if len(spans) != len(want) {
		t.Fatalf("got %d spans %+v, want %d", len(spans), spans, len(want))
	}
	for i, w := range want {
		s := spans[i]
		if s.Alias.Name() != w.alias || s.Offset != w.offset || s.Width != w.width {
			t.Fatalf("span %d = {%s %d %d}, want {%s %d %d}", i, s.Alias.Name(), s.Offset, s.Width, w.alias, w.offset, w.width)
		}
		if s.LegType == nil || len(s.LegType.Fields) != w.width {
			t.Fatalf("span %d leg type = %v, want the LEAF leg's own %d-field type", i, s.LegType, w.width)
		}
	}
	// The recovered windows resolve a dotted read the way spanAwareRow will:
	// leg-local name against the LEAF type (the whole point of the recovery —
	// "B.W" over the flat top row).
	if idx, found := spans[1].LegType.FieldIndex("W"); !found || idx != 1 {
		t.Fatalf("leg B window must resolve W leg-locally (got %d, %v)", idx, found)
	}
}

// TestSpliceLegSpans pins the recursive box splice: a span whose leg
// is itself a join plan's output (a FULL box's gated-join leg — an anonymous
// flat concat named after its buried rightmost table) is replaced by the
// leg's OWN spans offset into the parent row, exposing the LEAF table aliases
// dotted upper references actually name. The width guard keeps a
// non-tiling (shadowed-alias) splice opaque instead of mis-windowed.
func TestSpliceLegSpans(t *testing.T) {
	t.Parallel()
	_, legB, _, _, boxSeed := ojWiringLegs(t)
	legC := values.NewRecordType("", false, []values.Field{
		{Name: "X", FieldType: values.NotNullLong, Ordinal: 0},
	})
	qovC := values.NewQuantifiedObjectValueOfType(values.NamedCorrelationIdentifier("C"), legC)

	// The box leg: alias "B" (sourceAlias names the box after its rightmost
	// leaf — the shadowing case), type = the flat {A,B} concat. RAW
	// construction: duplicate names across concat slots are legal.
	boxType := &values.RecordType{Fields: []values.Field{
		{Name: "ID", FieldType: values.NotNullLong, Ordinal: 0},
		{Name: "V", FieldType: values.NotNullLong, Ordinal: 1},
		{Name: "ID", FieldType: values.NotNullLong, Ordinal: 2},
		{Name: "W", FieldType: values.NotNullLong, Ordinal: 3},
	}}
	boxQOV := values.NewQuantifiedObjectValueOfType(values.NamedCorrelationIdentifier("B"), boxType)
	top := buildOrdinalJoinRC(t, qovC, boxQOV)

	legRVs := map[values.CorrelationIdentifier]values.Value{
		boxQOV.Correlation: boxSeed, // the box's own seed over legs A, B
	}
	spans, _, ok := ordinalJoinSpansOf(top, legRVs)
	if !ok {
		t.Fatal("pristine seed over [C, box] must yield spans")
	}
	spliced := spliceLegSpans(spans, legRVs)
	want := []struct {
		alias         string
		offset, width int
	}{{"C", 0, 1}, {"A", 1, 2}, {"B", 3, 2}}
	if len(spliced) != len(want) {
		t.Fatalf("got %d spliced spans %+v, want %d", len(spliced), spliced, len(want))
	}
	for i, w := range want {
		s := spliced[i]
		if s.Alias.Name() != w.alias || s.Offset != w.offset || s.Width != w.width {
			t.Fatalf("spliced span %d = {%s %d %d}, want {%s %d %d}", i, s.Alias.Name(), s.Offset, s.Width, w.alias, w.offset, w.width)
		}
	}
	// The LEAF B span resolves leg-locally against table B's own type — and
	// its own legRVs entry (the shadowed box alias) does NOT re-splice it:
	// table B's width (2) never tiles the box RV's total (4).
	if len(spliced[2].LegType.Fields) != len(legB.Fields) {
		t.Fatalf("leaf B span type = %v, want table B's own type", spliced[2].LegType)
	}
}

// TestHashJoinDeclinesFusedPred pins a correctness trap in the hash-join
// fast path: an upper NLJ whose inner leg is a materialized MERGE select
// carries preds like `a.id = m._0.id`. Naively, the equijoin extractor could
// read the fused ref's display name as a plain "M.ID" hash key, probe the
// merge-shaped rows (which carry only `_i` slots) for "ID", build an EMPTY
// index, and have the ≥100-row fast path silently drop every match — while
// the linear path (and the slow evaluation the index is supposed to
// accelerate) resolves the fused reference correctly. Fused refs must
// decline hash extraction.
func TestHashJoinDeclinesFusedPred(t *testing.T) {
	t.Parallel()
	legA, legB, qovA, _, _ := ojWiringLegs(t)
	legC := values.NewRecordType("", false, []values.Field{
		{Name: "X", FieldType: values.NotNullLong, Ordinal: 0},
	})
	mergedType := values.NewRecordType("", false, []values.Field{
		{Name: values.OrdinalFieldName(0), FieldType: legB, Ordinal: 0},
		{Name: values.OrdinalFieldName(1), FieldType: legC, Ordinal: 1},
	})
	mergeQOV := values.NewQuantifiedObjectValueOfType(values.NamedCorrelationIdentifier("M"), mergedType)

	// 150 merge-shaped inner rows (≥100 arms the hash index): each holds a
	// nested positional row keyed `_0`/`_1`, one slot per leg. Leg B's
	// ID runs 0..149, so every outer row finds exactly one match.
	innerRows := make([]QueryResult, 150)
	for i := range innerRows {
		legBRow := &fakeExecOrdinalRow{names: []string{"ID", "W"}, slots: []any{int64(i), int64(i * 10)}}
		legCRow := &fakeExecOrdinalRow{names: []string{"X"}, slots: []any{int64(i)}}
		pos := NewPositionalRow(mergedType)
		pos.Set(0, legBRow)
		pos.Set(1, legCRow)
		innerRows[i] = QueryResult{
			Positional: pos,
		}
	}
	outerRows := []QueryResult{
		ojLegQR(t, legA, int64(3), int64(30)),
		ojLegQR(t, legA, int64(7), int64(70)),
	}

	// a.id = m._0.id — the fused two-step rebase shape; the MIXED upper RV
	// (baked over both legs) makes this a real ordinal-build cursor.
	aRef, err := values.NewFieldValueOfOrdinal(qovA, 0)
	if err != nil {
		t.Fatalf("bake A#0: %v", err)
	}
	mixed := values.NewRawRecordConstructorValue(
		values.RecordConstructorField{Name: "AID", Value: aRef},
		values.RecordConstructorField{Name: "BID", Value: s3FusedRef(t, mergeQOV, 0, 0)},
	)
	pred := ojEqPred(
		values.NewCorrelatedFieldValueWithResolvedOrdinal(qovA, "ID", 0, values.NotNullLong),
		s3FusedRef(t, mergeQOV, 0, 0),
	)
	c := mustNLJCursor(t, recordlayer.FromList(outerRows), innerRows, plans.JoinInner,
		values.NamedCorrelationIdentifier("A"), values.NamedCorrelationIdentifier("M"), []predicates.QueryPredicate{pred}, mixed, EmptyEvaluationContext(), nil)
	defer c.Close()
	results := collectCursor(t, c)
	if len(results) != 2 {
		t.Fatalf("got %d rows, want 2 (a.id 3 and 7 each match one merge row) — a hash index keyed on the fused ref's display name drops them all", len(results))
	}
}
