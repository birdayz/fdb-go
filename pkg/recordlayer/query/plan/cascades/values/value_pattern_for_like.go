// Portions derived from FoundationDB Record Layer (PatternForLikeValue.java,
// LikeOperatorValue.java),
// Copyright 2015-2022 Apple Inc. and the FoundationDB project authors
// Licensed under the Apache License, Version 2.0; translated to Go and modified.

package values

import "unicode/utf16"

// PatternForLikeValue is Java's PatternForLikeValue: a LIKE pattern and its
// escape, evaluated into a two-field record — field 1 the pattern, field 2
// the escape, each absent when NULL (PatternForLikeValue.java:92-131). The
// escape is validated whenever it is present, the pattern against it only
// when both are.
type PatternForLikeValue struct {
	PatternChild Value
	EscapeChild  Value
}

// LikePattern is a PatternForLikeValue's result, Java's record: a nil
// field is an absent one.
type LikePattern struct {
	Pattern *string
	Escape  *string
}

// patternForLikeType is PatternForLikeValue.TYPE: a record of two nullable
// strings.
var patternForLikeType = NewRecordType("", false, []Field{
	{FieldType: NullableString},
	{FieldType: NullableString},
})

// NewPatternForLikeValue is the value over a pattern and an escape; an
// absent ESCAPE is a NULL escape child, as Java's visitor builds it.
func NewPatternForLikeValue(pattern, escape Value) *PatternForLikeValue {
	return &PatternForLikeValue{PatternChild: pattern, EscapeChild: escape}
}

// NewPatternForLikeValueChecked is Java's encapsulate
// (PatternForLikeValue.java:231-239): the pattern and the escape are each
// NULL-typed or STRING.
func NewPatternForLikeValueChecked(pattern, escape Value) (*PatternForLikeValue, error) {
	if !likeStringOperand(pattern) || !likeStringOperand(escape) {
		return nil, &LikeError{Kind: LikeOperandNotString}
	}
	return NewPatternForLikeValue(pattern, escape), nil
}

// likeStringOperand is Java's `type.isNull() || type code STRING`. An
// unresolved type passes: it is decided at evaluation.
func likeStringOperand(v Value) bool {
	if v == nil {
		return false
	}
	t := v.Type()
	if t == nil {
		return true
	}
	switch t.Code() {
	case TypeCodeString, TypeCodeNull, TypeCodeUnknown:
		return true
	}
	return false
}

// Children returns [pattern, escape].
func (v *PatternForLikeValue) Children() []Value {
	return []Value{v.PatternChild, v.EscapeChild}
}

// Name returns the SQL function name.
func (*PatternForLikeValue) Name() string { return "patternForLike" }

// Type is the two-field record.
func (*PatternForLikeValue) Type() Type { return patternForLikeType }

// Evaluate is PatternForLikeValue.eval: the pattern is evaluated first, then
// the escape, which is validated when present, and the pattern against it
// when both are.
func (v *PatternForLikeValue) Evaluate(evalCtx any) (any, error) {
	var out LikePattern
	pattern, err := evaluateLikeString(v.PatternChild, evalCtx)
	if err != nil {
		return nil, err
	}
	out.Pattern = pattern
	escape, err := evaluateLikeString(v.EscapeChild, evalCtx)
	if err != nil {
		return nil, err
	}
	if escape != nil {
		unit, err := validateLikeEscape(*escape)
		if err != nil {
			return nil, err
		}
		if pattern != nil {
			if err := validateLikePattern(utf16.Encode([]rune(*pattern)), unit); err != nil {
				return nil, err
			}
		}
		out.Escape = escape
	}
	return out, nil
}

func evaluateLikeString(v Value, evalCtx any) (*string, error) {
	if v == nil {
		return nil, nil
	}
	raw, err := v.Evaluate(evalCtx)
	if err != nil || raw == nil {
		return nil, err
	}
	s, ok := raw.(string)
	if !ok {
		return nil, &LikeError{Kind: LikeOperandNotString}
	}
	return &s, nil
}

// LikeOperation is LikeOperatorValue.likeOperation
// (LikeOperatorValue.java:93-105): NULL for a NULL operand, a NULL pattern
// record or an absent pattern; otherwise the match.
func LikeOperation(operand any, pattern any) (any, error) {
	if operand == nil || pattern == nil {
		return nil, nil
	}
	lp, ok := pattern.(LikePattern)
	if !ok {
		return nil, &LikeError{Kind: LikeOperandNotString}
	}
	if lp.Pattern == nil {
		return nil, nil
	}
	text, ok := operand.(string)
	if !ok {
		// Typing admits a string operand only, and the Go-only DATE and
		// TIMESTAMP operands, which do not evaluate to a string: UNKNOWN.
		return nil, nil
	}
	return MatchLike(text, *lp.Pattern, lp.Escape)
}
