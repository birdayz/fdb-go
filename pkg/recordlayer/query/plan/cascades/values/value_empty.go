package values

// EmptyValue represents an empty record (zero fields). Mirrors Java's
// `com.apple.foundationdb.record.query.plan.cascades.values.EmptyValue`.
//
// Used by:
//   - Default value for COUNT(*) over an empty set (returns 0; the
//     empty record is the unit element for COUNT).
//   - Insert/Update/Delete operations that don't return a row but
//     need a Value-shaped placeholder for the planner.
//   - Any rewrite that produces a "no-op" Value tree.
//
// Type is the empty RecordType (no fields, non-null).
//
// Evaluate returns nil (no fields → nothing to evaluate).
type EmptyValue struct{}

// NewEmptyValue returns the canonical EmptyValue. Callers can use
// this or the package-level `Empty` singleton interchangeably.
func NewEmptyValue() *EmptyValue { return &EmptyValue{} }

// Empty is the canonical EmptyValue instance. Callers should prefer
// this over allocating a new one — pointer identity makes equality
// checks O(1).
var Empty = &EmptyValue{}

// Children returns the empty slice — leaf.
func (*EmptyValue) Children() []Value { return []Value{} }

// Name returns the debug-print kind.
func (*EmptyValue) Name() string { return "empty" }

// Type returns an empty non-null RecordType.
func (*EmptyValue) Type() Type {
	return NewRecordType("", false, nil)
}

// Evaluate returns nil — empty record has no value to extract.
func (*EmptyValue) Evaluate(any) any { return nil }

// EvaluateErr is the error-returning twin (RFC-091). Never fails.
func (*EmptyValue) EvaluateErr(any) (any, error) { return nil, nil }
