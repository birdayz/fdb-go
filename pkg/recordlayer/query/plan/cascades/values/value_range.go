package values

// RangeValue is the SQL range(begin, end, step) table-valued
// function — produces a stream of LONG values from
// `beginInclusive` (default 0) up to but not including `endExclusive`,
// stepped by `step` (default 1). Mirrors Java's
// `com.apple.foundationdb.record.query.plan.cascades.values.RangeValue`.
//
// Java's class is a STREAMING value (`StreamingValue` +
// `CreatesDynamicTypesValue`) — its primary eval path is
// `evalAsStream` which returns a `RecordCursor<QueryResult>`. The
// scalar `eval` method throws because per-row evaluation makes no
// sense for a table-function. The Go seed mirrors this: Evaluate
// returns nil per the placeholder pattern, and a separate
// EvaluateAsStream method materialises the finite range as `[]int64`
// for testability.
//
// Result type: a 1-column record with LONG-valued column "ID" — the
// row shape Java's currentRangeValue produces. The seed exposes the
// element type (NotNullLong) directly via Type since record-typed
// returns are awkward without StreamingValue / record-type
// sub-shape support; consumers that care about the row shape can
// wrap in a RecordConstructorValue.
//
// Cardinality is statically known when begin/end/step all evaluate
// to constants — useful for the cost model to estimate. The seed
// exposes Cardinality() with the same `floorDiv(end-begin, step)`
// formula Java uses.
type RangeValue struct {
	BeginInclusive Value
	EndExclusive   Value
	Step           Value
}

// NewRangeValue constructs a RangeValue. All three children are
// REQUIRED (Java's grammar lets begin and step default to 0 and 1
// respectively, but those defaults are added at the parser level —
// the constructed Value carries explicit children).
func NewRangeValue(begin, end, step Value) *RangeValue {
	return &RangeValue{
		BeginInclusive: begin,
		EndExclusive:   end,
		Step:           step,
	}
}

// Children returns [begin, end, step] in source order (matches
// Java's withChildren list ordering).
func (r *RangeValue) Children() []Value {
	return []Value{r.BeginInclusive, r.EndExclusive, r.Step}
}

// Name returns the SQL function name.
func (*RangeValue) Name() string { return "range" }

// Type returns NotNullLong — the element type of the produced range.
//
// Note: Java's getResultType() returns Type.Record (a 1-column
// record with LONG-valued "ID"). The Go seed exposes the element
// type directly because record-typed Type wrappers without proper
// StreamingValue support would force the seed to introduce the
// streaming infrastructure piecemeal. Wrapping in a
// RecordConstructorValue at the call site is the canonical way to
// produce the record-shaped row.
func (*RangeValue) Type() Type { return NotNullLong }

// Evaluate is a placeholder — RangeValue is a streaming Value;
// per-row eval makes no sense. Java throws IllegalStateException;
// the Go seed surfaces nil per the existing placeholder pattern.
//
// Use EvaluateAsStream for the materialised range expansion.
func (*RangeValue) Evaluate(any) any { return nil }

// EvaluateAsStream materialises the finite range as a slice of
// int64 elements: [begin, begin+step, begin+2*step, ...) up to but
// excluding `end`. Returns nil if any of the children evaluate to
// non-int64, or if the range is degenerate (step <= 0 with positive
// direction, etc.).
//
// Real Java RangeValue produces a streaming RecordCursor — the
// finite materialisation here is for tests + cost-model cardinality
// estimation. Production execution would route through a streaming
// integration (gated on StreamingValue port).
func (r *RangeValue) EvaluateAsStream(evalCtx any) []int64 {
	begin, ok := r.BeginInclusive.Evaluate(evalCtx).(int64)
	if !ok {
		return nil
	}
	end, ok := r.EndExclusive.Evaluate(evalCtx).(int64)
	if !ok {
		return nil
	}
	step, ok := r.Step.Evaluate(evalCtx).(int64)
	if !ok {
		return nil
	}
	if step == 0 {
		return nil // infinite loop guard — Java throws on step=0 too
	}
	if step > 0 && begin >= end {
		return nil // empty range (matches Java's empty-cursor case)
	}
	if step < 0 && begin <= end {
		return nil // empty range (negative step over rising bounds)
	}
	out := []int64{}
	for v := begin; (step > 0 && v < end) || (step < 0 && v > end); v += step {
		out = append(out, v)
	}
	return out
}

// Cardinality returns the static row count if all three children
// are constant-foldable to int64, else returns (-1, false).
//
// Mirrors Java's getCardinalities — used by the cost model when
// the planner has a RangeValue table function in scope and wants
// to size operators above it.
func (r *RangeValue) Cardinality() (int64, bool) {
	begin, ok := r.BeginInclusive.Evaluate(nil).(int64)
	if !ok {
		return -1, false
	}
	end, ok := r.EndExclusive.Evaluate(nil).(int64)
	if !ok {
		return -1, false
	}
	step, ok := r.Step.Evaluate(nil).(int64)
	if !ok || step == 0 {
		return -1, false
	}
	// floorDiv(end - begin, step), matching Java's Math.floorDiv.
	num := end - begin
	if (num < 0) != (step < 0) && num%step != 0 {
		return num/step - 1, true
	}
	return num / step, true
}
