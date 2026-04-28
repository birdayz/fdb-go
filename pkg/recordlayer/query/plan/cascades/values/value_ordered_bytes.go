package values

// OrderedBytesDirection enumerates the four ordering modes Java's
// `TupleOrdering.Direction` supports — combinations of (ASC|DESC) ×
// (NULLS_FIRST|NULLS_LAST). Mirrors Java's enum verbatim so plan
// hashes / explain output diff cleanly across language boundaries.
type OrderedBytesDirection int

const (
	// OrderedBytesAscNullsFirst sorts ascending; NULL sorts BEFORE
	// any non-null value (lowest).
	OrderedBytesAscNullsFirst OrderedBytesDirection = iota
	// OrderedBytesAscNullsLast sorts ascending; NULL sorts AFTER any
	// non-null value (highest).
	OrderedBytesAscNullsLast
	// OrderedBytesDescNullsFirst sorts descending; NULL sorts BEFORE
	// (which becomes "highest" under DESC, since the iteration is
	// reversed).
	OrderedBytesDescNullsFirst
	// OrderedBytesDescNullsLast sorts descending; NULL sorts AFTER.
	OrderedBytesDescNullsLast
)

// String renders the direction for explain / debug print.
func (d OrderedBytesDirection) String() string {
	switch d {
	case OrderedBytesAscNullsFirst:
		return "ASC_NULLS_FIRST"
	case OrderedBytesAscNullsLast:
		return "ASC_NULLS_LAST"
	case OrderedBytesDescNullsFirst:
		return "DESC_NULLS_FIRST"
	case OrderedBytesDescNullsLast:
		return "DESC_NULLS_LAST"
	}
	return "INVALID"
}

// IsAscending reports whether the direction encodes an ASC ordering.
// Used by ordering-property analysis to determine whether the
// produced bytes preserve or invert the underlying value's natural
// ordering.
func (d OrderedBytesDirection) IsAscending() bool {
	return d == OrderedBytesAscNullsFirst || d == OrderedBytesAscNullsLast
}

// ToOrderedBytesValue encodes its child Value's evaluation as a
// FoundationDB-compatible ordered-bytes blob suitable for use as
// part of an index key. Mirrors Java's
// `com.apple.foundationdb.record.query.plan.cascades.values.ToOrderedBytesValue`.
//
// Used by index-key construction: the planner lowers a SQL
// ORDER-BY-expression-as-index-prefix to a chain of ToOrderedBytes
// applications — each column's ordering direction baked into the
// produced bytes so a forward FDB scan over the index produces rows
// in the requested SQL order.
//
// Java's eval calls `TupleOrdering.pack(Key.Evaluated.scalar(child),
// direction)` — packs the child as a 1-element FDB tuple, then
// applies direction-aware encoding (DESC inverts bits / reverses
// NULL placement). The Go seed defers eval to a future shift that
// wires `tuple.PackOrdered` (the equivalent Go API) — for now the
// Value-shape is reachable but eval returns nil per the
// "non-evaluable yet" placeholder pattern shared with VersionValue
// / IncarnationValue / ObjectValue.
//
// Result type: NotNullBytes. Even when the child is NULL, the
// encoding produces a sentinel byte sequence, so the byte output is
// always populated.
type ToOrderedBytesValue struct {
	Child     Value
	Direction OrderedBytesDirection
}

// NewToOrderedBytesValue constructs the encoder.
func NewToOrderedBytesValue(child Value, direction OrderedBytesDirection) *ToOrderedBytesValue {
	return &ToOrderedBytesValue{Child: child, Direction: direction}
}

// Children returns the single child Value.
func (v *ToOrderedBytesValue) Children() []Value {
	if v.Child == nil {
		return []Value{}
	}
	return []Value{v.Child}
}

// Name returns the SQL function name.
func (*ToOrderedBytesValue) Name() string { return "to_ordered_bytes" }

// Type returns NotNullBytes — the encoder produces bytes regardless
// of input.
func (*ToOrderedBytesValue) Type() Type { return NotNullBytes }

// Evaluate is currently a placeholder — returns nil. Real eval
// wires tuple.PackOrdered (the Go equivalent of Java's
// TupleOrdering.pack). The Value-shape is reachable for
// planner / matcher / serialisation work today; runtime
// integration lands when index-key-construction port reaches
// this branch.
func (*ToOrderedBytesValue) Evaluate(any) any { return nil }

// CreateInverse returns the FromOrderedBytesValue that decodes the
// ordered-bytes form back to the original value. Java's
// createInverseValueMaybe always returns Optional.of — the encoding
// is always invertible. We expose the inverse as a method for
// matchers / rules that need to canonicalise To→From chains.
//
// Per Java's signature, the inverse takes a NEW child (the
// ordered-bytes input the inverse decodes) and the ORIGINAL
// child's result type (so the inverse knows what type to decode
// to).
func (v *ToOrderedBytesValue) CreateInverse(newChild Value, originalType Type) *FromOrderedBytesValue {
	return NewFromOrderedBytesValue(newChild, v.Direction, originalType)
}

// FromOrderedBytesValue decodes an ordered-bytes blob (the output
// of ToOrderedBytesValue) back to the original typed value.
// Mirrors Java's
// `com.apple.foundationdb.record.query.plan.cascades.values.FromOrderedBytesValue`.
//
// The decoder needs the encoded direction (so it knows whether to
// undo the DESC inversion) and the target type (so it knows what
// Tuple element type to extract). Java's class carries both fields.
//
// As the inverse of ToOrderedBytesValue, this Value typically
// appears in covering-index / index-only access plans where the
// planner has rewritten a SQL projection to read the encoded form
// from an index entry.
type FromOrderedBytesValue struct {
	Child      Value
	Direction  OrderedBytesDirection
	TargetType Type
}

// NewFromOrderedBytesValue constructs the decoder.
func NewFromOrderedBytesValue(child Value, direction OrderedBytesDirection, targetType Type) *FromOrderedBytesValue {
	if targetType == nil {
		targetType = UnknownType
	}
	return &FromOrderedBytesValue{
		Child:      child,
		Direction:  direction,
		TargetType: targetType,
	}
}

// Children returns the single child Value.
func (v *FromOrderedBytesValue) Children() []Value {
	if v.Child == nil {
		return []Value{}
	}
	return []Value{v.Child}
}

// Name returns the SQL function name.
func (*FromOrderedBytesValue) Name() string { return "from_ordered_bytes" }

// Type returns the target type — the decoded value's natural type.
// Note: Java's getResultType() returns the target type made nullable,
// since decoding may produce NULL for the NULL-sentinel byte
// sequence. The Go seed mirrors that — wraps target in nullable.
func (v *FromOrderedBytesValue) Type() Type {
	if v.TargetType == nil {
		return UnknownType
	}
	return WithNullability(v.TargetType, true)
}

// Evaluate is currently a placeholder — returns nil. Real eval
// wires tuple.UnpackOrdered (the Go equivalent of Java's
// TupleOrdering.unpack). Same gating as ToOrderedBytesValue.
func (*FromOrderedBytesValue) Evaluate(any) any { return nil }
