// Package plangen converts the embedded engine's LogicalOperator
// hierarchy into the Cascades-side RelationalExpression hierarchy.
// This is the seed of TODO Track C1 ("PlanGenerator: LogicalOperator
// → RelationalExpression adapter") — it bridges today's text-based
// logical builder to the new RelationalExpression hierarchy that the
// Cascades planner will operate on.
//
// Scope (seed): the simplest LogicalOperator types that have direct
// RelationalExpression equivalents. Operator types whose conversion
// requires re-parsing string projections / sort keys / aggregates
// from the LogicalOperator's text form into cascades.values.Value
// trees are deferred — they need the SQL parser threaded through,
// which is a bigger plumbing job (gated on the catalog-aware walker
// landing in C1's full scope).
//
// Currently supported:
//   - LogicalScan → FullUnorderedScanExpression
//   - LogicalFilter (Predicate non-nil) → LogicalFilterExpression
//   - LogicalUnion → LogicalUnionExpression (recursive)
//   - LogicalDelete → DeleteExpression (keyed by target table)
//
// Currently unsupported (returns ErrUnsupported):
//   - LogicalProject / LogicalSort — need text→Value parsing
//   - LogicalLimit — no RelationalExpression equivalent yet
//   - LogicalAggregate — needs GroupByExpression port
//   - LogicalJoin — maps to SelectExpression with multiple
//     Quantifiers; needs predicate placement work
//   - LogicalInsert / LogicalUpdate — need targetType inference
//   - LogicalValues / LogicalCTE / LogicalDDL — no equivalent
//   - LogicalFilter with PredicateText only (no QueryPredicate)
package plangen

import (
	"errors"
	"fmt"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/predicates"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
	"github.com/birdayz/fdb-record-layer-go/pkg/relational/core/query/logical"
)

// ErrUnsupported is returned by Convert for LogicalOperator types
// the seed adapter doesn't yet handle. Callers should fall back to
// the legacy text-based logical builder.
var ErrUnsupported = errors.New("plangen: operator type not yet supported")

// Convert returns the RelationalExpression equivalent of the given
// LogicalOperator tree. Returns ErrUnsupported (wrapped with the
// concrete type name) if any node in the tree isn't yet handled.
//
// The returned RelationalExpression's Quantifiers point at fresh
// Reference instances — this is a one-way conversion; the caller
// owns the resulting tree.
func Convert(op logical.LogicalOperator) (expressions.RelationalExpression, error) {
	if op == nil {
		return nil, errors.New("plangen: nil LogicalOperator")
	}
	switch o := op.(type) {
	case *logical.LogicalScan:
		return convertScan(o), nil
	case *logical.LogicalFilter:
		return convertFilter(o)
	case *logical.LogicalUnion:
		return convertUnion(o)
	case *logical.LogicalDelete:
		return convertDelete(o)
	default:
		return nil, fmt.Errorf("%w: %T", ErrUnsupported, op)
	}
}

// convertScan builds a FullUnorderedScanExpression over the
// LogicalScan's table name. The Alias is dropped — RelationalExpression
// uses a Quantifier to bind aliases at the next level up.
func convertScan(s *logical.LogicalScan) expressions.RelationalExpression {
	return expressions.NewFullUnorderedScanExpression([]string{s.Table}, values.UnknownType)
}

// convertFilter builds a LogicalFilterExpression over the recursively-
// converted child. Requires LogicalFilter.Predicate to be non-nil
// (the cascades QueryPredicate form); LogicalFilters built from the
// non-catalog-aware text path return ErrUnsupported.
func convertFilter(f *logical.LogicalFilter) (expressions.RelationalExpression, error) {
	if f.Predicate == nil {
		return nil, fmt.Errorf("%w: LogicalFilter without QueryPredicate (text-only path)", ErrUnsupported)
	}
	inner, err := Convert(f.Input)
	if err != nil {
		return nil, fmt.Errorf("filter input: %w", err)
	}
	q := expressions.ForEachQuantifier(expressions.InitialOf(inner))
	return expressions.NewLogicalFilterExpression(
		[]predicates.QueryPredicate{f.Predicate}, q,
	), nil
}

// convertUnion builds a LogicalUnionExpression over each recursively-
// converted child wrapped in a fresh Quantifier.
func convertUnion(u *logical.LogicalUnion) (expressions.RelationalExpression, error) {
	qs := make([]expressions.Quantifier, 0, len(u.Inputs))
	for i, child := range u.Inputs {
		conv, err := Convert(child)
		if err != nil {
			return nil, fmt.Errorf("union input %d: %w", i, err)
		}
		qs = append(qs, expressions.ForEachQuantifier(expressions.InitialOf(conv)))
	}
	return expressions.NewLogicalUnionExpression(qs), nil
}

// convertDelete builds a DeleteExpression over the recursively-
// converted child. The LogicalDelete's Target is the table name.
func convertDelete(d *logical.LogicalDelete) (expressions.RelationalExpression, error) {
	inner, err := Convert(d.Input)
	if err != nil {
		return nil, fmt.Errorf("delete input: %w", err)
	}
	q := expressions.ForEachQuantifier(expressions.InitialOf(inner))
	return expressions.NewDeleteExpression(q, d.Target), nil
}
