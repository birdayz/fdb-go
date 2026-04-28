package values

// IncarnationValue is a LEAF Value that returns the record store's
// incarnation number — an integer counter the store advances whenever
// its versionstamp prefix is "fresh" (e.g. after a tenant move /
// re-key). Mirrors Java's
// `com.apple.foundationdb.record.query.plan.cascades.values.IncarnationValue`.
//
// SQL-callable as `get_versionstamp_incarnation()`. The underlying
// purpose is to let queries detect store-incarnation changes when
// processing versionstamp-keyed data — two versionstamps from
// different incarnations are not directly comparable, so a query
// that joins / dedups across versionstamp boundaries uses this to
// scope correctness.
//
// Result type: INT (NOT NULL — every store has an incarnation).
//
// Per Java's contract, eval requires a non-null FDBRecordStoreBase;
// the seed accepts an evalCtx that exposes an "incarnation" key (the
// row-shape harness pattern shared with VersionValue), and otherwise
// returns nil — the actual store-bound eval lands when execution
// integration wires `*FDBRecordStore.GetIncarnation()` (currently
// not yet exposed; the field exists in StoreHeader but isn't
// surfaced through a public method).
//
// Until then this Value is parser / planner / fuzz-reachable but
// does not have a runtime evaluator that touches a real store —
// matches the existing "non-evaluable yet" pattern of ObjectValue,
// QueriedValue, CardinalityValue.
type IncarnationValue struct{}

// NewIncarnationValue constructs the singleton-shape leaf.
//
// IncarnationValue carries no per-instance state; every constructed
// instance is semantically equal. Returning a freshly allocated
// pointer (rather than a package-level singleton) preserves the
// per-call allocation contract used throughout the cascades
// package — callers that compare via pointer-identity won't be
// confused by interned reuse.
func NewIncarnationValue() *IncarnationValue { return &IncarnationValue{} }

// Children returns the empty slice — leaf, no operands.
func (*IncarnationValue) Children() []Value { return []Value{} }

// Name returns the debug-print kind, matching Java's
// `get_versionstamp_incarnation` SQL function name.
func (*IncarnationValue) Name() string { return "get_versionstamp_incarnation" }

// Type returns NotNullInt — every record store has a non-null
// incarnation (zero is a valid initial value, not absence).
func (*IncarnationValue) Type() Type { return NotNullInt }

// Evaluate returns the incarnation from the evalCtx if present.
// Mirrors VersionValue's row-shape harness pattern: when evalCtx is
// a `map[string]any` the evaluator looks up the "incarnation" key.
//
// Returns nil if:
//   - evalCtx is nil.
//   - evalCtx is not a row-shape map.
//   - The map has no "incarnation" key.
//
// Real store-bound evaluation lands when execution integration
// surfaces FDBRecordStore.GetIncarnation() through the eval context.
func (*IncarnationValue) Evaluate(evalCtx any) any {
	if evalCtx == nil {
		return nil
	}
	if m, ok := evalCtx.(map[string]any); ok {
		if v, ok := m["incarnation"]; ok {
			return v
		}
	}
	return nil
}
