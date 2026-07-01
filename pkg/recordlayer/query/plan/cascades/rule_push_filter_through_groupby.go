package cascades

import (
	"strings"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/matching"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// PushFilterThroughGroupByRule pushes predicates from a LogicalFilter
// below a GroupByExpression when those predicates reference only the
// grouping keys. Supports partial pushdown: pushable predicates move
// below GroupBy; residual predicates stay as a filter above.
//
//	Filter([P1, P2], GroupBy(keys, aggs, X))
//	  → Filter([P2], GroupBy(keys, aggs, Filter([P1], X)))  [P1 on keys, P2 not]
//	  → GroupBy(keys, aggs, Filter([P1, P2], X))            [all on keys]
//
// Soundness: if a predicate references only grouping-key columns,
// filtering before aggregation produces the same groups — rows
// eliminated by the predicate wouldn't contribute to any group that
// survives it.
//
// Java equivalent: PushPredicateThroughGroupByRule.
type PushFilterThroughGroupByRule struct {
	matcher matching.BindingMatcher
}

func NewPushFilterThroughGroupByRule() *PushFilterThroughGroupByRule {
	return &PushFilterThroughGroupByRule{
		matcher: NewExpressionMatcher[*expressions.LogicalFilterExpression]("logical_filter"),
	}
}

func (r *PushFilterThroughGroupByRule) Matcher() matching.BindingMatcher { return r.matcher }

func (r *PushFilterThroughGroupByRule) OnMatch(call *ExpressionRuleCall) {
	f := matching.Get[*expressions.LogicalFilterExpression](call.Bindings, r.matcher)
	innerExpr := f.GetInner().GetRangesOver().Get()
	gb, ok := innerExpr.(*expressions.GroupByExpression)
	if !ok {
		return
	}

	groupKeySet := buildGroupKeySet(gb.GetGroupingKeys())
	if len(groupKeySet) == 0 && len(gb.GetGroupingKeys()) > 0 {
		return
	}

	var pushable, residual []predicates.QueryPredicate
	for _, p := range f.GetPredicates() {
		if predicateReferencesOnlyKeys(p, groupKeySet) {
			pushable = append(pushable, p)
		} else {
			residual = append(residual, p)
		}
	}
	if len(pushable) == 0 {
		return
	}

	pushed := expressions.NewLogicalFilterExpression(pushable, gb.GetInner())
	pushedQ := expressions.ForEachQuantifier(call.MemoizeExpression(pushed))
	newGB := expressions.NewGroupByExpression(gb.GetGroupingKeys(), gb.GetAggregates(), pushedQ)

	if len(residual) == 0 {
		call.Yield(newGB)
	} else {
		gbQ := expressions.ForEachQuantifier(call.MemoizeExpression(newGB))
		call.Yield(expressions.NewLogicalFilterExpression(residual, gbQ))
	}
}

func buildGroupKeySet(keys []values.Value) map[string]struct{} {
	m := make(map[string]struct{}, len(keys))
	for _, k := range keys {
		fv, ok := k.(*values.FieldValue)
		if !ok {
			return nil
		}
		m[strings.ToUpper(fv.Field)] = struct{}{}
	}
	return m
}

func predicateReferencesOnlyKeys(p predicates.QueryPredicate, keySet map[string]struct{}) bool {
	cp, ok := p.(*predicates.ComparisonPredicate)
	if !ok {
		// A ConstantPredicate (e.g. HAVING FALSE / HAVING NULL after folding)
		// references NO grouping column, so it is NOT a grouping-key predicate
		// and must stay ABOVE the GroupBy. Pushing a row-eliminating constant
		// below a SCALAR (zero-grouping-key) aggregate is WRONG: the scalar
		// aggregate emits one row even over empty input, so `SELECT COUNT(*)
		// FROM t HAVING FALSE` would return 1 row {0} instead of 0 rows. Only a
		// ComparisonPredicate on a grouping column is pushable (RFC-166).
		return false
	}
	fv, ok := cp.Operand.(*values.FieldValue)
	if !ok {
		return false
	}
	if _, inKeys := keySet[strings.ToUpper(fv.Field)]; !inKeys {
		return false
	}
	// The RHS comparand must ALSO reference only grouping keys / constants.
	// A HAVING predicate like `g > SUM(v)` has a grouping-key LHS but an
	// AGGREGATE-valued RHS: pushing it below the GroupBy evaluates it on raw
	// scan rows where SUM(v) does not yet exist, mis-filtering the aggregation
	// input. Java's PredicatePushDownRule.visitGroupByExpression pushes NOTHING
	// through a GroupBy for exactly this reason; we keep the (sound) key-only
	// pushdown but must reject any predicate whose comparand is not key-only.
	return comparandReferencesOnlyKeys(cp.Comparison, keySet)
}

// comparandReferencesOnlyKeys reports whether a comparison's RHS comparand is
// safe to evaluate below a GroupBy: a unary comparison (IS [NOT] NULL, no
// comparand), a literal/constant comparand, or a grouping-key field. Anything
// else (an aggregate value, a non-key column, an arithmetic/correlated value)
// is conservatively treated as non-pushable.
func comparandReferencesOnlyKeys(c predicates.Comparison, keySet map[string]struct{}) bool {
	if c.Type.IsUnary() || c.Operand == nil {
		return true
	}
	switch rhs := c.Operand.(type) {
	case *values.FieldValue:
		_, inKeys := keySet[strings.ToUpper(rhs.Field)]
		return inKeys
	case *values.ConstantValue, *values.NullValue, *values.BooleanValue:
		return true
	default:
		return false
	}
}

var _ ExpressionRule = (*PushFilterThroughGroupByRule)(nil)
