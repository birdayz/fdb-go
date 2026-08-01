package values

import "fmt"

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
// FuzzLikeMatch / FuzzLikeMatchEscape against a Java-semantics regex
// oracle; the spec is Java's `PatternForLikeValue.eval`
// (PatternForLikeValue.java:96-117) + `LikeOperatorValue.likeOperation`
// (LikeOperatorValue.java:93-99).
//
// Note: this Value-level LIKE carries no ESCAPE (escape rune = 0).
// ESCAPE support lives on the predicate layer (Comparison.Escape,
// predicates/comparisons.go) and in the shared LikeMatch helper —
// add the field here only if a Value-level ESCAPE consumer appears.
//
// STRUCTURAL divergence from Java, asserted rather than silent:
// Java's tree is `LikeOperatorValue(src, PatternForLikeValue(pat,
// esc))` — the pattern child mints a REGEX string which likeOperation
// then regex-compiles. Go's tree keeps the RAW SQL pattern as the
// child and evaluates it with values.LikeMatch, which implements the
// composed mint+find semantics directly (the cross-check test proves
// the equivalence). Feeding a PatternForLikeValue child here would
// hand a regex string to the SQL-pattern matcher and silently return
// wrong rows, so Evaluate rejects that shape with an explicit error
// — an asserted bridge, never a silent fallback. If a Java-shaped
// plan import ever needs to evaluate that composition, unwrap the
// PatternForLikeValue and lower its raw pattern + escape instead.
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
func (v *LikeOperatorValue) Evaluate(evalCtx any) (any, error) {
	if v.Probe == nil || v.Pattern == nil {
		return nil, nil
	}
	// A PatternForLikeValue child evaluates to a REGEX string
	// (Java-style assembly), not a raw SQL LIKE pattern; LikeMatch
	// consumes raw patterns only. Reject rather than mis-match.
	if _, mintsRegex := v.Pattern.(*PatternForLikeValue); mintsRegex {
		return nil, fmt.Errorf("LikeOperatorValue: pattern child is a PatternForLikeValue (regex-producing); lower the raw SQL pattern instead — LikeMatch implements the regex semantics itself")
	}
	probe, err := v.Probe.Evaluate(evalCtx)
	if err != nil {
		return nil, err
	}
	pattern, err := v.Pattern.Evaluate(evalCtx)
	if err != nil {
		return nil, err
	}
	if probe == nil || pattern == nil {
		return nil, nil
	}
	probeStr, ok := probe.(string)
	if !ok {
		return nil, nil
	}
	patternStr, ok := pattern.(string)
	if !ok {
		return nil, nil
	}
	// Delegate to the conformance-pinned LikeMatch — same matcher
	// the QueryPredicate-layer ComparisonLike uses, fuzz-tested
	// against a regex oracle modelling Java's
	// PatternForLikeValue.eval + LikeOperatorValue.likeOperation
	// composition.
	return LikeMatch(patternStr, probeStr, 0), nil
}
