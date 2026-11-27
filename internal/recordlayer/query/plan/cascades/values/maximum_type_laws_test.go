package values

import (
	"testing"
)

// maximumTypeCorpus is every predefined primitive Type in both nullabilities,
// plus the NULL literal type. MaximumType's job is to homogenise the operands
// of an arithmetic or comparison expression, so the corpus is the set of things
// that can actually appear on either side of one.
func maximumTypeCorpus() []Type {
	return []Type{
		NullType,
		NullableInt, NotNullInt,
		NullableLong, NotNullLong,
		NullableFloat, NotNullFloat,
		NullableDouble, NotNullDouble,
		NullableString, NotNullString,
		NullableBoolean, NotNullBoolean,
		NullableBytes, NotNullBytes,
		NullableUuid, NotNullUuid,
		NullableVersion, NotNullVersion,
		NullableDate, NotNullDate,
		NullableTimestamp, NotNullTimestamp,
	}
}

// TestMaximumType_IsAnUpperBoundOfBothOperands is the soundness law:
//
//	if MaximumType(a,b) != nil then IsPromotable(a, m) AND IsPromotable(b, m).
//
// MaximumType names the type an arithmetic or comparison expression will
// evaluate its operands at. If it returns a type an operand cannot actually be
// promoted to, the planner has emitted a coercion that does not exist — the
// operand is read as the wrong type, or the promotion is dropped and the
// operation runs on a mismatched pair.
//
// The existing tests are hand-written case tables asserting one expected type
// per pair. Those pin the answers somebody thought of; this pins the property
// every answer has to have, over the whole cross-product.
func TestMaximumType_IsAnUpperBoundOfBothOperands(t *testing.T) {
	t.Parallel()

	corpus := maximumTypeCorpus()
	defined, checked := 0, 0
	for _, a := range corpus {
		for _, b := range corpus {
			checked++
			m := MaximumType(a, b)
			if m == nil {
				continue // no common type: a legitimate answer, not a claim
			}
			defined++
			if !IsPromotable(a, m) {
				t.Errorf("MaximumType(%v, %v) = %v, but %v is NOT promotable to it — "+
					"the planner would coerce an operand through an edge that does not exist",
					a, b, m, a)
			}
			if !IsPromotable(b, m) {
				t.Errorf("MaximumType(%v, %v) = %v, but %v is NOT promotable to it — "+
					"the planner would coerce an operand through an edge that does not exist",
					a, b, m, b)
			}
		}
	}

	if checked != len(corpus)*len(corpus) {
		t.Fatalf("checked %d pairs, want %d", checked, len(corpus)*len(corpus))
	}
	// Vacuity guard: every assertion sits behind a nil check, so a MaximumType
	// that answered nil for everything would skip them all and pass clean.
	// Measured: 165 of 529 pairs have a maximum.
	if defined < 100 {
		t.Fatalf("only %d of %d pairs produced a maximum type — too few to exercise the "+
			"law, which is now close to vacuous", defined, checked)
	}
	t.Logf("maximum-type upper bound: %d pairs, %d with a defined maximum", checked, defined)
}

// TestMaximumType_IsCommutative pins that `a OP b` and `b OP a` homogenise to
// the same type. An asymmetry here means the two spellings of one comparison
// plan differently, which is a user-visible inconsistency reached by reordering
// operands.
//
// This is worth its own law because MaximumType's NULL and NONE handling is a
// pair of HAND-MIRRORED conditionals — `c1 == TypeCodeNull && IsPromotable(t1,
// t2)` and then the same with the indices swapped — and a hand-mirrored pair is
// exactly where one side drifts.
func TestMaximumType_IsCommutative(t *testing.T) {
	t.Parallel()

	corpus := maximumTypeCorpus()
	asymmetric := 0
	for _, a := range corpus {
		for _, b := range corpus {
			ab, ba := MaximumType(a, b), MaximumType(b, a)
			switch {
			case ab == nil && ba == nil:
			case ab == nil || ba == nil:
				asymmetric++
				t.Errorf("MaximumType is not commutative for (%v, %v): one direction is "+
					"defined and the other is nil (%v vs %v)", a, b, ab, ba)
			case !ab.Equals(ba):
				asymmetric++
				t.Errorf("MaximumType(%v, %v) = %v but MaximumType(%v, %v) = %v — the same "+
					"comparison plans to two different types depending on operand order",
					a, b, ab, b, a, ba)
			}
		}
	}
	if asymmetric > 0 {
		t.Logf("%d asymmetric pairs", asymmetric)
	}
}

// TestMaximumType_NullabilityRuleAndIdempotence pins the two remaining stated
// rules: MaximumType(a,a) is a, and the result is nullable iff either input is.
//
// The nullability half is what decides whether a NOT NULL column's expression
// stays NOT NULL. Getting it wrong in the permissive direction loses a
// constraint the storage layer relies on; in the strict direction it rejects
// rows that are fine.
func TestMaximumType_NullabilityRuleAndIdempotence(t *testing.T) {
	t.Parallel()

	corpus := maximumTypeCorpus()
	for _, a := range corpus {
		if m := MaximumType(a, a); m == nil || !m.Equals(a) {
			t.Errorf("MaximumType(%v, %v) = %v, want %v — a type is its own maximum", a, a, m, a)
		}
	}

	pairs := 0
	for _, a := range corpus {
		for _, b := range corpus {
			m := MaximumType(a, b)
			if m == nil {
				continue
			}
			// NullType is the type of the NULL literal: it is nullable by
			// construction and every maximum involving it is nullable too, so it
			// carries no information about the rule and is skipped rather than
			// special-cased into it.
			if a.Code() == TypeCodeNull || b.Code() == TypeCodeNull {
				continue
			}
			pairs++
			want := a.IsNullable() || b.IsNullable()
			if m.IsNullable() != want {
				t.Errorf("MaximumType(%v, %v) = %v: nullable=%v, want %v — the documented "+
					"rule is that the result is nullable iff either input is",
					a, b, m, m.IsNullable(), want)
			}
		}
	}
	// Measured: 124 non-NULL-literal pairs with a defined maximum.
	if pairs < 80 {
		t.Fatalf("the nullability rule was checked on only %d pairs — too few; the maximum "+
			"is now undefined for most of the corpus and this law is near-vacuous", pairs)
	}
}

// TestMaximumType_NullLiteralRuleIsPinnedIndependently states what MaximumType's
// dedicated NULL arms guarantee, because nothing else does and they are
// currently REDUNDANT.
//
// Measured: disabling all three of them — the NULL x NULL arm and both
// promotable arms — changes the answer for ZERO of 1225 (t1, t2) pairs spanning
// every primitive in both nullabilities plus ARRAY, RECORD (one and two fields),
// ENUM (two names), RELATION and NONE. The general promotion fallthrough at the
// end of the function reaches every one of those answers on its own, because
// `resultNullable` already ORs both sides and NullType is nullable.
//
// That is the same observation the NONE comment inside MaximumType makes about
// ITS arms ("Java needs dedicated arms; Go's promotion lattice below reaches the
// identical result"), applied to the NULL ones — where the arms were kept.
//
// They are redundant, NOT dead: they hold only because NullType is nullable and
// because promotionMap carries a NULL->T edge for each T. Remove either
// invariant and the arms start doing work the fallthrough cannot. So the rule
// gets its own assertions here rather than resting on a fallthrough that agrees
// with it by coincidence of two other decisions.
func TestMaximumType_NullLiteralRuleIsPinnedIndependently(t *testing.T) {
	t.Parallel()

	if m := MaximumType(NullType, NullType); m == nil || m.Code() != TypeCodeNull {
		t.Errorf("MaximumType(NULL, NULL) = %v, want the NULL type", m)
	}

	// NULL against a type it can promote to yields that type, made nullable —
	// in BOTH operand orders.
	for _, other := range []Type{
		NotNullInt, NotNullLong, NotNullFloat, NotNullDouble,
		NotNullBoolean, NotNullString, NotNullBytes,
	} {
		for _, pair := range [][2]Type{{NullType, other}, {other, NullType}} {
			m := MaximumType(pair[0], pair[1])
			if m == nil {
				t.Errorf("MaximumType(%v, %v) = nil, want %v made nullable", pair[0], pair[1], other)
				continue
			}
			if m.Code() != other.Code() {
				t.Errorf("MaximumType(%v, %v) = %v, want code %v", pair[0], pair[1], m, other.Code())
			}
			if !m.IsNullable() {
				t.Errorf("MaximumType(%v, %v) = %v, want it NULLABLE — a NULL literal on "+
					"either side makes the result nullable", pair[0], pair[1], m)
			}
		}
	}

	// NULL against a type it cannot promote to has no maximum. UUID is the case:
	// promotionMap carries STRING->UUID but no NULL->UUID edge, matching Java,
	// whose PhysicalOperator enum has NULL_TO_* for the primitives, ARRAY,
	// RECORD, ENUM, BYTES, VECTOR and VERSION — and not for UUID.
	for _, pair := range [][2]Type{{NullType, NotNullUuid}, {NotNullUuid, NullType}} {
		if m := MaximumType(pair[0], pair[1]); m != nil {
			t.Errorf("MaximumType(%v, %v) = %v, want nil — there is no NULL->UUID promotion "+
				"edge in either Go or Java", pair[0], pair[1], m)
		}
	}
}
