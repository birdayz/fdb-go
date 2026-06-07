package values

// RecordTypeValue extracts the record-type discriminator from a
// record. Mirrors Java's
// `com.apple.foundationdb.record.query.plan.cascades.values.RecordTypeValue`.
//
// Used by:
//   - Type filters (TypeFilterExpression's predicate equivalent at
//     the Value layer): `recordType(record) IN ('OrderHistory',
//     'Order')` is rewritten to a TypeFilter scan.
//   - Index-pushdown rules that select an index based on which
//     record types the planner can serve via that index.
//
// The child Value must evaluate to a record-shaped object that
// carries a "_recordType" or similar discriminator field. Java
// gets the type-key from the FDBRecordStore's metadata; the seed
// extracts via map lookup for "_recordType" — the convention used
// by the Go embedded engine's row-shape.
//
// Type is non-null long (the record-type discriminator is an
// implicit int64 in Java; record-type names map to integer IDs).
// In practice the seed accepts either string or int64 returns.
type RecordTypeValue struct {
	Child Value
}

// NewRecordTypeValue constructs the extractor.
func NewRecordTypeValue(child Value) *RecordTypeValue {
	return &RecordTypeValue{Child: child}
}

// Children returns the single child.
func (v *RecordTypeValue) Children() []Value {
	if v.Child == nil {
		return []Value{}
	}
	return []Value{v.Child}
}

// Name returns the debug-print kind.
func (*RecordTypeValue) Name() string { return "recordtype" }

// Type returns NotNullLong — the record-type discriminator is
// always present on a valid record.
func (*RecordTypeValue) Type() Type { return NotNullLong }

// Evaluate is the error-returning twin (RFC-091).
func (v *RecordTypeValue) Evaluate(evalCtx any) (any, error) {
	if v.Child == nil {
		return nil, nil
	}
	rec, err := v.Child.Evaluate(evalCtx)
	if err != nil {
		return nil, err
	}
	if rec == nil {
		return nil, nil
	}
	if m, ok := rec.(map[string]any); ok {
		if rt, ok := m["_recordType"]; ok {
			return rt, nil
		}
	}
	return nil, nil
}
