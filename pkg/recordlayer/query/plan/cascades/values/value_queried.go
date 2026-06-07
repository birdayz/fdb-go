package values

// QueriedValue is a leaf placeholder representing the FROM-clause
// row context (the "what's been queried"). Mirrors Java's
// `com.apple.foundationdb.record.query.plan.cascades.values.QueriedValue`.
//
// Used by the planner during semantic resolution: a SELECT *
// projection lowers to QueriedValue + RecordConstructor over the
// queried record types' fields. The Value is non-evaluable —
// specialized planner rewrites resolve it to concrete Field /
// QuantifiedObject values before reaching the row-eval layer.
//
// The seed accepts an optional record-type-name list and a result
// Type. Both are advisory; the actual type comes from the queried
// record store's metadata at execution time.
type QueriedValue struct {
	RecordTypes []string
	ResultType  Type
}

// NewQueriedValue constructs the placeholder.
func NewQueriedValue(recordTypes []string, resultType Type) *QueriedValue {
	if resultType == nil {
		resultType = UnknownType
	}
	return &QueriedValue{
		RecordTypes: dedupSortedRecordTypes(recordTypes),
		ResultType:  resultType,
	}
}

// dedupSortedRecordTypes returns the input slice sorted +
// deduped. Returns nil for empty input.
func dedupSortedRecordTypes(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	out := make([]string, len(in))
	copy(out, in)
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j-1] > out[j]; j-- {
			out[j-1], out[j] = out[j], out[j-1]
		}
	}
	w := 1
	for r := 1; r < len(out); r++ {
		if out[r] != out[w-1] {
			out[w] = out[r]
			w++
		}
	}
	return out[:w]
}

// Children returns the empty slice — leaf.
func (*QueriedValue) Children() []Value { return []Value{} }

// Name returns the debug-print kind.
func (*QueriedValue) Name() string { return "queried" }

// Type returns the bound result type.
func (v *QueriedValue) Type() Type { return v.ResultType }

// Evaluate returns nil — QueriedValue is a placeholder. Specialized
// planner rewrites resolve it to concrete Field / QuantifiedObject
// values before any row-level eval.
func (*QueriedValue) Evaluate(any) (any, error) { return nil, nil }
