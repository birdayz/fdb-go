package values

// GetCorrelatedToOfValue walks v + its descendants and returns the
// union of every QuantifiedObjectValue's correlation. The result is a
// fresh map; callers may mutate freely.
//
// Returns nil for nil input. Returns an empty map (not nil) for trees
// that contain no QuantifiedObjectValue, so callers can rely on a
// non-nil map shape if they want to use map literal-syntax.
//
// Java's equivalent walks `Value.getCorrelatedTo()` which delegates to
// `getCorrelatedToWithoutChildren` + child sets. We don't have the
// per-Value-type methods yet (Correlated is implemented only on
// QuantifiedObjectValue today), so this single-walk implementation is
// the bridge until each Value type ports a per-impl method. When that
// happens this helper degrades naturally — collect already returns
// the same set, just with O(N) calls into each impl rather than one
// type-switch per node.
func GetCorrelatedToOfValue(v Value) map[CorrelationIdentifier]struct{} {
	if v == nil {
		return nil
	}
	out := map[CorrelationIdentifier]struct{}{}
	WalkValue(v, func(node Value) bool {
		if q, ok := node.(*QuantifiedObjectValue); ok {
			out[q.Correlation] = struct{}{}
		}
		return true
	})
	return out
}
