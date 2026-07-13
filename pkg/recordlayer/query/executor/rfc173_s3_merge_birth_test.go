package executor

import (
	"reflect"
	"testing"

	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

// RFC-173 S3-W2 commit-2 pins — the NESTED positional merge-row birth. The
// partition rule's merge shape is a record OF records (`_i` = leg i's WHOLE
// row, Java Column.unnamedOf), unlike the S2 flat seed's inline concat.
// DARK: nothing produces these RCs until the fulcrum; the fixtures hand-build
// the two post-fulcrum physical shapes: the lowest merge level (ALL bare
// `_i` QOVs, no baked refs) and the translated MIXED upper (ofOrdinal refs
// over the inner merge alongside a bare leg QOV).

// s3MergeRC builds RC(_0: QOV(a), _1: QOV(b)) — the lowest merge level.
func s3MergeRC(qovA, qovB *values.QuantifiedObjectValue) *values.RecordConstructorValue {
	return values.NewRawRecordConstructorValue(
		values.RecordConstructorField{Name: values.OrdinalFieldName(0), Value: qovA},
		values.RecordConstructorField{Name: values.OrdinalFieldName(1), Value: qovB},
	)
}

// TestRFC173S3_MergeBirth_Constructor pins the second birth trigger: the
// all-bare positional-merge RC enables the birth (no baked refs anywhere —
// ContainsBakedOrdinal is false, the S2-only trigger missed it), WindowsOK
// is false (no flat spans), and LegTypes come from the bare QOVs' flowed
// types. A plain named RC of QOVs (not `_i`-named) stays name-model.
func TestRFC173S3_MergeBirth_Constructor(t *testing.T) {
	t.Parallel()
	legA, legB, qovA, qovB, _ := ojWiringLegs(t)

	birth, err := newOrdinalJoinBirth(s3MergeRC(qovA, qovB), nil)
	if err != nil {
		t.Fatalf("merge-RC birth: %v", err)
	}
	if !birth.enabled() || birth.WindowsOK {
		t.Fatalf("merge RC must birth WITHOUT windows (enabled=%v windows=%v)", birth.enabled(), birth.WindowsOK)
	}
	// Structural compare — qov.Type() returns a nullability-wrapped instance,
	// not the raw pointer.
	gotA, gotB := birth.LegTypes[qovA.Correlation], birth.LegTypes[qovB.Correlation]
	if gotA == nil || gotB == nil || len(gotA.Fields) != len(legA.Fields) || len(gotB.Fields) != len(legB.Fields) ||
		gotA.Fields[1].Name != "V" || gotB.Fields[1].Name != "W" {
		t.Fatalf("LegTypes must come from the bare QOVs' flowed types, got %v", birth.LegTypes)
	}
	if len(birth.OutputType.Fields) != 2 || birth.OutputType.Fields[0].Name != "_0" {
		t.Fatalf("OutputType = %v, want the 2-slot nested merge type", birth.OutputType)
	}

	// A NAMED RC of QOVs is a projection of whole rows, not the merge shape:
	// no birth (dark for everything the planner produces today).
	named := values.NewRecordConstructorValue(
		values.RecordConstructorField{Name: "LEFT", Value: qovA},
		values.RecordConstructorField{Name: "RIGHT", Value: qovB},
	)
	if b, err := newOrdinalJoinBirth(named, nil); err != nil || b.enabled() {
		t.Fatalf("named QOV RC must stay name-model, got (%v, %v)", b, err)
	}
}

// TestRFC173S3_MergeBirth_NLJCursor_Nested drives the merge RC through the
// real NLJ cursor: the emitted rows carry the MERGE-SHAPE Datum (slot `_i` =
// leg i's own Datum — the fulcrum's settlement, superseding the pre-fulcrum
// untouched-mergeRows expectation: the partition rule rebases every upper
// reference through the merge quantifier, so mergeRows' flat keys silently
// NULLed them all on the §5 oracle side) plus a nested Positional row — slot
// i IS leg i's positional row — and a fused two-step reference (commit 1's
// TranslationMap output shape) reads through it end-to-end.
func TestRFC173S3_MergeBirth_NLJCursor_Nested(t *testing.T) {
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
		values.NewFieldValue(qovA, "ID", values.NotNullLong),
		values.NewFieldValue(qovB, "ID", values.NotNullLong),
	)
	c := mustNLJCursor(t, recordlayer.FromList(outerRows), innerRows, plans.JoinInner,
		"A", "B", []predicates.QueryPredicate{pred}, s3MergeRC(qovA, qovB), EmptyEvaluationContext(), nil)
	defer c.Close()
	results := collectCursor(t, c)
	if len(results) != 1 {
		t.Fatalf("got %d rows, want 1 (A.ID=1 ⋈ B.ID=1)", len(results))
	}

	// The merge-shape Datum: per-leg Datum maps under the `_i` keys — what
	// the rebased upper references (and the §5 oracle) read.
	wantDatum := map[string]any{
		"_0": map[string]any{"ID": int64(1), "V": int64(10)},
		"_1": map[string]any{"ID": int64(1), "W": int64(100)},
	}
	if !reflect.DeepEqual(results[0].Datum, wantDatum) {
		t.Fatalf("Datum = %#v, want the merge-shape per-leg maps %#v", results[0].Datum, wantDatum)
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
	// commit 1's TranslationMap produces for a buried `a.V` — evaluates
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
	fused, err := values.NewFieldValueOfOrdinal(step0, 1) // _0#0.V#1 — fuses via the rebuild arm at construction? No: constructor over a FieldValue child
	if err != nil {
		t.Fatalf("bake V over _0: %v", err)
	}
	twoStep := values.SimplifyValue(fused) // compose rule fuses the baked chain
	got, err := twoStep.Evaluate(&values.RowEvalContext{Correlations: &ordinalBindingStub{id: upperQOV.Correlation, row: pos}})
	if err != nil || got != int64(10) {
		t.Fatalf("fused two-step read over the nested merge row = (%v, %v), want (10, nil)", got, err)
	}
}

// TestRFC173S3_MergeBirth_MixedUpper pins the translated MIXED upper shape:
// RC(_0: ofOrdinal(innerMerge, 0), _1: ofOrdinal(innerMerge, 1), _2: QOV(c))
// — baked refs flatten the inner nesting one level, the bare leg rides
// whole. Exercised at the primitive level (evaluateOrdinalJoinRow over a
// two-leg binder), the exact evaluation the cursor performs post-fulcrum.
func TestRFC173S3_MergeBirth_MixedUpper(t *testing.T) {
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

	birth, err := newOrdinalJoinBirth(mixed, nil)
	if err != nil || !birth.enabled() || birth.WindowsOK {
		t.Fatalf("mixed upper birth = (%v, %v), want enabled without windows", birth, err)
	}
	// The bare leg's type must be collected from the QOV (review-shaped gap:
	// only baked refs contributed before). Structural compare (nullability
	// wrapping).
	if gotC := birth.LegTypes[qovC.Correlation]; gotC == nil || len(gotC.Fields) != 1 || gotC.Fields[0].Name != "X" {
		t.Fatalf("bare-QOV leg type missing: %v", birth.LegTypes)
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
	pos, err := evaluateOrdinalJoinRow(mixed, birth.OutputType, binder)
	if err != nil {
		t.Fatalf("mixed-upper row birth: %v", err)
	}
	// _0/_1 flatten the inner nesting one level; _2 is c's whole row.
	if pos.Slots[0] != values.OrdinalRow(legARow) || pos.Slots[1] != values.OrdinalRow(legBRow) {
		t.Fatalf("translated slots must flatten one nesting level: %v", pos.Slots)
	}
	if pos.Slots[2] != values.OrdinalRow(cRow) {
		t.Fatalf("bare leg slot = %v, want c's whole row", pos.Slots[2])
	}
}

// TestRFC173S3_MergeBirth_FlatMapBirths pins the fulcrum's settlement of the
// merge Datum story (superseding the pre-fulcrum decline pin): the all-bare
// merge RC DOES birth on the FlatMap (correlated) path — the partition rule's
// merge arm produces live merge-RC FlatMaps whose inner plans carry baked leg
// SARGs, so a name-model outer binding here would feed those baked operands a
// name-keyed context (BakedNameContextError at scan-range build). The
// coexistence Datum carries each leg's own DATUM under the `_i` keys — the
// exact shape the §5 oracle produces by evaluating the bare-QOV RC over name
// bindings — never raw OrdinalRows, preserving dualwindow row-for-row
// invariance.
func TestRFC173S3_MergeBirth_FlatMapBirths(t *testing.T) {
	t.Parallel()
	legA, legB, qovA, qovB, _ := ojWiringLegs(t)
	c, err := newFlatMapCursor(
		recordlayer.FromList([]QueryResult{}), nil, nil, nil, EmptyEvaluationContext(),
		qovA.Correlation, qovB.Correlation,
		s3MergeRC(qovA, qovB), false, recordlayer.ExecuteProperties{},
	)
	if err != nil {
		t.Fatalf("newFlatMapCursor: %v", err)
	}
	defer c.Close()
	if !c.birth.enabled() || c.birth.WindowsOK {
		t.Fatalf("the merge RC must birth on the FlatMap path without windows (enabled=%v windows=%v)", c.birth.enabled(), c.birth.WindowsOK)
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
		// The coexistence Datum: per-leg DATUM MAPS under _0/_1 — the oracle's
		// shape (bare QOV evaluated over name bindings), never OrdinalRows.
		wantDatum := map[string]any{
			"_0": map[string]any{"ID": int64(1), "V": int64(10)},
			"_1": map[string]any{"ID": int64(2), "W": int64(20)},
		}
		if !reflect.DeepEqual(got.Datum, wantDatum) {
			t.Fatalf("Datum = %#v, want per-leg Datum maps %#v", got.Datum, wantDatum)
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
		// The name model reconstructs the empty-Datum inner row; the merge
		// Datum mirrors that as the empty map — exactly the oracle's output.
		wantDatum := map[string]any{
			"_0": map[string]any{"ID": int64(1), "V": int64(10)},
			"_1": map[string]any{},
		}
		if !reflect.DeepEqual(got.Datum, wantDatum) {
			t.Fatalf("null-inner Datum = %#v, want %#v", got.Datum, wantDatum)
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

func (r *fakeExecOrdinalRow) GetByName(name string) (any, bool) {
	for i, n := range r.names {
		if n == name {
			return r.slots[i], true
		}
	}
	return nil, false
}

// s3FusedRef builds the fused two-step reference the TranslationMap's rebuild
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

// TestRFC173S3_TranslatedTopSpans pins the S3 fulcrum's span recovery for a
// TRANSLATED top result value — the partition rule's post-merge shape: pinned
// single-accessor refs over the remaining leg mixed with FUSED two-step refs
// over the merge quantifier. The merged-away legs' aliases survive only in the
// merge quantifier's child RV (the positional-merge RC), so the probe resolves
// through legRVs; without it (the S2 nil probe) the shape must decline —
// fail-safe, never mis-windowed.
func TestRFC173S3_TranslatedTopSpans(t *testing.T) {
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

	// The S2 probe (no legRVs) must DECLINE the fused shape.
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

// TestRFC173S3_SpliceLegSpans pins the recursive box splice: a span whose leg
// is itself a join plan's output (a FULL box's gated-join leg — an anonymous
// flat concat named after its buried rightmost table) is replaced by the
// leg's OWN spans offset into the parent row, exposing the LEAF table aliases
// dotted upper references actually name. The width guard keeps a
// non-tiling (shadowed-alias) splice opaque instead of mis-windowed.
func TestRFC173S3_SpliceLegSpans(t *testing.T) {
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

// TestRFC173S3_HashJoinDeclinesFusedPred pins the hash-join fast path against
// FUSED merge references (review catch on the fulcrum): an upper NLJ whose
// inner leg is a materialized MERGE select carries preds like
// `a.id = m._0.id`; the equijoin extractor read the fused ref's display name
// as a plain "M.ID" hash key, probed the merge-shaped rows (which carry only
// `_i` slots) for "ID", built an EMPTY index, and the ≥100-row fast path
// silently dropped every match — while the linear path (and the slow
// evaluation the index is supposed to accelerate) resolves the fused
// reference correctly. Fused refs must decline hash extraction.
func TestRFC173S3_HashJoinDeclinesFusedPred(t *testing.T) {
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

	// 150 merge-shaped inner rows (≥100 arms the hash index), DUAL like the
	// real merge emission: Datum {_i: leg map} + nested positional. Leg B's
	// ID runs 0..149, so every outer row finds exactly one match.
	innerRows := make([]QueryResult, 150)
	for i := range innerRows {
		legBRow := &fakeExecOrdinalRow{names: []string{"ID", "W"}, slots: []any{int64(i), int64(i * 10)}}
		legCRow := &fakeExecOrdinalRow{names: []string{"X"}, slots: []any{int64(i)}}
		pos := NewPositionalRow(mergedType)
		pos.Set(0, legBRow)
		pos.Set(1, legCRow)
		innerRows[i] = QueryResult{
			Datum: map[string]any{
				"_0": map[string]any{"ID": int64(i), "W": int64(i * 10)},
				"_1": map[string]any{"X": int64(i)},
			},
			Positional: pos,
		}
	}
	outerRows := []QueryResult{
		ojLegQR(t, legA, int64(3), int64(30)),
		ojLegQR(t, legA, int64(7), int64(70)),
	}

	// a.id = m._0.id — the fused two-step rebase shape; the MIXED upper RV
	// (baked over both legs) makes this a real ordinal-birth cursor.
	aRef, err := values.NewFieldValueOfOrdinal(qovA, 0)
	if err != nil {
		t.Fatalf("bake A#0: %v", err)
	}
	mixed := values.NewRawRecordConstructorValue(
		values.RecordConstructorField{Name: "AID", Value: aRef},
		values.RecordConstructorField{Name: "BID", Value: s3FusedRef(t, mergeQOV, 0, 0)},
	)
	pred := ojEqPred(
		values.NewFieldValue(qovA, "ID", values.NotNullLong),
		s3FusedRef(t, mergeQOV, 0, 0),
	)
	c := mustNLJCursor(t, recordlayer.FromList(outerRows), innerRows, plans.JoinInner,
		"A", "M", []predicates.QueryPredicate{pred}, mixed, EmptyEvaluationContext(), nil)
	defer c.Close()
	results := collectCursor(t, c)
	if len(results) != 2 {
		t.Fatalf("got %d rows, want 2 (a.id 3 and 7 each match one merge row) — a hash index keyed on the fused ref's display name drops them all", len(results))
	}
}

// TestRFC173S3_FlatMapSeedBoxLegDatumSplice pins the cursor-side SPLICE for a
// PRISTINE seed whose leg is a gated-join BOX (review catch on the fulcrum):
// the seed's span names the box after its rightmost leaf and covers the whole
// concat, so without the splice datumFromSpans qualifies every concat column
// under that ONE alias — "B.ID" carrying leg A's ID — and dotted reads above
// ("A.ID") silently miss. With the box leg's own RV available through the
// outer plan, the coexistence Datum must qualify by LEAF alias. The leg
// ADAPTER meanwhile keeps the box-level window (Spans, unspliced): the outer
// binding flows the whole concat row.
func TestRFC173S3_FlatMapSeedBoxLegDatumSplice(t *testing.T) {
	t.Parallel()
	legA, legB, qovA, qovB, boxSeedRV := ojWiringLegs(t)
	legC := values.NewRecordType("", false, []values.Field{
		{Name: "X", FieldType: values.NotNullLong, Ordinal: 0},
	})
	qovC := values.NewQuantifiedObjectValueOfType(values.NamedCorrelationIdentifier("C"), legC)

	// The box leg: alias "B" (sourceAlias names it after its rightmost leaf),
	// type = the flat {A,B} concat (RAW: duplicate names legal).
	boxType := &values.RecordType{Fields: []values.Field{
		{Name: "ID", FieldType: values.NotNullLong, Ordinal: 0},
		{Name: "V", FieldType: values.NotNullLong, Ordinal: 1},
		{Name: "ID", FieldType: values.NotNullLong, Ordinal: 2},
		{Name: "W", FieldType: values.NotNullLong, Ordinal: 3},
	}}
	boxQOV := values.NewQuantifiedObjectValueOfType(values.NamedCorrelationIdentifier("B"), boxType)
	topRV := buildOrdinalJoinRC(t, boxQOV, qovC)

	// The outer plan carries the box's OWN seed RV — where the leaf aliases
	// survive for the splice.
	outerPlan := plans.NewRecordQueryFlatMapPlan(nil, nil,
		qovA.Correlation, qovB.Correlation, boxSeedRV, false)

	c, err := newFlatMapCursor(
		nil, outerPlan, nil, nil, EmptyEvaluationContext(),
		boxQOV.Correlation, qovC.Correlation,
		topRV, false, recordlayer.ExecuteProperties{},
	)
	if err != nil {
		t.Fatalf("newFlatMapCursor: %v", err)
	}
	if !c.birth.enabled() || !c.birth.WindowsOK {
		t.Fatalf("seed over [box, C] must birth with windows (enabled=%v windows=%v)", c.birth.enabled(), c.birth.WindowsOK)
	}
	// The adapter keeps the BOX window; the Datum spans open to the leaves.
	if got := c.birth.legType(boxQOV.Correlation); got == nil || len(got.Fields) != 4 {
		t.Fatalf("adapter leg type for the box alias = %v, want the whole 4-col concat", got)
	}
	if len(c.birth.DatumSpans) != 3 {
		t.Fatalf("DatumSpans = %+v, want 3 spliced spans (A, B leaf, C)", c.birth.DatumSpans)
	}

	// Drive a row through: the box flows its flat concat positional.
	boxRow := QueryResult{Datum: map[string]any{}, Positional: func() *PositionalRow {
		p := NewPositionalRow(boxType)
		for i, v := range []any{int64(1), int64(10), int64(2), int64(20)} {
			p.Set(i, v)
		}
		return p
	}()}
	cRow := ojLegQR(t, legC, int64(7))
	got, err := c.computeResult(boxRow, cRow)
	if err != nil {
		t.Fatalf("computeResult: %v", err)
	}
	datum, isMap := rowMap(got)
	if !isMap {
		t.Fatalf("Datum = %T, want map", got.Positional)
	}
	// Leaf-alias qualified keys — the whole point of the splice.
	for k, want := range map[string]any{
		"A.ID": int64(1), "A.V": int64(10), "B.ID": int64(2), "B.W": int64(20), "C.X": int64(7),
	} {
		if datum[k] != want {
			t.Fatalf("Datum[%q] = %v, want %v (full Datum: %#v)", k, datum[k], want, datum)
		}
	}
	_ = legA
	_ = legB
}
