package values

// FirstOrDefaultStreamingValue is the streaming variant of
// FirstOrDefaultValue. Returns the first element produced by its
// streaming child Value, or the default Value if the stream is
// empty. Mirrors Java's
// `com.apple.foundationdb.record.query.plan.cascades.values.FirstOrDefaultStreamingValue`.
//
// Distinction from FirstOrDefaultValue: that variant operates on a
// fixed array (the child Value evaluates to []any). This streaming
// variant takes a streaming-shaped child (typically RangeValue or
// a Quantifier flowing rows) and pulls just the first element via
// the streaming protocol — useful when the child is potentially
// large but we only need the first row.
//
// Eval is a placeholder for now — the streaming protocol isn't
// fully wired (RangeValue's EvaluateAsStream is the closest
// counterpart). When the StreamingValue interface lands, eval here
// pulls childValue.evalAsStream(store, ctx).first() and falls back
// to onEmptyResultValue.Evaluate if absent.
//
// Result type: childValue.Type() — Java does the same; the stream
// element type IS the result type (we pull one element).
type FirstOrDefaultStreamingValue struct {
	ChildValue         Value
	OnEmptyResultValue Value
}

// NewFirstOrDefaultStreamingValue constructs the streaming first-
// or-default. Caller is responsible for ensuring childValue is a
// streaming-shaped Value (e.g. RangeValue); the seed doesn't
// type-enforce the streaming-marker check (Go's lack of a
// StreamingValue interface marker — the planner is expected to
// pre-validate).
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

// Evaluate is a placeholder — streaming eval needs the
// StreamingValue interface fully wired. The seed-level harness
// pulls EvaluateAsStream() if the child is a *RangeValue (the
// only streaming-shaped Value we have today); otherwise returns
// nil per the placeholder pattern.
//
// Real eval would call childValue.evalAsStream(store, ctx).first()
// and fall back to onEmptyResultValue if absent.
func (v *FirstOrDefaultStreamingValue) Evaluate(evalCtx any) any {
	if v.ChildValue == nil {
		return nil
	}
	// Special-case: RangeValue exposes EvaluateAsStream which we
	// can pull the first element from.
	if rv, ok := v.ChildValue.(*RangeValue); ok {
		stream := rv.EvaluateAsStream(evalCtx)
		if len(stream) > 0 {
			return stream[0]
		}
		// Empty stream — fall through to default.
	} else {
		// Non-RangeValue streaming child: gated on full StreamingValue
		// interface. Surface nil per the placeholder pattern.
		return nil
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
