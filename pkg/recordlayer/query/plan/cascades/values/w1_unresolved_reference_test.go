package values

import (
	"errors"
	"reflect"
	"sort"
	"testing"

	"github.com/stretchr/testify/require"
)

// RFC-048 W1 under the RFC-173 ordinal model: the executor "no unresolved
// reference" invariant. A top-level FieldValue whose name is absent from the
// authoritative ordinal row must be a LOUD *OrdinalResolutionError — never a
// silent NULL (the cardinal silent-wrong).
//
// This REPLACES the retired name-model Strict/ReportUnresolvedReference hook.
// The cap made the ordinal PositionalRow the sole runtime row, and
// evaluateOrdinal goes loud on a miss — so a masked reference fails the query
// outright, a strictly STRONGER guarantee than the old report-and-continue hook.
// The end-to-end proof that no real query trips it is the sqldriver FDB suite:
// any query that hit a miss would fail loudly, so a GREEN suite IS the
// binary-wide W1 invariant (no separate binary-wide hook needed).
//
// These tests pin the mechanism in isolation (FieldValue.Evaluate over an
// OrdinalRow). They are pure (no FDB) and safe under t.Parallel() — unlike the
// retired hook-based tests, they touch no process-global state.

// w1Row is a local OrdinalRow that also exposes TypeNames, so an
// *OrdinalResolutionError raised against it carries the row's available names
// for the attribution assertion. (fakeOrdinalRow lacks TypeNames on purpose;
// duplicating the three trivial methods here keeps this test self-contained and
// avoids perturbing that shared helper's diagnostics.)
type w1Row struct {
	names []string
	slots []any
}

func (r *w1Row) Get(ord int) (any, bool) {
	if ord < 0 || ord >= len(r.slots) {
		return nil, false
	}
	return r.slots[ord], true
}

func (r *w1Row) GetByName(name string) (any, bool) {
	for i, n := range r.names {
		if n == name {
			return r.slots[i], true
		}
	}
	return nil, false
}

func (r *w1Row) TypeNames() []string { return r.names }

func TestFieldValue_OrdinalMiss_IsLoud(t *testing.T) {
	t.Parallel()

	// Exhibit-A shape: the aggregate slot was materialised under "COUNT(*)" but
	// the HAVING rewrite referenced "COUNT(1)". Against the ordinal row that only
	// carries "COUNT(*)", the "COUNT(1)" read is an unresolved reference — a loud
	// *OrdinalResolutionError, not a silent NULL.
	row := &w1Row{names: []string{"COUNT(*)", "STATUS"}, slots: []any{int64(3), "shipped"}}
	v, err := (&FieldValue{Field: "COUNT(1)"}).Evaluate(row)
	if v != nil {
		t.Fatalf("missing local ref must not yield a value, got %v", v)
	}
	var ordErr *OrdinalResolutionError
	if !errors.As(err, &ordErr) {
		t.Fatalf("missing local ref must be a loud *OrdinalResolutionError, got %v", err)
	}
	if ordErr.Field != "COUNT(1)" {
		t.Fatalf("error Field: want COUNT(1), got %q", ordErr.Field)
	}
}

func TestFieldValue_OrdinalPresentNil_IsNull(t *testing.T) {
	t.Parallel()

	// A present key whose value is a legitimate SQL NULL must NOT be loud: the
	// distinction is "name absent" (bug) vs "value nil" (NULL). This also covers
	// the base-record optional-field case — a base stored record's ordinal row
	// carries every field of its type, unset optionals present-as-nil, so an
	// unset optional reads NULL here (never an absent key), never a violation.
	row := &w1Row{names: []string{"COUNT(*)", "SUM(AMOUNT)"}, slots: []any{int64(3), nil}}

	// RFC-173 item C: each reference carries its plan-time ordinal — the field's slot
	// in the row (COUNT(*)=0, SUM(AMOUNT)=1), read positionally. Present-nil reads NULL.
	v, err := NewFieldValueWithResolvedOrdinal("SUM(AMOUNT)", 1, UnknownType).Evaluate(row)
	require.NoError(t, err)
	if v != nil {
		t.Fatalf("present nil-valued key: want nil value, got %v", v)
	}

	v, err = NewFieldValueWithResolvedOrdinal("COUNT(*)", 0, UnknownType).Evaluate(row)
	require.NoError(t, err)
	if v != int64(3) {
		t.Fatalf("present key: want 3, got %v", v)
	}
}

func TestFieldValue_OrdinalMiss_CarriesAvailableKeys(t *testing.T) {
	t.Parallel()

	// The diagnostic carries the row's actual name set so a violation is
	// attributable (what was materialised vs what was asked for).
	row := &w1Row{names: []string{"A", "COUNT(*)"}, slots: []any{1, 2}}
	_, err := (&FieldValue{Field: "B"}).Evaluate(row)
	var ordErr *OrdinalResolutionError
	if !errors.As(err, &ordErr) {
		t.Fatalf("want *OrdinalResolutionError, got %v", err)
	}
	avail := append([]string(nil), ordErr.Available...)
	sort.Strings(avail)
	if want := []string{"A", "COUNT(*)"}; !reflect.DeepEqual(avail, want) {
		t.Fatalf("available keys: want %v, got %v", want, avail)
	}
}
