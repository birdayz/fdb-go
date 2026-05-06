package values

// DeconstructRecord flattens a record-typed Value into its
// constituent field Values. Mirrors Java's
// `Values.deconstructRecord(Value)`.
//
// Used by planner rules that need to look at individual field
// expressions inside a record-shaped Value:
//
//   - For a RecordConstructorValue, returns the children Values
//     directly (one per field). Pointer-stable: if the caller
//     wants to rewrite a single field's Value, working with the
//     flat list and re-constructing avoids deep cloning.
//
//   - For any other record-typed Value (e.g. QuantifiedRecordValue,
//     QueriedValue, RecordTypeValue when wrapping a record), returns
//     FieldValue accessors keyed by the field name — one per record
//     field. The caller can substitute or simplify these
//     individually then re-construct.
//
// Returns nil if v is nil or its Type isn't a record. The caller
// is expected to check via the returned slice's length (zero =
// not-record OR record with zero fields, either of which is a
// degenerate case).
func DeconstructRecord(v Value) []Value {
	if v == nil {
		return nil
	}
	// If v is itself a RecordConstructorValue, return its children
	// directly — they're already the per-field Values.
	if rc, ok := v.(*RecordConstructorValue); ok {
		out := make([]Value, len(rc.Fields))
		for i, f := range rc.Fields {
			out[i] = f.Value
		}
		return out
	}
	// For other record-typed Values, build FieldValue accessors per
	// field. The seed reads the field-name list from a hypothetical
	// `RecordType` shape — without a richer Type system the Go port
	// can't enumerate the field names from v.Type() alone, so this
	// branch returns nil. When Type.Record gets real GetFields()
	// support, this branch can build [FieldValue(v, fields[0]),
	// FieldValue(v, fields[1]), ...] mirroring Java's behaviour.
	rt := v.Type()
	if rt == nil {
		return nil
	}
	if rt.Code() != TypeCodeRecord {
		return nil
	}
	// Type.Record has a Fields slice we can read.
	if rec, ok := rt.(*RecordType); ok {
		out := make([]Value, 0, len(rec.Fields))
		for _, f := range rec.Fields {
			out = append(out, &FieldValue{Field: f.Name, Typ: f.FieldType})
		}
		return out
	}
	return nil
}

// SimplifyAll batch-applies SimplifyValue to a list of Values.
// Mirrors Java's `Values.simplify(Iterable<Value>, ...)`.
//
// Returns a fresh slice of the same length as the input. The
// pointer-equality short-circuit IS preserved: if no Value
// changed, the returned slice contains the original pointers.
// Callers can detect "no fold happened" via deep slice-pointer
// equality.
func SimplifyAll(in []Value) []Value {
	out := make([]Value, len(in))
	for i, v := range in {
		out[i] = SimplifyValue(v)
	}
	return out
}
