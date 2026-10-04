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
//     scope and find every ExistsValue. For each, resolve the
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

// BuriedExistentialPredicateError reports an ExistsValue that was not lowered
// to a boolean predicate consumer before planning.
type BuriedExistentialPredicateError struct{}

func (e *BuriedExistentialPredicateError) Error() string {
	return "EXISTS in this query shape is not yet supported"
}

// CheckBuriedExistentialPredicate checks consumer ownership through boolean
// connectives and rejects unlowered scalar ExistsValues.
func CheckBuriedExistentialPredicate(root *expressions.Reference) error {
	if root == nil {
		return nil
	}
	found := false
	var dangling error
	for _, m := range root.Members() {
		expressions.Walk(m, func(e expressions.RelationalExpression) bool {
			wp, ok := e.(expressions.RelationalExpressionWithPredicates)
			if !ok {
				return true
			}
			owned := make(map[values.CorrelationIdentifier]struct{}, len(e.GetQuantifiers()))
			for _, q := range e.GetQuantifiers() {
				owned[q.GetAlias()] = struct{}{}
			}
			for _, p := range wp.GetPredicates() {
				predicates.WalkPredicate(p, func(node predicates.QueryPredicate) bool {
					if alias, ok := predicates.IsExistentialPredicate(node); ok {
						if _, isOwned := owned[alias]; !isOwned && dangling == nil {
							dangling = &DanglingExistentialPredicateError{Alias: alias}
						}
					}
					return true
				})
				if predicateContainsExistsValue(p) {
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
	return dangling
}

// DanglingExistentialPredicateError signals a translated plan tree whose
// directly-handled existential predicate (`EXISTS(q)` / `NOT EXISTS(q)`)
// names a quantifier the predicate-bearing expression does not own. Such a
// marker has no subquery to peel into a semi-join; the NLJ rule finds no
// existential quantifier for it and the predicate is dropped, so the query
// returns every row the EXISTS should have excluded. It is a translator
// defect by construction — translateJoinWithExists once attached only the
// WHERE's existential quantifiers and left an ON-clause EXISTS marker
// dangling — and the guard refuses the plan rather than let the planner
// silently lose the predicate.
type DanglingExistentialPredicateError struct {
	Alias values.CorrelationIdentifier
}

func (e *DanglingExistentialPredicateError) Error() string {
	return "EXISTS predicate references a subquery quantifier its query block does not own: " + e.Alias.Name()
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
	if ev == nil {
		return values.CorrelationIdentifier{}, false
	}
	qov, ok := values.AsQuantifiedObjectValue(ev.GetChild())
	if !ok {
		return values.CorrelationIdentifier{}, false
	}
	return qov.Correlation(), true
}
