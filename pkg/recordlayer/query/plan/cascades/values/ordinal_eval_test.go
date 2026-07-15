package values

import (
	"errors"
	"sort"
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

// fom ("fake ordinal from map") wraps a name->value map as an OrdinalRow for
// the value-eval unit tests that historically passed a bare name-keyed map to
// Evaluate. Keys are SORTED, so a field's ordinal is its ALPHABETICAL slot
// position — deterministic across runs (Go map iteration order is
// randomized). The ordinal PositionalRow (here fakeOrdinalRow) is the sole
// eval context, and every column reference must carry a plan-time ordinal —
// a BAKED FieldValue (fomB's baker, or NewFieldValueWithResolvedOrdinal /
// NewCorrelatedFieldValueWithResolvedOrdinal) reads row.Get(ordinal). A LAZY
// flat reference does not resolve against fom by name; it fails loud. A key
// PRESENT with a nil value reads as SQL NULL; an ABSENT key is a loud miss
// (out-of-range ordinal), never a silent NULL — a test that wants NULL must
// include the key.
func fom(m map[string]any) *fakeOrdinalRow {
	r := &fakeOrdinalRow{}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		r.names = append(r.names, k)
		r.slots = append(r.slots, m[k])
	}
	return r
}

// fomB is fom plus a baker: bk(field, typ) returns a BAKED flat FieldValue
// whose ordinal is field's (sorted) slot in the row — every ref binds its
// ordinal at plan time so runtime access is positional (row.Get(ordinal)),
// never a name lookup. bk panics if field is absent from the row (a test bug
// — bake only real columns; loud-miss behavior is exercised with a lazy
// NewFlatFieldValue instead).
func fomB(m map[string]any) (*fakeOrdinalRow, func(string, Type) *FieldValue) {
	r := fom(m)
	bk := func(field string, typ Type) *FieldValue {
		for i, n := range r.names {
			if n == field {
				return NewFieldValueWithResolvedOrdinal(field, i, typ)
			}
		}
		panic("fomB: baked field not present in row: " + field)
	}
	return r, bk
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

// TestFieldValue_OrdinalEval exercises the ordinal read path
// FieldValue.Evaluate takes when the runtime row is an OrdinalRow. It pins:
// (1) a BAKED flat reference reads its plan-time ordinal positionally (the
// CTE-column-rename shape — the rename is baked, not re-derived by name);
// (2) a BAKED QOV-child reference bound to an ordinal row reads by ordinal;
// (3)+(4) a resolution miss (lazy flat miss / out-of-range ordinal) is a
// LOUD OrdinalResolutionError, never a silent NULL — there is no name
// fallback on the ordinal row.
func TestFieldValue_OrdinalEval(t *testing.T) {
	t.Parallel()

	// (1) Flat FieldValue over an ordinal row — the CTE-rename shape. The row's
	// TYPE carries the RENAMED names [X, Y]; slots are positional [10, 20].
	// The CTE rename is handled by BAKING X's ordinal at plan time (there is
	// no runtime name->ordinal fallback), so the baked "X" reads positional
	// slot 0 directly.
	flat := NewFieldValueWithResolvedOrdinal("X", 0, UnknownType)
	renamedRow := &fakeOrdinalRow{names: []string{"X", "Y"}, slots: []any{int64(10), int64(20)}}
	got, err := flat.Evaluate(renamedRow)
	if err != nil {
		t.Fatalf("flat ordinal eval: %v", err)
	}
	if got != int64(10) {
		t.Fatalf("flat X over renamed row = %v, want 10 (positional slot 0)", got)
	}

	// (2) QOV-child FieldValue bound to an ordinal row (correlated path). The QOV
	// is typed [ID, V]; "V" is BAKED to ordinal 1 → slot 1.
	rt := NewRecordType("", false, []Field{
		{Name: "ID", FieldType: UnknownType, Ordinal: 0},
		{Name: "V", FieldType: UnknownType, Ordinal: 1},
	})
	corr := UniqueCorrelationIdentifier()
	qov := NewQuantifiedObjectValueOfType(corr, rt)
	fv := NewCorrelatedFieldValueWithResolvedOrdinal(qov, "V", 1, UnknownType)
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
