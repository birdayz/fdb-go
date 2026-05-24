package values

// StreamingValue extends Value with streaming evaluation. Mirrors
// Java's StreamingValue interface — Values that can produce a stream
// of elements rather than a single scalar result.
type StreamingValue interface {
	Value
	EvaluateAsStream(evalCtx any) []any
}

// FirstOrDefaultStreamingValue is the streaming variant of
// FirstOrDefaultValue. Returns the first element produced by its
// streaming child Value, or the default Value if the stream is
// empty. Mirrors Java's
// `com.apple.foundationdb.record.query.plan.cascades.values.FirstOrDefaultStreamingValue`.
//
// The child should implement StreamingValue. RangeValue returns
// []int64 (not []any), so it doesn't satisfy StreamingValue and
// is handled by an explicit type-switch fallback in Evaluate.
type FirstOrDefaultStreamingValue struct {
	ChildValue         Value
	OnEmptyResultValue Value
}

// NewFirstOrDefaultStreamingValue constructs the streaming first-
// or-default. The childValue should implement StreamingValue or be a
// *RangeValue (adapted internally).
func NewFirstOrDefaultStreamingValue(childValue, onEmpty Value) *FirstOrDefaultStreamingValue {
	return &FirstOrDefaultStreamingValue{
		ChildValue:         childValue,
		OnEmptyResultValue: onEmpty,
	}
}

// Children returns [childValue, onEmptyResultValue].
func (v *FirstOrDefaultStreamingValue) Children() []Value {
	return []Value{v.ChildValue, v.OnEmptyResultValue}
}

// Name returns the SQL function name (matches FirstOrDefaultValue).
func (*FirstOrDefaultStreamingValue) Name() string { return "firstOrDefault" }

// Type returns the child's type. The stream element type IS the
// result type (we pull one element, fall back to default which
// must be type-compatible).
func (v *FirstOrDefaultStreamingValue) Type() Type {
	if v.ChildValue == nil {
		return UnknownType
	}
	return v.ChildValue.Type()
}

// Evaluate pulls the first element from the streaming child, or
// returns the default value if the stream is empty.
func (v *FirstOrDefaultStreamingValue) Evaluate(evalCtx any) any {
	if v.ChildValue == nil {
		return nil
	}
	var stream []any
	if sv, ok := v.ChildValue.(StreamingValue); ok {
		stream = sv.EvaluateAsStream(evalCtx)
	} else if rv, ok := v.ChildValue.(*RangeValue); ok {
		for _, val := range rv.EvaluateAsStream(evalCtx) {
			stream = append(stream, val)
		}
	} else {
		return nil
	}
	if len(stream) > 0 {
		return stream[0]
	}
	if v.OnEmptyResultValue == nil {
		return nil
	}
	return v.OnEmptyResultValue.Evaluate(evalCtx)
}

// WithChildren returns a fresh FirstOrDefaultStreamingValue with
// the given children. Caller passes exactly 2 children
// (childValue, onEmptyResultValue).
func (v *FirstOrDefaultStreamingValue) WithChildren(newChildren []Value) *FirstOrDefaultStreamingValue {
	if len(newChildren) < 2 {
		// Insufficient children; return a copy with whatever's available.
		var c, o Value
		if len(newChildren) > 0 {
			c = newChildren[0]
		}
		if len(newChildren) > 1 {
			o = newChildren[1]
		}
		return NewFirstOrDefaultStreamingValue(c, o)
	}
	return NewFirstOrDefaultStreamingValue(newChildren[0], newChildren[1])
}
