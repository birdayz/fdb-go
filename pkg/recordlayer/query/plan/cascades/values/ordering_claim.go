package values

import "strings"

// This file holds the ONE predicate every ordering-claim producer must ask
// before it states that a scan delivers rows in a column's order.
//
// An ordering claim is a promise that the PHYSICAL order in which a scan hands
// rows back equals the LOGICAL order the comparator imposes. For most column
// types those two orders are the same by construction: FDB tuple encoding is
// order-preserving, so the byte order of the packed key is the value order.
//
// FLOAT and DOUBLE break that identity, and they break it in a way no
// range-set can repair. Tuple encoding flips the sign bit of a non-negative
// double and flips every bit of a negative one, which lays the IEEE-754 domain
// out as:
//
//	negative NaN payloads < -Inf < … < -0.0 < +0.0 < … < +Inf < positive NaN payloads
//
// so NaN occupies TWO disjoint physical blocks, one at each end of the key
// space. The logical order disagrees on both counts: CompareFloat64 (faithful
// to java.lang.Double.compare, the Record Layer's ordering authority)
// canonicalizes every NaN bit pattern to a single value and ranks it GREATEST.
// A negative NaN is therefore the physically FIRST row and the logically LAST
// one.
//
// Two separate defects follow, and the second is why a range-set cannot fix
// this:
//
//  1. The column itself is misordered — a scan emits negative NaNs before -Inf
//     where the comparator wants them after +Inf.
//  2. All NaN payloads are ONE logical tie class spread across two disjoint
//     physical ranges. Any SUBSEQUENT sort column is ordered only within each
//     physical range, never across the tie class as a whole. So even visiting
//     both NaN blocks in the right order would not order the columns after it.
//
// Because of (2) a float coordinate cannot merely be reordered — it TERMINATES
// the ordering claim. Everything before it is still claimable; the float
// coordinate and everything after it is not.
//
// This is strictly about NaN, not about signed zero: -0.0 packs immediately
// before +0.0 and CompareFloat64 also ranks -0.0 below +0.0, so the two orders
// agree there. (A zero-valued float EQUALITY is a different matter — it spans
// both signed zeros and so does not pin a single key; that is handled
// separately by equalityPrefixLen/isZeroFloatEqualityRange in the plans
// package.)
//
// Java is unsound in exactly this way and we deliberately diverge; see
// DIVERGENCES.md.

// TypeTerminatesOrderingClaim returns true for FLOAT/DOUBLE because tuple NaN
// ordering differs from logical ordering. It ignores scan bounds; unknown types return false.
func TypeTerminatesOrderingClaim(t Type) bool {
	if t == nil {
		return false
	}
	switch t.Code() {
	case TypeCodeFloat, TypeCodeDouble:
		return true
	default:
		return false
	}
}

// ColumnCanExtendOrderingClaim resolves name against layout and reports whether
// that column may extend an ordering claim.
//
// Returns true when the column cannot be shown to be a FLOAT/DOUBLE — see
// TypeTerminatesOrderingClaim for why the burden of proof sits on that side.
//
// An AMBIGUOUS name (a layout declaring it twice, which is constructible
// because NewRecordType's duplicate check is case-SENSITIVE while column
// resolution is case-INSENSITIVE) terminates the claim if ANY matching field is
// a float. Addressability of an ambiguous key is a SEPARATE contract, already
// enforced downstream by the unique-match rule in bakeOrderingColumnIn — this
// predicate deliberately does not duplicate it, and answers only the question
// it owns: could this coordinate be a float?
func ColumnCanExtendOrderingClaim(layout Type, name string) bool {
	if layout == nil || name == "" {
		return true
	}
	rt, isRecord := layout.(*RecordType)
	if !isRecord || rt == nil || len(rt.Fields) == 0 {
		return true
	}
	for _, f := range rt.Fields {
		if strings.EqualFold(f.Name, name) && TypeTerminatesOrderingClaim(f.FieldType) {
			return false
		}
	}
	return true
}

// ColumnCouldBeFloat resolves name against layout and reports whether that
// coordinate COULD hold a float — the question a signed-zero widening decision
// needs, and deliberately NOT the negation of ColumnCanExtendOrderingClaim.
//
// The two differ exactly where the layout cannot answer, and the difference is
// the whole reason this exists. ColumnCanExtendOrderingClaim is permissive on an
// unresolvable layout (nil, not a *RecordType, no fields, name absent) because
// its own use is "may this coordinate extend a claim?", where permissive means
// GRANT and the burden of proof sits on the float side.
//
// Inverted into a float test that answer flips meaning: "not a float" becomes
// "no signed zero", which becomes "this equality PINS", which is assume-SOUND —
// the unsound direction. A burden-of-proof direction does not survive an
// inversion, and reading one predicate backwards to answer the other silently
// turned a conservative default into an optimistic one.
//
// That state is not hypothetical. plans.NewRecordQueryIndexPlan defaults a nil
// flowedType to UnknownType, and AggregateIndexMatchCandidate passes UnknownType
// explicitly; UnknownType is a *PrimitiveType, so it takes the not-a-record arm
// and every coordinate on such a plan reads as non-float. MEASURED live: a
// reachability probe in that arm fired under the planner suite.
//
// So this asks positively and fails CLOSED — an unresolvable coordinate could be
// a float, so a zero-capable equality on it does not pin and the claim is
// refused. The cost is a sort that may not have been needed; the alternative
// cost is a wrong row order.
func ColumnCouldBeFloat(layout Type, name string) bool {
	if layout == nil || name == "" {
		return true
	}
	rt, isRecord := layout.(*RecordType)
	if !isRecord || rt == nil || len(rt.Fields) == 0 {
		return true
	}
	for _, f := range rt.Fields {
		if strings.EqualFold(f.Name, name) {
			return TypeTerminatesOrderingClaim(f.FieldType)
		}
	}
	// Name absent from a layout that otherwise resolved: still unproven.
	return true
}

// ClaimableOrderingPrefix returns the leading name-resolved ordering prefix.
// Producers with typed key values use ClaimableTypedKeyPrefix instead.
func ClaimableOrderingPrefix(layout Type, names []string) int {
	for i, name := range names {
		if !ColumnCanExtendOrderingClaim(layout, name) {
			return i
		}
	}
	return len(names)
}

// ClaimableTypedKeyPrefix is ClaimableOrderingPrefix for keys that carry their
// OWN declared type and have no flowed layout to resolve against — grouping
// keys, which the translator mints already typed.
//
// A nil key is skipped rather than treated as terminating: an unidentifiable
// key is the same "burden of proof sits on the float side" trade
// TypeTerminatesOrderingClaim documents.
//
// Two DIFFERENT questions are answered by this one count, and it is worth
// naming both because only the first is about ordering:
//
//   - Does the producer's advertised ORDER hold? Only for the leading prefix,
//     so the claim is truncated there.
//   - Is the input CLUSTERED by the full grouping key? A streaming aggregation
//     compares each row against the PREVIOUS group only, which is sound exactly
//     when rows equal under the grouping identity are ADJACENT. A float
//     coordinate breaks that too, and more sharply: the grouping identity is
//     java.lang.Double.equals, which makes every NaN payload one value, while
//     the tuple encoding scatters those payloads into two blocks at OPPOSITE
//     ENDS of the key space. So the same group opens, closes and reopens, and
//     the aggregation emits it twice. A consumer asking that question needs the
//     count to reach len(keys) — a prefix is not enough, because clustering is
//     a property of the whole key.
func ClaimableTypedKeyPrefix(keys []Value) int {
	for i, k := range keys {
		if k == nil {
			continue
		}
		if TypeTerminatesOrderingClaim(k.Type()) {
			return i
		}
	}
	return len(keys)
}
