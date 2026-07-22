package cascades

import (
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
			// A HAVING group-key reference is baked to the group-by OUTPUT
			// ordinal. Below the GroupBy the row is the scan/inner layout, where that
			// ordinal is invalid — so the pushed copy REBINDS to the GROUPING KEY'S
			// OWN VALUE (the inner-row-addressed expression, itself construction-
			// baked), Java's pushdown translation. The residual copy that stays
			// ABOVE the GroupBy keeps its output-baked ordinal.
			pushable = append(pushable, predicates.ReplaceValues(p, rebindGroupKeyRefToInner(gb.GetGroupingKeys())))
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

// rebindGroupKeyRefToInner returns the Value replacement that rewrites a
// pushed HAVING reference naming a group key into THAT GROUPING KEY'S OWN
// VALUE — the inner-row-addressed expression (itself construction-baked),
// which is what the reference denotes below the aggregate. This is Java's
// push-down translation; resolving the bare name by string at runtime instead
// would be a lossy shortcut. A reference matching no key is returned unchanged.
func rebindGroupKeyRefToInner(keys []values.Value) func(values.Value) values.Value {
	return func(v values.Value) values.Value {
		if _, ok := v.(*values.FieldValue); !ok {
			return v
		}
		// Match by full accessor path, not leaf name (RFC-187 S7): a predicate
		// field is rebound to a grouping key only when they denote the same
		// column, so a nested `addr.city` is not rewritten to a same-leaf-named
		// top-level `city` grouping key.
		for _, k := range keys {
			if values.ColumnNamePathsEqual(k, v) {
				return k
			}
		}
		return v
	}
}

// buildGroupKeySet keys the grouping columns by their canonical accessor PATH
// (RFC-187 S7), so predicate-pushdown membership distinguishes a nested
// `addr.city` grouping key from a same-leaf-named top-level `city`. Returns nil
// (pushdown disabled) if any grouping key's column identity can't be established.
func buildGroupKeySet(keys []values.Value) map[string]struct{} {
	m := make(map[string]struct{}, len(keys))
	for _, k := range keys {
		key, ok := values.AccessorNamePathKey(k)
		if !ok {
			return nil
		}
		m[key] = struct{}{}
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
	key, ok := values.AccessorNamePathKey(cp.Operand)
	if !ok {
		return false
	}
	if _, inKeys := keySet[key]; !inKeys {
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
		key, ok := values.AccessorNamePathKey(rhs)
		if !ok {
			return false
		}
		_, inKeys := keySet[key]
		return inKeys
	case *values.ConstantValue, *values.NullValue, *values.BooleanValue:
		return true
	default:
		return false
	}
}

var _ ExpressionRule = (*PushFilterThroughGroupByRule)(nil)
