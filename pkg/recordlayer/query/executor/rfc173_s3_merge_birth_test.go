package executor

import (
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
// real NLJ cursor: the emitted rows carry the UNTOUCHED mergeRows Datum
// (dual emission) plus a nested Positional row — slot i IS leg i's
// positional row — and a fused two-step reference (commit 1's TranslationMap
// output shape) reads through it end-to-end.
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

	// Dual emission: the name-model Datum is mergeRows', untouched.
	datum, isMap := results[0].Datum.(map[string]any)
	if !isMap || datum["A.ID"] != int64(1) || datum["B.W"] != int64(100) {
		t.Fatalf("Datum must stay the untouched mergeRows map, got %v", results[0].Datum)
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

// TestRFC173S3_MergeBirth_FlatMapDeclines pins the review P2: the all-bare
// merge RC does NOT birth on the FlatMap (correlated) path — a birth there
// would derive the coexistence Datum from the positional row (OrdinalRows
// under _i) while the §5 oracle path evaluates the bare-QOV RC against name
// bindings (Datum maps under _i), breaking dualwindow row-for-row
// invariance. The merge row's Datum story is the fulcrum's; until then the
// shape stays name-model on BOTH oracle sides of the FlatMap.
func TestRFC173S3_MergeBirth_FlatMapDeclines(t *testing.T) {
	t.Parallel()
	_, _, qovA, qovB, _ := ojWiringLegs(t)
	c, err := newFlatMapCursor(
		recordlayer.FromList([]QueryResult{}), nil, nil, EmptyEvaluationContext(),
		qovA.Correlation, qovB.Correlation,
		s3MergeRC(qovA, qovB), false, recordlayer.ExecuteProperties{},
	)
	if err != nil {
		t.Fatalf("newFlatMapCursor: %v", err)
	}
	defer c.Close()
	if c.birth.enabled() {
		t.Fatal("the merge RC must NOT birth on the FlatMap path (oracle invariance — fulcrum owns the merge Datum story)")
	}
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
