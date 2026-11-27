// Portions derived from FoundationDB Record Layer (LeafValue.java,
// CorrelationIdentifier.java, QuantifiedObjectValue.java),
// Copyright 2015-2020 Apple Inc. and the FoundationDB project authors
// Copyright 2015-2022 Apple Inc. and the FoundationDB project authors
// Licensed under the Apache License, Version 2.0; translated to Go and modified.

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
var _ LeafValue = (*quantifiedObjectValue)(nil)

// RebaseLeaf on QuantifiedObjectValue returns a new
// QuantifiedObjectValue with the target alias, preserving the type.
// Ports Java's QuantifiedObjectValue.rebaseLeaf.
func (q *quantifiedObjectValue) RebaseLeaf(targetAlias CorrelationIdentifier) Value {
	cp := *q
	cp.correlation = targetAlias
	return &cp
}
