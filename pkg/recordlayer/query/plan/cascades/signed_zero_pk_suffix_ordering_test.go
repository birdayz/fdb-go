package cascades

// ComputeMatchedOrderingParts over an index whose LAST key column is bound by a
// signed-zero-widened float equality.
//
// The widened equality claims its OWN order but carries no order THROUGH itself,
// so the trimmed primary-key suffix must not be appended. The loop expresses the
// second half by breaking AFTER emitting the coordinate — and a break alone does
// not express it, because the suffix gate asks "did the loop consume the whole
// index key?" and the answer is YES once the terminating coordinate has been
// emitted and counted. That is only reachable when the coordinate is the LAST
// key column, which is why the index here has exactly one: on a wider index the
// count falls short by itself and the suffix is refused for the wrong reason,
// leaving the defect invisible.
//
// The end-to-end consequence, and the wrong answer it produces, is pinned by
// TestFDB_SignedZeroEqualityDoesNotOrderThePKSuffix in pkg/relational/sqldriver.

import (
	"fmt"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// signedZeroSuffixCandidate builds a one-double-key-column index over PK (ID).
//
// The record row layout is supplied deliberately. Without it the candidate's
// ordering-key layout is nil, ColumnCanExtendOrderingClaim answers "yes" for
// every name, and the float reasoning under test is bypassed entirely — the
// test would then pass with the defect fully present.
func signedZeroSuffixCandidate(t *testing.T) (*ValueIndexScanMatchCandidate, values.CorrelationIdentifier) {
	t.Helper()
	row := values.NewRecordType("", false, []values.Field{
		{Name: "ID", FieldType: values.NullableLong, Ordinal: 0},
		{Name: "V", FieldType: values.NullableDouble, Ordinal: 1},
	})
	alias := values.UniqueCorrelationIdentifier()
	cand := newKnownDistinctValueIndexCandidate(
		"IDX_V", []string{"T"},
		[]string{"V"},
		[]values.CorrelationIdentifier{alias},
		row, false,
		[]string{"ID"}).
		WithKeyComponentTypes([]values.Type{values.NullableDouble}).
		WithPrimaryKeyComponentTypes([]values.Type{values.NullableLong})
	cand.WithRecordTypeRowTypes([]values.Type{row})
	return cand, alias
}

func signedZeroEqualityRange(t testing.TB, literal any) *predicates.ComparisonRange {
	t.Helper()
	comparison := predicates.NewLiteralComparison(predicates.ComparisonEquals, literal)
	merged := predicates.EmptyComparisonRange().Merge(&comparison)
	if !merged.Complete() {
		t.Fatal("build signed-zero equality range")
	}
	return merged.Range
}

func signedZeroSuffixParts(
	t *testing.T, literal any, isReverse bool,
) []*MatchedOrderingPart {
	t.Helper()
	cand, alias := signedZeroSuffixCandidate(t)
	mi := NewRegularMatchInfo(
		map[values.CorrelationIdentifier]*predicates.ComparisonRange{
			alias: signedZeroEqualityRange(t, literal),
		},
		nil, nil, nil, nil, nil, nil, nil)
	return cand.ComputeMatchedOrderingParts(mi, []values.CorrelationIdentifier{alias}, isReverse)
}

// TestMatchedOrderingParts_SignedZeroEqualityRefusesThePKSuffix: the widened
// coordinate is emitted, and the primary key after it is NOT.
func TestMatchedOrderingParts_SignedZeroEqualityRefusesThePKSuffix(t *testing.T) {
	t.Parallel()

	for _, isReverse := range []bool{false, true} {
		isReverse := isReverse
		name := "forward"
		if isReverse {
			name = "reverse"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			parts := signedZeroSuffixParts(t, float64(0), isReverse)
			if len(parts) != 1 {
				got := make([]string, 0, len(parts))
				for _, p := range parts {
					got = append(got, fmt.Sprintf("%v", p.GetValue()))
				}
				t.Fatalf(
					"matched ordering parts = %v (%d), want exactly 1 (V).\n"+
						"A zero-valued float equality spans TWO physical key blocks, so the "+
						"primary key restarts at the block boundary and is not ordered across "+
						"the pair. Appending it claims an order the scan does not deliver.",
					got, len(parts))
			}
			if !parts[0].GetComparisonRange().IsEquality() {
				t.Fatal("the emitted coordinate must carry its equality range")
			}
		})
	}
}

// TestMatchedOrderingParts_SignedZeroEqualityStillClaimsItsOwnOrder pins the
// other half. The coordinate is refused the SUFFIX, never its own emission —
// dropping it entirely would cost every such query the index order it does
// have, which is the regression the emit-then-break shape exists to prevent.
func TestMatchedOrderingParts_SignedZeroEqualityStillClaimsItsOwnOrder(t *testing.T) {
	t.Parallel()

	parts := signedZeroSuffixParts(t, float64(0), false)
	if len(parts) == 0 {
		t.Fatal("the widened coordinate must still be emitted as an ordering part: " +
			"the scan opens the two zero blocks in key order, so it IS ordered — " +
			"directionally — and refusing it outright forfeits a sound claim")
	}
}

// TestMatchedOrderingParts_NonZeroFloatEqualityKeepsThePKSuffix is the control.
// An ordinary float equality pins ONE physical key, so the suffix after it is
// genuinely ordered and must not be caught by the refusal above.
func TestMatchedOrderingParts_NonZeroFloatEqualityKeepsThePKSuffix(t *testing.T) {
	t.Parallel()

	parts := signedZeroSuffixParts(t, float64(1.5), false)
	if len(parts) != 2 {
		t.Fatalf("matched ordering parts = %d, want 2 (V + the ID suffix); a non-zero "+
			"float equality pins a single key, so the primary key after it stays ordered",
			len(parts))
	}
	fv, ok := values.AsFieldValue(parts[1].GetValue())
	if !ok || fv.DisplayName() != "ID" {
		t.Fatalf("second part must be the ID primary-key suffix, got %v", parts[1].GetValue())
	}
	if got := fv.Path().Ordinals(); len(got) != 1 || got[0] != 0 {
		t.Fatalf("ID suffix ordinal path = %v, want [0]", got)
	}
}

// signedZeroTwoColumnCandidate builds INDEX(V DOUBLE, B LONG) over PK (ID), for
// the coordinate AFTER a widened one.
func signedZeroTwoColumnCandidate(t *testing.T) (*ValueIndexScanMatchCandidate, []values.CorrelationIdentifier) {
	t.Helper()
	row := values.NewRecordType("", false, []values.Field{
		{Name: "ID", FieldType: values.NullableLong, Ordinal: 0},
		{Name: "V", FieldType: values.NullableDouble, Ordinal: 1},
		{Name: "B", FieldType: values.NullableLong, Ordinal: 2},
	})
	aliases := []values.CorrelationIdentifier{values.UniqueCorrelationIdentifier(), values.UniqueCorrelationIdentifier()}
	cand := newKnownDistinctValueIndexCandidate(
		"IDX_V_B", []string{"T"},
		[]string{"V", "B"},
		aliases,
		row, false,
		[]string{"ID"}).
		WithKeyComponentTypes([]values.Type{values.NullableDouble, values.NullableLong}).
		WithPrimaryKeyComponentTypes([]values.Type{values.NullableLong})
	cand.WithRecordTypeRowTypes([]values.Type{row})
	return cand, aliases
}

func signedZeroTwoColumnParts(t *testing.T, bindings ...*predicates.ComparisonRange) []*MatchedOrderingPart {
	t.Helper()
	cand, aliases := signedZeroTwoColumnCandidate(t)
	bound := map[values.CorrelationIdentifier]*predicates.ComparisonRange{}
	for i, cr := range bindings {
		if cr != nil {
			bound[aliases[i]] = cr
		}
	}
	mi := NewRegularMatchInfo(bound, nil, nil, nil, nil, nil, nil, nil)
	return cand.ComputeMatchedOrderingParts(mi, aliases, false)
}

func partNames(parts []*MatchedOrderingPart) []string {
	got := make([]string, 0, len(parts))
	for _, p := range parts {
		if fv, ok := values.AsFieldValue(p.GetValue()); ok {
			got = append(got, fv.DisplayName())
		} else {
			got = append(got, fmt.Sprintf("%v", p.GetValue()))
		}
	}
	return got
}

// TestMatchedOrderingParts_PinnedCoordinateAfterWidenedOneIsEmitted: FIXED is a
// per-coordinate fact. Under `v = 0.0 AND b = 1` the widened V is emitted (its
// own order) and B — one physical key within every block of V, every admitted
// row carrying b = 1 — is emitted too, as an equality part the consumer reads
// as FIXED; the PK suffix stays refused because V carries no order through
// itself. This is the candidate-side twin of the plan-side rich form, which
// binds B FIXED for the same scan; breaking at V emitted nothing for B and
// let the two derivations disagree on the same coordinate.
func TestMatchedOrderingParts_PinnedCoordinateAfterWidenedOneIsEmitted(t *testing.T) {
	t.Parallel()

	parts := signedZeroTwoColumnParts(t,
		signedZeroEqualityRange(t, float64(0)), signedZeroEqualityRange(t, int64(1)))
	if got := partNames(parts); len(got) != 2 || got[0] != "V" || got[1] != "B" {
		t.Fatalf("matched ordering parts = %v, want [V B]: B = 1 pins one key within each of V's "+
			"two blocks, so it is FIXED everywhere in the stream; the ID suffix restarts at the "+
			"block boundary and must not follow", got)
	}
	if !parts[1].GetComparisonRange().IsEquality() {
		t.Fatal("B must carry its equality range so the consumer classifies it FIXED")
	}
}

// TestMatchedOrderingParts_UnboundOrWidenedCoordinateAfterWidenedOneIsNotEmitted:
// only a PINNED coordinate may follow a widened one. An unbound B restarts at
// each of V's block boundaries, and a second widened equality (both zero
// doubles, over INDEX(V, V2)) restarts likewise — neither is ordered across
// the stream, so the claim ends at V.
func TestMatchedOrderingParts_UnboundOrWidenedCoordinateAfterWidenedOneIsNotEmitted(t *testing.T) {
	t.Parallel()

	unbound := signedZeroTwoColumnParts(t, signedZeroEqualityRange(t, float64(0)), nil)
	if got := partNames(unbound); len(got) != 1 || got[0] != "V" {
		t.Fatalf("matched ordering parts (v = 0.0, b unbound) = %v, want [V]: an unbound B is "+
			"ordered only within each of V's blocks", got)
	}

	row := values.NewRecordType("", false, []values.Field{
		{Name: "ID", FieldType: values.NullableLong, Ordinal: 0},
		{Name: "V", FieldType: values.NullableDouble, Ordinal: 1},
		{Name: "V2", FieldType: values.NullableDouble, Ordinal: 2},
	})
	aliases := []values.CorrelationIdentifier{values.UniqueCorrelationIdentifier(), values.UniqueCorrelationIdentifier()}
	cand := newKnownDistinctValueIndexCandidate(
		"IDX_V_V2", []string{"T"}, []string{"V", "V2"}, aliases, row, false, []string{"ID"}).
		WithKeyComponentTypes([]values.Type{values.NullableDouble, values.NullableDouble}).
		WithPrimaryKeyComponentTypes([]values.Type{values.NullableLong})
	cand.WithRecordTypeRowTypes([]values.Type{row})
	mi := NewRegularMatchInfo(map[values.CorrelationIdentifier]*predicates.ComparisonRange{
		aliases[0]: signedZeroEqualityRange(t, float64(0)),
		aliases[1]: signedZeroEqualityRange(t, float64(0)),
	}, nil, nil, nil, nil, nil, nil, nil)
	widened := cand.ComputeMatchedOrderingParts(mi, aliases, false)
	if got := partNames(widened); len(got) != 1 || got[0] != "V" {
		t.Fatalf("matched ordering parts (v = 0.0, v2 = 0.0) = %v, want [V]: a second widened "+
			"coordinate spans two blocks within each of V's and is not ordered across them", got)
	}
}
