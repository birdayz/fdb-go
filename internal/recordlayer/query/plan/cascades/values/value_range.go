// Portions derived from FoundationDB Record Layer (RangeValue.java,
// RecordCoreException.java, RecordType.java),
// Copyright 2015-2018 Apple Inc. and the FoundationDB project authors
// Copyright 2015-2025 Apple Inc. and the FoundationDB project authors
// Licensed under the Apache License, Version 2.0; translated to Go and modified.

package values

import "fmt"

// RangeValue is the built-in range(begin, end, step) table function (Java's
// RangeValue): a stream of rows (ID LONG) from beginInclusive up to but not
// including endExclusive. Children are LONG-promoted at construction.
type RangeValue struct {
	BeginInclusive Value
	EndExclusive   Value
	Step           Value
}

// rangeRowType is Java's currentRangeValue type: one non-null LONG column ID.
var rangeRowType = &RecordType{Fields: []Field{{Name: "ID", Ordinal: 0, FieldType: NotNullLong}}}

func NewRangeValue(begin, end, step Value) *RangeValue {
	return &RangeValue{BeginInclusive: begin, EndExclusive: end, Step: step}
}

// Children returns [begin, end, step].
func (r *RangeValue) Children() []Value {
	return []Value{r.BeginInclusive, r.EndExclusive, r.Step}
}

func (*RangeValue) Name() string { return "range" }

// Type is the per-row record type (ID LONG).
func (*RangeValue) Type() Type { return rangeRowType }

// Evaluate: a streaming value has no scalar result (Java throws).
func (*RangeValue) Evaluate(any) (any, error) { return nil, nil }

// RangeBoundsError is Java's RecordCoreException from Cursor.checkValidRange.
type RangeBoundsError struct{ Message string }

func (e *RangeBoundsError) Error() string { return e.Message }

// Bounds evaluates the three children and applies Java's checkValidRange.
func (r *RangeValue) Bounds(evalCtx any) (begin, end, step int64, err error) {
	vals := [3]int64{}
	for i, child := range []Value{r.BeginInclusive, r.EndExclusive, r.Step} {
		if child == nil {
			return 0, 0, 0, fmt.Errorf("range bound %d is unbound", i)
		}
		v, evalErr := child.Evaluate(evalCtx)
		if evalErr != nil {
			return 0, 0, 0, evalErr
		}
		n, ok := v.(int64)
		if !ok {
			return 0, 0, 0, fmt.Errorf("range bound %d evaluated to %T, want LONG", i, v)
		}
		vals[i] = n
	}
	begin, end, step = vals[0], vals[1], vals[2]
	return begin, end, step, CheckRangeBounds(begin, end, step)
}

func (*RangeBoundsError) JavaRecordCoreException() {}

// CheckRangeBounds is Java's RangeValue.Cursor.checkValidRange.
func CheckRangeBounds(position, end, step int64) error {
	switch {
	case position < 0:
		return &RangeBoundsError{Message: "only non-negative position is allowed in range"}
	case end < 0:
		return &RangeBoundsError{Message: "only non-negative exclusive end is allowed in range"}
	case step <= 0:
		return &RangeBoundsError{Message: "only positive step is allowed in range"}
	}
	return nil
}

// EvaluateAsStream materialises the range, or nil when the bounds are invalid.
func (r *RangeValue) EvaluateAsStream(evalCtx any) []int64 {
	begin, end, step, err := r.Bounds(evalCtx)
	if err != nil {
		return nil
	}
	out := []int64{}
	for v := begin; v < end; v += step {
		out = append(out, v)
	}
	return out
}

// Cardinality is Java's floorDiv(end - begin, step) over constant bounds.
func (r *RangeValue) Cardinality() (int64, bool) {
	begin, end, step, err := r.Bounds(nil)
	if err != nil {
		return -1, false
	}
	num := end - begin
	if (num < 0) != (step < 0) && num%step != 0 {
		return num/step - 1, true
	}
	return num / step, true
}
