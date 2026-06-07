package values

// VersionValue extracts the FDBRecordVersion (12-byte versionstamp +
// local) from a record. Mirrors Java's
// `com.apple.foundationdb.record.query.plan.cascades.values.VersionValue`.
//
// Used by VERSION-aware queries:
//   - SELECT version(record) FROM t
//   - WHERE version(record) > X (versionstamp range queries)
//   - ORDER BY version(record) (versionstamp-ordered scans)
//
// The child Value must evaluate to a record-shaped object that
// carries a "version" field — typically a QuantifiedObjectValue
// flowing FDBQueriedRecord. The seed extracts via map["version"]
// lookup; downstream consumers wire the actual FDBQueriedRecord
// type when execution lands.
//
// Type is nullable VERSION (12-byte composite). NULL when the
// record's version is unknown / unset.
type VersionValue struct {
	Child Value
}

// NewVersionValue constructs a VersionValue.
func NewVersionValue(child Value) *VersionValue { return &VersionValue{Child: child} }

// Children returns the single child Value.
func (v *VersionValue) Children() []Value {
	if v.Child == nil {
		return []Value{}
	}
	return []Value{v.Child}
}

// Name returns the debug-print kind.
func (*VersionValue) Name() string { return "version" }

// Type returns NullableVersion.
func (*VersionValue) Type() Type { return NullableVersion }

// Evaluate extracts the version from the child's evaluated value.
// The child is expected to produce a map with a "version" key (the
// seed's row-shape), or a struct with a similar accessor.
//
// Returns nil if:
//   - Child is nil-shaped or evaluates to nil.
//   - The evaluated record has no version field.
//   - The version field is itself nil.
//
// Returns the version (typically []byte or a 12-byte tuple) on
// success.
func (v *VersionValue) Evaluate(evalCtx any) any {
	res, err := v.EvaluateErr(evalCtx)
	if err != nil {
		panic(err)
	}
	return res
}

// EvaluateErr is the error-returning twin of Evaluate (RFC-091).
func (v *VersionValue) EvaluateErr(evalCtx any) (any, error) {
	if v.Child == nil {
		return nil, nil
	}
	rec, err := v.Child.EvaluateErr(evalCtx)
	if err != nil {
		return nil, err
	}
	if rec == nil {
		return nil, nil
	}
	// The seed's row-shape is map[string]any with field name keys.
	// Map lookup for "version" lifts the version bytes / tuple.
	if m, ok := rec.(map[string]any); ok {
		if ver, ok := m["version"]; ok {
			return ver, nil
		}
		return nil, nil
	}
	// Other row shapes (proto messages, structs) require dedicated
	// extractors per shape — wired in when execution lands.
	return nil, nil
}
