package values

// LeafValue is the Go counterpart of Java's LeafValue interface — a
// scalar value type that has no children. LeafValues participate in
// translation/rebasing via RebaseLeaf, which returns a new Value with
// correlation identifiers updated to a target alias.
//
// Ports Java's
// com.apple.foundationdb.record.query.plan.cascades.values.LeafValue.
type LeafValue interface {
	Value

	// RebaseLeaf returns a new Value that is the same as this one but
	// with correlated identifiers updated to targetAlias. Returns this
	// if there are no correlated identifiers to update.
	//
	// Ports Java's LeafValue.rebaseLeaf(CorrelationIdentifier).
	RebaseLeaf(targetAlias CorrelationIdentifier) Value
}

// Compile-time interface satisfaction checks.
var _ LeafValue = (*QuantifiedObjectValue)(nil)

// RebaseLeaf on QuantifiedObjectValue returns a new
// QuantifiedObjectValue with the target alias, preserving the type.
// Ports Java's QuantifiedObjectValue.rebaseLeaf.
func (q *QuantifiedObjectValue) RebaseLeaf(targetAlias CorrelationIdentifier) Value {
	return &QuantifiedObjectValue{
		Correlation: targetAlias,
		Typ:         q.Typ,
	}
}
