package values

import (
	"errors"
	"testing"
)

// fakeOrdinalRow is a test-local OrdinalRow (executor.PositionalRow lives in a
// higher layer that this package cannot import). Get is positional; GetByName
// resolves name->ordinal against the row's own name list.
type fakeOrdinalRow struct {
	names []string
	slots []any
}

func (r *fakeOrdinalRow) Get(ord int) (any, bool) {
	if ord < 0 || ord >= len(r.slots) {
		return nil, false
	}
	return r.slots[ord], true
}

func (r *fakeOrdinalRow) GetByName(name string) (any, bool) {
	for i, n := range r.names {
		if n == name {
			return r.slots[i], true
		}
	}
	return nil, false
}

type ordEvalBinder struct {
	id    CorrelationIdentifier
	bound any
}

func (b *ordEvalBinder) GetCorrelationBinding(id CorrelationIdentifier) (any, bool) {
	if id == b.id {
		return b.bound, true
	}
	return nil, false
}

// TestFieldValue_OrdinalEval_RFC173Slice1 exercises the DARK ordinal read path
// FieldValue.Evaluate takes when the runtime row is an OrdinalRow (RFC-173
// Slice 1). No production producer flows an OrdinalRow yet — this drives the
// branch directly. It pins: (1) a flat reference resolves against the row's OWN
// renamed type (the CTE-column-rename fix — the name map, keyed by the
// UNDERLYING names, would read NULL); (2) a typed QOV-child reference bound to an
// ordinal row resolves by ordinal; (3)+(4) a resolution miss is a LOUD
// OrdinalResolutionError, never a silent NULL (Graefe: no name-map fallback on
// the authoritative frontier).
func TestFieldValue_OrdinalEval_RFC173Slice1(t *testing.T) {
	t.Parallel()

	// (1) Flat FieldValue over an ordinal row — the CTE-rename shape. The row's
	// TYPE carries the RENAMED names [X, Y]; slots are positional [10, 20]. Flat
	// "X" resolves name->ordinal against the row's own type → slot 0.
	flat := NewFlatFieldValue("X", UnknownType)
	renamedRow := &fakeOrdinalRow{names: []string{"X", "Y"}, slots: []any{int64(10), int64(20)}}
	got, err := flat.Evaluate(renamedRow)
	if err != nil {
		t.Fatalf("flat ordinal eval: %v", err)
	}
	if got != int64(10) {
		t.Fatalf("flat X over renamed row = %v, want 10 (positional slot 0)", got)
	}

	// (2) QOV-child FieldValue bound to an ordinal row (correlated path). The QOV
	// is typed [ID, V]; "V" resolves to ordinal 1 → slot 1.
	rt := NewRecordType("", false, []Field{
		{Name: "ID", FieldType: UnknownType, Ordinal: 0},
		{Name: "V", FieldType: UnknownType, Ordinal: 1},
	})
	corr := UniqueCorrelationIdentifier()
	qov := NewQuantifiedObjectValueOfType(corr, rt)
	fv := NewFieldValue(qov, "V", UnknownType)
	binder := &ordEvalBinder{id: corr, bound: &fakeOrdinalRow{slots: []any{int64(10), int64(20)}}}
	got, err = fv.Evaluate(&RowEvalContext{Correlations: binder})
	if err != nil {
		t.Fatalf("correlated ordinal eval: %v", err)
	}
	if got != int64(20) {
		t.Fatalf("Q.V over ordinal row = %v, want 20 (ordinal 1)", got)
	}

	// (3) Loud error on a flat miss — NOT a silent NULL.
	var ore *OrdinalResolutionError
	if _, err = NewFlatFieldValue("MISSING", UnknownType).Evaluate(renamedRow); !errors.As(err, &ore) {
		t.Fatalf("flat miss must be a loud OrdinalResolutionError, got %v", err)
	}

	// (4) Loud error on an out-of-range ordinal (row shorter than the type).
	shortRow := &fakeOrdinalRow{slots: []any{int64(10)}} // slot 0 only; ordinal 1 missing
	if _, err = fv.Evaluate(&RowEvalContext{Correlations: &ordEvalBinder{id: corr, bound: shortRow}}); !errors.As(err, &ore) {
		t.Fatalf("out-of-range ordinal must be a loud OrdinalResolutionError, got %v", err)
	}
}
