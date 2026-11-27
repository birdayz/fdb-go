// Portions derived from FoundationDB Record Layer (LikeOperatorValue.java,
// PatternForLikeValue.java),
// Copyright 2015-2022 Apple Inc. and the FoundationDB project authors
// Licensed under the Apache License, Version 2.0; translated to Go and modified.

package values

// LikeOperatorValue is Java's LikeOperatorValue: `probe LIKE pattern`, the
// pattern a PatternForLikeValue (LikeOperatorValue.java:83-105). NULL when
// the probe is NULL or the pattern absent.
type LikeOperatorValue struct {
	Probe   Value
	Pattern Value
}

// NewLikeOperatorValue constructs the LIKE Value.
func NewLikeOperatorValue(probe, pattern Value) *LikeOperatorValue {
	return &LikeOperatorValue{Probe: probe, Pattern: pattern}
}

// NewLikeOperatorValueChecked is Java's encapsulate
// (LikeOperatorValue.java:320-327): the probe NULL-typed or STRING, the
// pattern a PatternForLikeValue.
func NewLikeOperatorValueChecked(probe Value, pattern *PatternForLikeValue) (*LikeOperatorValue, error) {
	if !likeStringOperand(probe) || pattern == nil {
		return nil, &LikeError{Kind: LikeOperandNotString}
	}
	return NewLikeOperatorValue(probe, pattern), nil
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

// Evaluate is LikeOperatorValue.eval: the probe, then the pattern record,
// then likeOperation.
func (v *LikeOperatorValue) Evaluate(evalCtx any) (any, error) {
	if v.Probe == nil || v.Pattern == nil {
		return nil, nil
	}
	probe, err := v.Probe.Evaluate(evalCtx)
	if err != nil {
		return nil, err
	}
	pattern, err := v.Pattern.Evaluate(evalCtx)
	if err != nil {
		return nil, err
	}
	return LikeOperation(probe, pattern)
}
