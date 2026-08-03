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
		values.UnknownType, false,
		[]string{"ID"}).
		WithKeyComponentTypes([]values.Type{values.NullableDouble}).
		WithPrimaryKeyComponentTypes([]values.Type{values.NullableLong})
	cand.WithRecordTypeRowTypes([]values.Type{row})
	return cand, alias
}

func signedZeroSuffixParts(
	t *testing.T, literal any, isReverse bool,
) []*MatchedOrderingPart {
	t.Helper()
	cand, alias := signedZeroSuffixCandidate(t)
	mi := NewRegularMatchInfo(
		map[values.CorrelationIdentifier]*predicates.ComparisonRange{
			alias: equalityRange(t, literal),
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
	fv, ok := parts[1].GetValue().(*values.FieldValue)
	if !ok || fv.Field != "ID" {
		t.Fatalf("second part must be the ID primary-key suffix, got %v", parts[1].GetValue())
	}
}
