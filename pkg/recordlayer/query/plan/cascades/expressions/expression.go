// Portions derived from FoundationDB Record Layer (RelationalExpression.java,
// RelationalExpressionWithChildren.java, TranslationMap.java, MaxMatchMap.java,
// and others),
// Copyright 2015-2018 Apple Inc. and the FoundationDB project authors
// Copyright 2015-2024 Apple Inc. and the FoundationDB project authors
// Copyright 2015-2026 Apple Inc. and the FoundationDB project authors
// Licensed under the Apache License, Version 2.0; translated to Go and modified.

// Package expressions ports the Cascades-side relational expression
// hierarchy from Java's
// `com.apple.foundationdb.record.query.plan.cascades.expressions`.
//
// A RelationalExpression represents a node in the logical query plan
// tree — a stream of records with a known result Type. The hierarchy is
// the planner's working tree: each expression has zero or more children
// (modelled as Quantifiers ranging over References), a result Value
// describing the row shape it emits, and a small bundle of
// node-information fields specific to the operator.
//
// Concrete operators live one-per-file in this package: the Logical*
// rewrite surface (filter, projection, sort, distinct, unique, limit,
// type-filter, union, intersection, values), SelectExpression, the DML
// expressions (insert, update, delete), the leaf/scan expressions
// (FullUnorderedScan, Explode, TableFunction, TempTableScan), and the
// structural operators (GroupBy, MatchableSort, RecursiveUnion,
// TempTableInsert).
//
// Foundation types: Quantifier (ForEach kind), Reference (an
// equivalence class carrying exploratory + final member sets with
// EqualsWithoutChildren-and-children-aware dedup), AliasMap
// (CorrelationIdentifier bijection).
//
// Walk infrastructure: SemanticEquals and dependency-aware memo containment.
//
// Optional interface: RelationalExpressionWithPredicates — implemented
// by LogicalFilterExpression and SelectExpression for generic predicate-
// walker rules.
package expressions

import (
	"errors"
	"fmt"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// ErrQuantifierArity reports that a relational expression was rebuilt with a
// different number of child quantifiers. It is intentionally a stable sentinel
// so generic reconstruction callers can distinguish malformed rewrites without
// depending on an operator-specific error string.
var ErrQuantifierArity = errors.New("relational expression quantifier arity mismatch")

func requireQuantifierArity(operator string, got, want int) error {
	if got == want {
		return nil
	}
	return fmt.Errorf("%w: %s requires %d, got %d", ErrQuantifierArity, operator, want, got)
}

// RelationalExpression is the root interface for every node in the
// logical query plan tree. Implementations are immutable.
//
// Surface ported from Java's RelationalExpression:
//
//   - GetResultValue: the Value describing the rows this expression
//     emits. The Value's Type is necessarily a RelationType.
//   - GetQuantifiers: the children — every concrete operator returns
//     its inputs as a list of Quantifiers, in a stable order.
//   - CanCorrelate: whether this operator anchors a correlation (i.e.
//     whether evaluating one quantifier may bind values seen by
//     another). Defaults to false; SelectExpression and JOIN-shaped
//     expressions return true.
//   - GetCorrelatedToWithoutChildren: the set of CorrelationIdentifiers
//     this expression's node-information references (predicates,
//     projection list, sort key, etc.) — NOT including children's
//     correlations. Used by the planner to compute correlation order.
//   - EqualsWithoutChildren / HashCodeWithoutChildren: shape equality
//     of this node alone (predicate equality, type equality, …),
//     ignoring children. Two children are compared via SemanticEquals
//     under an alias map. Together they let the memo de-duplicate
//     equivalent expressions.
//
// The rest of the Java surface (TranslationMap rewriting, MaxMatchMap,
// findMatches, PartiallyOrderedSet correlation order) is deliberately
// not on this interface — Go carries that machinery as free-standing
// types and functions in the root cascades package (translation_map.go,
// max_match_map.go, the matching rules) rather than interface methods.
// PlannerGraph rendering has no Go equivalent (Explain output serves
// that role).
type RelationalExpression interface {
	// GetResultValue returns the Value whose Type describes the rows
	// this expression emits. For LogicalFilter this is the inner
	// Quantifier's flowed object value; for LogicalProjection it's a
	// RecordConstructor over the projection list; etc.
	GetResultValue() values.Value

	// GetQuantifiers returns the children of this expression in a
	// stable, defined order. The slice is read-only; callers must
	// not mutate it.
	GetQuantifiers() []Quantifier

	// CanCorrelate reports whether this expression anchors a
	// correlation between its quantifiers. For non-anchoring
	// expressions, evaluating one quantifier never binds values seen
	// by another. Defaults to false; only Select-shaped expressions
	// override.
	CanCorrelate() bool

	// GetCorrelatedToWithoutChildren returns the CorrelationIdentifiers
	// this expression's node-information depends on, NOT including
	// transitive correlations through children. Returned set is
	// read-only.
	GetCorrelatedToWithoutChildren() map[values.CorrelationIdentifier]struct{}

	// EqualsWithoutChildren reports whether this expression's
	// node-information matches `other`'s node-information, treating
	// CorrelationIdentifiers as equal under `aliases`. Children are
	// not consulted — that's the caller's job (typically by recursing
	// via SemanticEquals).
	EqualsWithoutChildren(other RelationalExpression, aliases *AliasMap) bool

	// HashCodeWithoutChildren is the structural hash of this node's
	// node-information, ignoring children. Must be consistent with
	// EqualsWithoutChildren under the empty alias map: x.Equals(y, ∅)
	// implies x.HashCode() == y.HashCode().
	HashCodeWithoutChildren() uint64

	// ChildrenAsSet reports whether this expression's children are
	// commutative — semantically equal regardless of order. Mirrors
	// Java's RelationalExpressionWithChildren.ChildrenAsSet marker
	// interface. When true, SemanticEquals enumerates child
	// permutations; when false, children are paired positionally.
	//
	// Default false. Overridden by LogicalUnion / LogicalIntersection
	// / SelectExpression — the operators whose children are SQL
	// set-bag-shaped or whose Java code marks them as ChildrenAsSet.
	ChildrenAsSet() bool

	// WithQuantifiers returns a copy of this expression with the given
	// quantifiers replacing the original children. The new quantifiers must have
	// the same arity and be in the same positional order as GetQuantifiers(). An
	// arity mismatch returns ErrQuantifierArity and no expression. Leaf
	// expressions accept only an empty slice and return themselves.
	//
	// Ports Java's RelationalExpression.withQuantifiers.
	WithQuantifiers(quantifiers []Quantifier) (RelationalExpression, error)
}

// SemanticEquals preserves the incoming node bindings and checks complete
// child populations. MemoEqual also allows renaming the root's local bindings.
func SemanticEquals(a, b RelationalExpression, aliases *AliasMap) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	if !a.EqualsWithoutChildren(b, aliases) {
		return false
	}
	return newMemoEquality().equal(a, b, aliases)
}

// MaxPermutationChildren is the former positional-fallback threshold.
// Deprecated: dependency-aware matching has no arity cutoff.
const MaxPermutationChildren = 8

// quantifierAttributesEqual reports whether two paired quantifiers agree on
// the semantics they carry THEMSELVES: kind, null-on-empty, strict-single.
// Java compares these in the quantifier's own equality (ForEach's
// `isNullOnEmpty == other.isNullOnEmpty`, Quantifier.java) — an edge
// attribute, not child content, so child recursion cannot see it. Without
// this check two selects differing only in a quantifier's nullOnEmpty — a
// LEFT-join box vs a plain INNER join — compare semantically EQUAL, and the
// memo interns/merges them as one, leaving whichever expression object
// arrived first as the authority for BOTH semantics (the LEFT-became-INNER
// wrong-rows class surfaced by RFC-186's deterministic winner choice).
func quantifierAttributesEqual(a, b Quantifier) bool {
	return a.Kind() == b.Kind() &&
		a.IsNullOnEmpty() == b.IsNullOnEmpty() &&
		a.IsStrictSingle() == b.IsStrictSingle()
}
