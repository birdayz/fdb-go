package plans

import "fdb.dev/pkg/recordlayer/query/plan/cascades/values"

// producerOwnedCorrelations is the set of roots a producer program may
// legitimately be asked to claim a slot for.
//
// The producer bridge selects an output slot for a value, and it has two ways
// to do it: an OWNERSHIP proof (the requested root is one this producer's
// program actually reads, so the right slot exists here and can be found by
// correlation) and a NAME fallback (one slot happens to carry the same accessor
// name and leaf type). The fallback is what shipped three wrong-answer bugs —
// A.VAL and B.VAL both reading A's slot, an EXISTS answering on another leg's
// ID, a sort key read under an aliased column's label — each patched by adding
// another guard to the same heuristic.
//
// Passing an owned set is what makes the fallback unreachable for a root this
// producer has no evidence for: ReanchorOwnedValueThroughProducer returns such
// a value BYTE-FOR-BYTE UNCHANGED rather than guessing, and an unresolved root
// then fails loudly downstream instead of silently reading the wrong column.
// That is the correct direction — a value this producer cannot place is
// unknown, not "whatever is next door".
//
// The set is deliberately exactly two things: the current carrier (a producer
// always owns its own output row) and the roots the producer program itself
// references. Notably it is NOT "the operator's declared legs". A leg the
// operator declares but whose producer never reads it has no slot that can
// denote it, so correlation matching is guaranteed to miss and the only thing
// admitting it buys is another trip through the name fallback — the exact
// false accept this set exists to close. Where a declared leg IS readable it
// is already here, because the program reads it.
func producerOwnedCorrelations(producer values.Value) map[values.CorrelationIdentifier]struct{} {
	owned := map[values.CorrelationIdentifier]struct{}{
		values.CurrentCorrelation(): {},
	}
	if producer != nil {
		for correlation := range values.GetCorrelatedToOfValue(producer) {
			if !correlation.IsZero() {
				owned[correlation] = struct{}{}
			}
		}
	}
	return owned
}
