package values

// QuantifiedRecordValue represents the entire QUERIED RECORD flowing
// from a Quantifier — the FDBQueriedRecord shape that carries the
// stored protobuf message plus version + primary-key metadata.
// Mirrors Java's
// `com.apple.foundationdb.record.query.plan.cascades.values.QuantifiedRecordValue`.
//
// Distinction from QuantifiedObjectValue:
//
//   - QuantifiedObjectValue evaluates to the OBJECT bound to the
//     alias — for record-typed quantifiers this is the proto
//     Message, for repeated-field unnest it's the inner element,
//     for non-record types it's the raw datum.
//   - QuantifiedRecordValue evaluates to the QUERIED RECORD
//     specifically — a record-shaped tuple of (storedRecord,
//     version, primaryKey). Java's eval calls
//     `binding.getQueriedRecord()` rather than `getMessage()` or
//     `getDatum()`.
//
// The two coexist because:
//   - Record-typed planner reasoning needs the metadata attached
//     to the record (version, PK); QuantifiedRecordValue carries
//     that intent.
//   - Per-message proto field access uses QuantifiedObjectValue and
//     descends via FieldValue chains.
//
// In the Go seed both Values evaluate compatibly through the row-
// shape harness — the runtime distinction surfaces when execution
// integration wires the actual FDBQueriedRecord-vs-Message
// dispatch. The TYPE-level distinction matters NOW for planner
// matchers that test for the QuantifiedRecordValue marker
// specifically (e.g. MatchCandidate-side rules that need the full
// queried record).
//
// Eval contract: returns the queried-record bound to `alias` in the
// eval context. The seed accepts a `map[string]any` keyed by alias
// name (sharing VersionValue / IncarnationValue's harness pattern);
// a nil / non-map / missing-key context returns nil.
type QuantifiedRecordValue struct {
	Alias      CorrelationIdentifier
	ResultType Type
}

// NewQuantifiedRecordValue constructs a record-flow placeholder
// bound to the given alias and typed at resultType.
//
// resultType is expected to be record-typed (the Java constructor
// admits any Type but downstream planner matchers select on
// resultType.isRecord()). The seed doesn't enforce — Type kind
// inspection is planner-side; constructor is permissive to keep
// the test surface honest.
func NewQuantifiedRecordValue(alias CorrelationIdentifier, resultType Type) *QuantifiedRecordValue {
	if resultType == nil {
		resultType = UnknownType
	}
	return &QuantifiedRecordValue{Alias: alias, ResultType: resultType}
}

// Children returns the empty slice — leaf, no operands.
func (*QuantifiedRecordValue) Children() []Value { return []Value{} }

// Name returns the debug-print kind.
func (*QuantifiedRecordValue) Name() string { return "qrv" }

// Type returns the bound record's type.
func (v *QuantifiedRecordValue) Type() Type { return v.ResultType }

// Evaluate looks up the queried record bound to alias in the eval
// context. Returns nil if evalCtx is nil or not a row-shape map.
func (v *QuantifiedRecordValue) Evaluate(evalCtx any) any {
	if evalCtx == nil {
		return nil
	}
	if m, ok := evalCtx.(map[string]any); ok {
		if rec, ok := m[v.Alias.Name()]; ok {
			return rec
		}
	}
	return nil
}

// GetCorrelatedTo returns the singleton set containing the bound
// alias — Java's QuantifiedValue contract surfaces the alias as a
// dataflow correlation.
func (v *QuantifiedRecordValue) GetCorrelatedTo() map[CorrelationIdentifier]struct{} {
	return map[CorrelationIdentifier]struct{}{v.Alias: {}}
}
