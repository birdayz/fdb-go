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

// TypeTerminatesOrderingClaim reports whether a column of type t ends an
// ordering claim — i.e. whether its FDB tuple key order differs from the order
// CompareFloat64/compareOrdered impose. True for FLOAT and DOUBLE.
//
// The predicate is deliberately POSITIVE ("prove it is a float") rather than
// negative ("prove it is safe"). A type we cannot identify returns false, so an
// unidentified column keeps whatever claim the producer would otherwise make.
// That is a knowing trade: the alternative — treating every untyped column as
// claim-terminating — silently deletes sort elimination everywhere a layout is
// absent, including paths where the column is provably an integer. The
// soundness that matters is enforced where the type system is actually
// engaged, which on the SQL path is everywhere a column comes from a table.
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

// ClaimableOrderingPrefix returns how many of names (resolved against layout,
// in order) may be claimed as an ordering — the count of leading columns before
// the first one that terminates the claim.
//
// This is the single entry point for producers that build an ordering key list
// from a metadata column-NAME sequence. Asking it in one place is the point: a
// producer that re-implements the predicate at its own call site is how two
// derivations drift apart and classify the same column differently.
//
// It is NOT the only shape a producer comes in, and an earlier revision of this
// comment claimed it was ("the single entry point for every producer"). That
// was false, and the two producers it did not cover — the streaming aggregation
// and the aggregate index, whose ordering is over GROUPS rather than rows —
// were returning wrong rows on a real cluster while this file asserted they
// could not. A producer holding already-typed key VALUES asks
// TypeTerminatesOrderingClaim directly instead; the predicate is shared, the
// entry point is not. plans/ordering.go's header enumerates which producer
// asks which, and which need not ask at all.
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
