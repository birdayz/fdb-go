package values

// IndexedValue is a leaf placeholder representing a column value
// bound to an indexed-key position. Mirrors Java's
// `com.apple.foundationdb.record.query.plan.cascades.values.IndexedValue`.
//
// Used by index-pushdown rules during pattern matching: a logical
// FieldValue can be matched against an IndexedValue placeholder
// to verify that the predicate's column corresponds to an indexed
// position in the candidate index.
//
// IndexedValue is a "non-evaluable" Value — it represents a
// position in an index key, not a computed result. Calling
// Evaluate panics so misuse surfaces loudly.
//
// The seed implementation ports the type structure + the panic-on-
// Evaluate contract. Pattern-matching against IndexedValue is the
// consumer; that consumer ports alongside MatchCandidate / index-
// equality rules in B5 Batch A's later phase.
type IndexedValue struct {
	ResultType Type
}

// NewIndexedValue constructs an IndexedValue with the given result
// Type. Pass UnknownType when the position's type isn't yet
// resolved.
func NewIndexedValue(resultType Type) *IndexedValue {
	if resultType == nil {
		resultType = UnknownType
	}
	return &IndexedValue{ResultType: resultType}
}

// Children returns the empty slice — leaf.
func (*IndexedValue) Children() []Value { return []Value{} }

// Name returns the debug-print kind.
func (*IndexedValue) Name() string { return "indexed" }

// Type returns the bound result type.
func (v *IndexedValue) Type() Type { return v.ResultType }

// Evaluate panics — IndexedValue is a placeholder, not a computable
// expression. The panic message is loud so misuse doesn't silently
// return nil and confuse downstream evaluators.
func (*IndexedValue) Evaluate(any) any {
	panic("IndexedValue.Evaluate: indexed-value placeholder is non-evaluable; pattern-match against it instead of evaluating")
}
