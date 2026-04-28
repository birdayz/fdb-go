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
//   - LogicalUnion → LogicalUnionExpression (recursive); UNION
//     DISTINCT wraps with LogicalDistinctExpression
//   - LogicalDelete → DeleteExpression (keyed by target table)
//   - LogicalInsert (Source non-nil) → InsertExpression
//   - LogicalProject (bare-column projections only) →
//     LogicalProjectionExpression; non-bare-column entries fall
//     back to ErrUnsupported
//   - LogicalSort (bare-column keys only) → LogicalSortExpression
//   - LogicalUpdate (bare-column SET right-hand sides only) →
//     UpdateExpression; SET <col> = <expr> still ErrUnsupported
//
// Currently unsupported (returns ErrUnsupported):
//   - LogicalProject with expression projections — needs text→Value
//     parsing
//   - LogicalSort with expression keys — needs text→Value parsing
//   - LogicalUpdate with non-bare-column RHS — needs text→Value
//     parsing
//   - LogicalLimit — no RelationalExpression equivalent yet
//   - LogicalAggregate — needs GroupByExpression port
//   - LogicalJoin — maps to SelectExpression with multiple
//     Quantifiers; needs predicate placement work
//   - LogicalInsert without Source (VALUES literal) — needs a
//     synthetic LogicalValues source operator
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
	case *logical.LogicalInsert:
		return convertInsert(o)
	case *logical.LogicalProject:
		return convertProject(o)
	case *logical.LogicalSort:
		return convertSort(o)
	case *logical.LogicalUpdate:
		return convertUpdate(o)
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
// converted child wrapped in a fresh Quantifier. UNION DISTINCT
// (Distinct=true) wraps the union in a LogicalDistinctExpression —
// matches Java's planner shape (Union → Distinct over Union).
func convertUnion(u *logical.LogicalUnion) (expressions.RelationalExpression, error) {
	qs := make([]expressions.Quantifier, 0, len(u.Inputs))
	for i, child := range u.Inputs {
		conv, err := Convert(child)
		if err != nil {
			return nil, fmt.Errorf("union input %d: %w", i, err)
		}
		qs = append(qs, expressions.ForEachQuantifier(expressions.InitialOf(conv)))
	}
	union := expressions.NewLogicalUnionExpression(qs)
	if !u.Distinct {
		return union, nil
	}
	innerQ := expressions.ForEachQuantifier(expressions.InitialOf(union))
	return expressions.NewLogicalDistinctExpression(innerQ), nil
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

// convertInsert builds an InsertExpression over the recursively-
// converted Source. INSERT-VALUES (Source nil) is unsupported until
// we have a LogicalValues operator to feed in. Target type is left
// Unknown — the cascades typing pass fills it from the catalog later.
func convertInsert(i *logical.LogicalInsert) (expressions.RelationalExpression, error) {
	if i.Source == nil {
		return nil, fmt.Errorf("%w: LogicalInsert without Source (VALUES literal)", ErrUnsupported)
	}
	inner, err := Convert(i.Source)
	if err != nil {
		return nil, fmt.Errorf("insert input: %w", err)
	}
	q := expressions.ForEachQuantifier(expressions.InitialOf(inner))
	return expressions.NewInsertExpression(q, i.Table, values.UnknownType), nil
}

// convertProject builds a LogicalProjectionExpression for the
// recursively-converted child, but ONLY if every projection is a
// bare column name. Anything more (arithmetic, function call,
// scalar subquery, dotted reference) requires a text→Value parser
// that we don't have here yet.
//
// Aliases are dropped — the projection's column-naming is decided
// by the upstream catalog walker; the projection list captures
// only the value flow.
func convertProject(p *logical.LogicalProject) (expressions.RelationalExpression, error) {
	for i, pj := range p.Projections {
		if !isBareColumn(pj) {
			return nil, fmt.Errorf("%w: LogicalProject entry %d (%q) is not a bare column", ErrUnsupported, i, pj)
		}
	}
	inner, err := Convert(p.Input)
	if err != nil {
		return nil, fmt.Errorf("project input: %w", err)
	}
	q := expressions.ForEachQuantifier(expressions.InitialOf(inner))
	projected := make([]values.Value, len(p.Projections))
	for i, pj := range p.Projections {
		projected[i] = &values.FieldValue{Field: pj, Typ: values.UnknownType}
	}
	return expressions.NewLogicalProjectionExpression(projected, q), nil
}

// convertSort builds a LogicalSortExpression for the recursively-
// converted child, but ONLY if every sort-key Expr is a bare column
// name. Anything else (`ORDER BY a + b`, `ORDER BY UPPER(name)`,
// `ORDER BY t.c`) requires a text→Value parser we don't have yet.
//
// `LogicalSort{Keys: nil}` lowers to UnsortedLogicalSortExpression —
// matches the no-op case in Java.
func convertSort(s *logical.LogicalSort) (expressions.RelationalExpression, error) {
	for i, k := range s.Keys {
		if !isBareColumn(k.Expr) {
			return nil, fmt.Errorf("%w: LogicalSort key %d (%q) is not a bare column", ErrUnsupported, i, k.Expr)
		}
	}
	inner, err := Convert(s.Input)
	if err != nil {
		return nil, fmt.Errorf("sort input: %w", err)
	}
	q := expressions.ForEachQuantifier(expressions.InitialOf(inner))
	if len(s.Keys) == 0 {
		return expressions.UnsortedLogicalSortExpression(q), nil
	}
	keys := make([]expressions.SortKey, len(s.Keys))
	for i, k := range s.Keys {
		keys[i] = expressions.SortKey{
			Value:   &values.FieldValue{Field: k.Expr, Typ: values.UnknownType},
			Reverse: k.Dir == logical.SortDesc,
		}
	}
	return expressions.NewLogicalSortExpression(keys, q), nil
}

// convertUpdate builds an UpdateExpression for the recursively-
// converted child. Each SET assignment's RHS must be a bare column
// name (the rename-like case `UPDATE t SET a = b`); literals,
// arithmetic, and function calls all need text→Value parsing.
//
// The Input is required (no SET-from-nothing).
func convertUpdate(u *logical.LogicalUpdate) (expressions.RelationalExpression, error) {
	if u.Input == nil {
		return nil, fmt.Errorf("%w: LogicalUpdate without Input", ErrUnsupported)
	}
	for i, a := range u.Sets {
		if !isBareColumn(a.Expr) {
			return nil, fmt.Errorf("%w: LogicalUpdate SET %d (%s = %q) is not a bare-column RHS", ErrUnsupported, i, a.Column, a.Expr)
		}
	}
	inner, err := Convert(u.Input)
	if err != nil {
		return nil, fmt.Errorf("update input: %w", err)
	}
	q := expressions.ForEachQuantifier(expressions.InitialOf(inner))
	transforms := make([]expressions.UpdateTransform, len(u.Sets))
	for i, a := range u.Sets {
		transforms[i] = expressions.UpdateTransform{
			FieldPath: a.Column,
			NewValue:  &values.FieldValue{Field: a.Expr, Typ: values.UnknownType},
		}
	}
	return expressions.NewUpdateExpression(q, u.Target, transforms), nil
}

// isBareColumn reports whether s is a SQL identifier with no
// punctuation — letters/digits/underscore only, starting with a
// letter or underscore. The sql parser preserves casing, so we
// don't normalise here. Empty string is rejected.
func isBareColumn(s string) bool {
	if s == "" {
		return false
	}
	for i, r := range s {
		isLetter := (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || r == '_'
		isDigit := r >= '0' && r <= '9'
		if i == 0 && !isLetter {
			return false
		}
		if !isLetter && !isDigit {
			return false
		}
	}
	return true
}
