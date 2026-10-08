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
func DeconstructRecord(v Value) ([]Value, error) {
	if v == nil {
		return nil, nil
	}
	// If v is itself a RecordConstructorValue, return its children
	// directly — they're already the per-field Values.
	if rc, ok := v.(*RecordConstructorValue); ok {
		out := make([]Value, len(rc.Fields))
		for i, f := range rc.Fields {
			out[i] = f.Value
		}
		return out, nil
	}
	// For other record-typed Values, build FieldValue accessors per
	// field by enumerating the *RecordType's Fields — mirroring
	// Java's behaviour. Record-typed Values whose Type is not a
	// *RecordType (erased / unknown shapes) return nil below.
	rt := v.Type()
	if rt == nil {
		return nil, nil
	}
	if rt.Code() != TypeCodeRecord {
		return nil, nil
	}
	// Type.Record has a Fields slice we can read.
	if rec, ok := rt.(*RecordType); ok {
		out := make([]Value, 0, len(rec.Fields))
		for i := range rec.Fields {
			request, err := FieldByOrdinal(i)
			if err != nil {
				return nil, err
			}
			field, err := ResolveFieldAccess(v, []FieldRequest{request})
			if err != nil {
				return nil, err
			}
			out = append(out, field)
		}
		return out, nil
	}
	return nil, nil
}
