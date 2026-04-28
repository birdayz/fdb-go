package values

// LikeOperatorValue is the Value-layer SQL `LIKE` operator: tests
// whether a string value matches a SQL LIKE pattern. Mirrors Java's
// `com.apple.foundationdb.record.query.plan.cascades.values.
// LikeOperatorValue`.
//
//	probe LIKE 'abc%'  ↔  LikeOperatorValue{Probe: probe, Pattern: 'abc%'}
//
// Why a Value-layer LIKE in addition to the predicate-layer
// ComparisonLike: rules that operate on the Value tree (e.g. fold
// a constant probe against a constant pattern, or extract a prefix
// for index-pushdown) need a Value-shaped node.
//
// SQL LIKE wildcards:
//   - `%` matches zero or more characters
//   - `_` matches exactly one character
//   - other characters match literally
//
// Delegates to the canonical LikeMatch helper (shared with the
// QueryPredicate-layer ComparisonLike). The matcher is pinned by
// FuzzLikeMatch / FuzzLikeMatchEscape against a regex oracle and
// matches Java's `Comparisons.likeMatcher` semantics.
//
// Note: ESCAPE clauses are accepted via the seed's no-escape
// default (escape rune = 0). Future LikeOperatorValue extension
// can carry an Escape field; the underlying LikeMatch already
// supports it.
//
// Evaluate semantics — Kleene 3VL:
//   - non-NULL probe + non-NULL pattern: true if pattern matches,
//     false otherwise.
//   - NULL probe OR NULL pattern: nil (UNKNOWN).
//   - Non-string probe: nil (type-degraded).
//
// Type is always nullable boolean.
type LikeOperatorValue struct {
	Probe   Value
	Pattern Value
}

// NewLikeOperatorValue constructs the LIKE Value.
func NewLikeOperatorValue(probe, pattern Value) *LikeOperatorValue {
	return &LikeOperatorValue{Probe: probe, Pattern: pattern}
}

// Children returns probe + pattern.
func (v *LikeOperatorValue) Children() []Value {
	out := make([]Value, 0, 2)
	if v.Probe != nil {
		out = append(out, v.Probe)
	}
	if v.Pattern != nil {
		out = append(out, v.Pattern)
	}
	return out
}

// Name returns the debug-print kind.
func (*LikeOperatorValue) Name() string { return "like" }

// Type is always nullable boolean (NULL propagation).
func (*LikeOperatorValue) Type() Type { return NullableBoolean }

// Evaluate computes probe LIKE pattern.
func (v *LikeOperatorValue) Evaluate(evalCtx any) any {
	if v.Probe == nil || v.Pattern == nil {
		return nil
	}
	probe := v.Probe.Evaluate(evalCtx)
	pattern := v.Pattern.Evaluate(evalCtx)
	if probe == nil || pattern == nil {
		return nil
	}
	probeStr, ok := probe.(string)
	if !ok {
		return nil
	}
	patternStr, ok := pattern.(string)
	if !ok {
		return nil
	}
	// Delegate to the conformance-pinned LikeMatch — same matcher
	// the QueryPredicate-layer ComparisonLike uses, fuzz-tested
	// against a regex oracle and Java's likeMatcher semantics.
	return LikeMatch(patternStr, probeStr, 0)
}
