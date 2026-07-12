package query

import (
	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// UnfoldedProjectedExistsError signals that a translated plan tree carries a
// projected ExistsValue that is NOT folded into the result value of the
// SelectExpression owning its existential quantifier — i.e. the boolean would be
// evaluated ABOVE the FlatMap, without the existential binding live, and
// ExistsValue.Evaluate would silently return false (or, via a QOV fallback,
// phantom-true). That is a silent wrong result, so the production path rejects
// such a plan with ErrCodeUnsupportedQuery rather than shipping wrong rows
// (RFC-141 §8 safety guard).
//
// The cleanly-rejected shapes are the projected-EXISTS long tail the fold does
// not yet recognize (e.g. multiple existential quantifiers in one query, or a
// projection over a shape findExistsFilterUnderUnaryChain cannot fold through).
// They are correctness-preserving rejections, never wrong answers.
type UnfoldedProjectedExistsError struct {
	// Alias is the existential correlation the offending ExistsValue reads.
	Alias values.CorrelationIdentifier
}

func (e *UnfoldedProjectedExistsError) Error() string {
	return "projected EXISTS in this query shape is not yet supported"
}

// CheckProjectedExistsFolded is the RFC-141 §8 safety guard. Given the root
// Reference of a freshly translated (pre-planning) plan tree, it returns an
// *UnfoldedProjectedExistsError when any ExistsValue in the tree is positioned
// where its existential binding will NOT be live at eval time — the long-tail
// silent-wrong-result the projected-EXISTS fold's structural pattern-matching
// could otherwise let through.
//
// Mechanism (two passes over the expression tree, mirroring Java's structural
// invariant that an ExistsValue is only correct inside the resultValue of the
// SelectExpression whose existential quantifier it reads):
//
//  1. Build an ownership map: for every SelectExpression in the tree, for each
//     existential quantifier it declares, record alias -> that SelectExpression.
//     An existential alias is declared by exactly one SelectExpression (the one
//     the translator attaches the NamedExistentialQuantifier to).
//
//  2. For every expression in the tree, inspect the Value(s) it emits in its own
//     scope (its resultValue, or — for a LogicalProjectionExpression whose
//     resultValue is the inner's flowed object, not the projection — its
//     projected values) and find every ExistsValue. For each, resolve the
//     existential alias it reads (its QuantifiedObjectValue child's correlation)
//     and require that the emitting expression IS the SelectExpression that owns
//     that existential quantifier. If it is not (or no SelectExpression owns the
//     alias at all), the binding is dead at this position -> reject.
//
// Returns nil when every ExistsValue is correctly folded (the supported shapes:
// projected EXISTS / NOT EXISTS, correlated / non-correlated, alongside ORDER BY
// / LIMIT / a scalar subquery, and projected EXISTS over a JOIN in FROM — all
// fold the projection into the existential SelectExpression's result value).
func CheckProjectedExistsFolded(root *expressions.Reference) error {
	if root == nil {
		return nil
	}

	// Pass A: alias -> the SelectExpression that owns the existential quantifier.
	owner := map[values.CorrelationIdentifier]expressions.RelationalExpression{}
	for _, m := range root.Members() {
		expressions.Walk(m, func(e expressions.RelationalExpression) bool {
			if sel, ok := e.(*expressions.SelectExpression); ok {
				for _, q := range sel.GetQuantifiers() {
					if q.Kind() == expressions.QuantifierExistential {
						owner[q.GetAlias()] = sel
					}
				}
			}
			return true
		})
	}

	// Pass B: every ExistsValue must be emitted by its existential's owner.
	var badAlias values.CorrelationIdentifier
	found := false
	for _, m := range root.Members() {
		expressions.Walk(m, func(e expressions.RelationalExpression) bool {
			for _, emitted := range emittedScopeValues(e) {
				if emitted == nil {
					continue
				}
				values.WalkValue(emitted, func(node values.Value) bool {
					ev, ok := node.(*values.ExistsValue)
					if !ok {
						return true
					}
					alias, hasAlias := existsValueAlias(ev)
					// An ExistsValue with no resolvable QuantifiedObjectValue
					// child, or whose alias no owner declares, or whose owner is
					// some OTHER expression, all evaluate without a live binding.
					if !hasAlias || owner[alias] != e {
						found = true
						badAlias = alias
						return false
					}
					return true
				})
				if found {
					return false
				}
			}
			return !found
		})
		if found {
			break
		}
	}
	if found {
		return &UnfoldedProjectedExistsError{Alias: badAlias}
	}
	return nil
}

// emittedScopeValues returns every Value an expression computes in its OWN scope
// — the places a projected ExistsValue can land before planning. The guard must
// be exhaustive over these: an ExistsValue that escapes into a GROUP BY key, an
// aggregate operand, or a sort key (rather than a SelectExpression result value)
// is just as binding-dead as one in a Map above the FlatMap, and silently reads
// false. Per type:
//
//   - SelectExpression: its result value (the correct fold target).
//   - LogicalProjectionExpression: its projected values (its GetResultValue() is
//     the inner's flowed object, which would miss a projected ExistsValue).
//   - GroupByExpression: grouping keys + aggregate operands (`GROUP BY id,
//     EXISTS(...)` parks the ExistsValue in a grouping key).
//   - LogicalSortExpression: each sort key's Value.
//
// Every other expression's GetResultValue() is inspected too (cheap, and a
// belt-and-braces catch for any value-bearing shape not special-cased — its
// flowed-object result values contain no ExistsValue, so this never
// false-positives).
func emittedScopeValues(e expressions.RelationalExpression) []values.Value {
	switch ex := e.(type) {
	case *expressions.LogicalProjectionExpression:
		return ex.GetProjectedValues()
	case *expressions.GroupByExpression:
		vals := append([]values.Value{}, ex.GetGroupingKeys()...)
		for _, agg := range ex.GetAggregates() {
			vals = append(vals, agg.Operand)
		}
		return vals
	case *expressions.LogicalSortExpression:
		keys := ex.GetSortKeys()
		vals := make([]values.Value, 0, len(keys))
		for _, k := range keys {
			vals = append(vals, k.Value)
		}
		return vals
	default:
		return []values.Value{e.GetResultValue()}
	}
}

// BuriedExistentialPredicateError signals that a translated plan tree carries a
// WHERE existential predicate (an ExistentialValuePredicate) buried under a
// wrapper that is NOT a directly-handled semi-join shape — i.e. it is neither a
// top-level existential nor a single-NOT-wrapped existential. Such a predicate
// falls into the regular-predicate bucket of the NLJ rule's
// implementExistentialSelect / implementJoinWithExistential, where the empty
// FirstOrDefault inner emits its NULL default that no residual filter removes,
// so EVERY outer row silently passes (a silent wrong result). The production
// path rejects such a plan with ErrCodeUnsupportedQuery rather than ship wrong
// rows (RFC-141 R4 convergence backstop, P1a).
//
// The cleanly-rejected shapes are the wrapped-WHERE-EXISTS long tail: any
// existential reachable only through a wrapper the rule's IsExistentialPredicate
// / IsNotExistentialPredicate routing does not recognise — `WHERE NOT (NOT
// EXISTS(...))`, `WHERE EXISTS(...) OR p`, deeper AND/OR/NOT nesting. A plain
// `WHERE EXISTS` / `WHERE NOT EXISTS` (top-level or single-NOT-wrapped) is the
// directly-handled shape and is NOT rejected.
type BuriedExistentialPredicateError struct{}

func (e *BuriedExistentialPredicateError) Error() string {
	return "EXISTS in this query shape is not yet supported"
}

// CheckBuriedExistentialPredicate is the RFC-141 R4 convergence
// backstop for WHERE EXISTS (P1a). Given the root Reference of a freshly
// translated (pre-planning) plan tree, it returns a
// *BuriedExistentialPredicateError when any predicate-bearing expression carries
// an existential predicate that is NOT in a directly-handled position — i.e. an
// ExistentialValuePredicate buried inside a predicate that is neither the
// top-level existential nor a single-NOT-wrapped existential.
//
// EXISTS can appear at any depth in a WHERE predicate tree. Only a top-level
// (or single-NOT-wrapped) existential is the semi-join shape the NLJ rule lowers
// to a FirstOrDefault + residual filter; everything else falls into the regular
// bucket where the empty FOD's NULL default is never dropped and every outer row
// passes. Rather than point-handle each wrapper shape (which never converges),
// this structurally DETECTS any buried existential and rejects cleanly.
//
// Per predicate-bearing expression (SelectExpression / LogicalFilterExpression),
// each top-level predicate is classified:
//
//   - IsExistentialPredicate(p) → directly handled (bare EXISTS). OK.
//   - IsNotExistentialPredicate(p) → directly handled (single-NOT NOT-EXISTS). OK.
//   - otherwise, if p's subtree CONTAINS an ExistentialValuePredicate anywhere
//     (predicates.ContainsExistentialPredicate) → buried → REJECT.
//
// Returns nil when every existential predicate is in a directly-handled position
// (the supported WHERE-EXISTS / NOT-EXISTS shapes, including alongside ordinary
// non-existential conjuncts, multi-table inners, and projected EXISTS).
func CheckBuriedExistentialPredicate(root *expressions.Reference) error {
	if root == nil {
		return nil
	}
	found := false
	for _, m := range root.Members() {
		expressions.Walk(m, func(e expressions.RelationalExpression) bool {
			wp, ok := e.(expressions.RelationalExpressionWithPredicates)
			if !ok {
				return true
			}
			for _, p := range wp.GetPredicates() {
				if _, ok := predicates.IsExistentialPredicate(p); ok {
					continue
				}
				if _, ok := predicates.IsNotExistentialPredicate(p); ok {
					continue
				}
				// A buried existential survives in two forms: as an
				// ExistentialValuePredicate nested under a non-direct wrapper
				// (`NOT (NOT EXISTS)`, OR), OR — when the EXISTS sits inside a
				// scalar expression in the WHERE (`WHERE CASE WHEN EXISTS(...)
				// THEN 1 ELSE 0 END = 1`, `WHERE (EXISTS(...)) = true`) — as a raw
				// ExistsValue embedded in a predicate's operand value (the
				// ExistsValueToQueryPredicate bridge never fired). A direct
				// existential lowers to ExistentialValuePredicate whose operand is
				// a QuantifiedObjectValue, never an ExistsValue, so detecting any
				// ExistsValue in a predicate value tree is a precise "buried"
				// signal. Catch both.
				if predicates.ContainsExistentialPredicate(p) || predicateContainsExistsValue(p) {
					found = true
					return false
				}
			}
			return !found
		})
		if found {
			break
		}
	}
	if found {
		return &BuriedExistentialPredicateError{}
	}
	return nil
}

// predicateContainsExistsValue reports whether any predicate in the tree rooted
// at p carries a values.ExistsValue anywhere in its operand value tree. This is
// the value-side companion to predicates.ContainsExistentialPredicate: it catches
// an EXISTS that the WHERE walk lowered into a SCALAR expression (a CASE, a
// comparison, an arithmetic) rather than into an ExistentialValuePredicate, so
// the predicate's operand Value tree carries a raw ExistsValue. Such an EXISTS
// has no existential quantifier attached and evaluates to a constant (false) —
// a silent wrong result — so the guard rejects it (RFC-141 R4).
func predicateContainsExistsValue(p predicates.QueryPredicate) bool {
	found := false
	predicates.WalkPredicate(p, func(node predicates.QueryPredicate) bool {
		for _, v := range predicateOperandValues(node) {
			if v == nil {
				continue
			}
			values.WalkValue(v, func(n values.Value) bool {
				if _, ok := n.(*values.ExistsValue); ok {
					found = true
					return false
				}
				return true
			})
			if found {
				return false
			}
		}
		return !found
	})
	return found
}

// predicateOperandValues returns the operand Value(s) a predicate node carries
// directly (not its child predicates — WalkPredicate recurses those). Covers the
// value-bearing predicate types a WHERE clause can produce; unknown types
// contribute nothing (their children, if any, are still walked).
func predicateOperandValues(p predicates.QueryPredicate) []values.Value {
	switch pred := p.(type) {
	case *predicates.ComparisonPredicate:
		return []values.Value{pred.Operand, pred.Comparison.Operand}
	case *predicates.ValuePredicate:
		return []values.Value{pred.Value}
	case *predicates.ExistentialValuePredicate:
		return []values.Value{pred.Value}
	case *predicates.PredicateWithValueAndRanges:
		return []values.Value{pred.GetValue()}
	case *predicates.Placeholder:
		return []values.Value{pred.Value}
	}
	return nil
}

// existsValueAlias extracts the existential correlation an ExistsValue reads —
// the correlation of its QuantifiedObjectValue child. Returns (zero, false) when
// the child is not a QuantifiedObjectValue (a shape the guard treats as
// un-foldable, since it can't be matched to an owning existential quantifier).
func existsValueAlias(ev *values.ExistsValue) (values.CorrelationIdentifier, bool) {
	qov, ok := ev.GetChild().(*values.QuantifiedObjectValue)
	if !ok {
		return values.CorrelationIdentifier{}, false
	}
	return qov.Correlation, true
}
