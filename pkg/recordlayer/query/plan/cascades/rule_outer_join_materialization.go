package cascades

import (
	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/matching"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// OuterJoinMaterializationRule re-forms the LEFT OUTER select from Java's
// canonical outer join, Select(preserved, nullOnEmpty(Select(nullSupplying,
// ON))), which RewriteOuterJoinRule leaves behind in REWRITING. It exists for
// the materialized nested-loop join (RFC-152), a Go extension that scans the
// null-supplying leg once where the correlated FlatMap re-scans it per
// preserved row; Java has no materialized outer join, so REWRITING
// canonicalizes as Java does and this PLANNING rule only adds the alternative
// for the cost model.
//
// The canonical select's own predicates are WHERE conjuncts over the joined
// row, which the LEFT OUTER select would read as ON conditions; they stay above
// it, over a box quantifier flowing the two legs' rows, as RewriteOuterJoinRule
// boxes an outer join under existentials.
type OuterJoinMaterializationRule struct {
	matcher matching.BindingMatcher
}

func NewOuterJoinMaterializationRule() *OuterJoinMaterializationRule {
	return &OuterJoinMaterializationRule{
		matcher: NewExpressionMatcher[*expressions.SelectExpression]("canonical_outer_join").WithRootPredicate(
			func(sel *expressions.SelectExpression) bool {
				qs := sel.GetQuantifiers()
				return sel.GetJoinType() == expressions.JoinInner && len(qs) == 2 &&
					qs[0].IsNullOnEmpty() != qs[1].IsNullOnEmpty()
			},
		),
	}
}

var _ ExpressionRule = (*OuterJoinMaterializationRule)(nil)

func (r *OuterJoinMaterializationRule) ConstraintDependencies() []any { return nil }

func (r *OuterJoinMaterializationRule) Matcher() matching.BindingMatcher { return r.matcher }

func (r *OuterJoinMaterializationRule) OnMatch(call *ExpressionRuleCall) {
	sel := matching.Get[*expressions.SelectExpression](call.Bindings, r.matcher)
	quantifiers := sel.GetQuantifiers()
	preserved, nullOnEmpty := quantifiers[0], quantifiers[1]
	if preserved.IsNullOnEmpty() {
		preserved, nullOnEmpty = nullOnEmpty, preserved
	}
	if preserved.Kind() != expressions.QuantifierForEach ||
		nullOnEmpty.Kind() != expressions.QuantifierForEach ||
		hasStrictSingleQuantifier(quantifiers) {
		return
	}
	nullSupplying, inner := nullSupplyingLegOf(nullOnEmpty)
	if inner == nil || outerJoinAlreadyFormed(call.Reference, preserved, nullSupplying) {
		return
	}
	var legAliases []string
	if la, ra := preserved.GetAlias().Name(), nullSupplying.GetAlias().Name(); la != "" && ra != "" {
		legAliases = []string{la, ra}
	}
	if len(sel.GetPredicates()) == 0 {
		// The null-supplying leg keeps the null-on-empty quantifier's alias, so
		// the select's result value reads the same rows from the outer join.
		outerJoin, err := expressions.NewSelectExpressionWithJoinType(
			sel.GetResultValue(),
			[]expressions.Quantifier{preserved, nullSupplying},
			inner.GetPredicates(),
			legAliases,
			expressions.JoinLeftOuter,
		)
		if err != nil {
			call.Fail(err)
			return
		}
		call.Yield(outerJoin)
		return
	}

	// The box's slots flow the canonical select's two quantifiers, so the
	// null-supplying slot keeps the null-on-empty edge's nullable row.
	legs := []expressions.Quantifier{preserved, nullOnEmpty}
	legTypes := legRowTypes(sel.GetResultValue(), sel.GetPredicates())
	boxFields := make([]values.RecordConstructorField, len(legs))
	boxTypeFields := make([]values.Field, len(legs))
	for i, leg := range legs {
		fov, err := leg.RequireFlowedObjectValue()
		if err != nil {
			recordMergeSlotTypeDisagreement(err)
			return
		}
		if _, typed := fov.Type().(*values.RecordType); !typed {
			rt := legTypes[leg.GetAlias()]
			if rt == nil {
				return
			}
			if fov, err = values.NewQuantifiedObjectValue(leg.GetAlias(), rt); err != nil {
				call.Fail(err)
				return
			}
		}
		boxFields[i] = values.RecordConstructorField{Name: values.OrdinalFieldName(i), Value: fov}
		boxTypeFields[i] = values.Field{Name: values.OrdinalFieldName(i), FieldType: fov.Type(), Ordinal: i}
	}
	outerJoin, err := expressions.NewSelectExpressionWithJoinType(
		values.NewRawRecordConstructorValue(boxFields...),
		[]expressions.Quantifier{preserved, nullSupplying},
		inner.GetPredicates(),
		legAliases,
		expressions.JoinLeftOuter,
	)
	if err != nil {
		call.Fail(err)
		return
	}
	boxAlias := values.UniqueCorrelationIdentifier()
	boxQ := expressions.NamedForEachQuantifier(boxAlias, call.MemoizeExpression(outerJoin))
	boxQOV, err := values.NewQuantifiedObjectValue(boxAlias, &values.RecordType{Fields: boxTypeFields})
	if err != nil {
		call.Fail(err)
		return
	}
	tb := values.NewTranslationMapBuilder()
	for i, leg := range legs {
		slot, slotErr := values.ResolveFieldOrdinals(boxQOV, []int{i})
		if slotErr != nil {
			call.Fail(slotErr)
			return
		}
		tb = tb.When(leg.GetAlias()).Then(func(_ values.CorrelationIdentifier, _ values.Value) values.Value {
			return slot
		})
	}
	boxMap := tb.Build()
	result, err := values.TranslateCorrelationsChecked(sel.GetResultValue(), boxMap)
	if err != nil {
		call.Fail(err)
		return
	}
	where := make([]predicates.QueryPredicate, len(sel.GetPredicates()))
	for i, p := range sel.GetPredicates() {
		if where[i], err = predicates.TranslateLeafPredicatesChecked(p, boxMap); err != nil {
			call.Fail(err)
			return
		}
	}
	var boxAliases []string
	if boxAlias.Name() != "" {
		boxAliases = []string{boxAlias.Name()}
	}
	above, err := expressions.NewSelectExpressionWithJoinType(
		result, []expressions.Quantifier{boxQ}, where, boxAliases, expressions.JoinInner)
	if err != nil {
		call.Fail(err)
		return
	}
	call.Yield(above)
}

// nullSupplyingLegOf finds RewriteOuterJoinRule's inner select under a
// null-on-empty quantifier: one plain ForEach leg under the quantifier's own
// alias, flowing that leg's row, filtered by the ON predicates.
func nullSupplyingLegOf(nullOnEmpty expressions.Quantifier) (expressions.Quantifier, *expressions.SelectExpression) {
	ref := nullOnEmpty.GetRangesOver()
	if ref == nil {
		return expressions.Quantifier{}, nil
	}
	for _, member := range ref.Members() {
		inner, ok := member.(*expressions.SelectExpression)
		if !ok || inner.GetJoinType() != expressions.JoinInner || len(inner.GetQuantifiers()) != 1 {
			continue
		}
		leg := inner.GetQuantifiers()[0]
		if leg.Kind() != expressions.QuantifierForEach || leg.IsNullOnEmpty() || leg.IsStrictSingle() ||
			leg.GetAlias() != nullOnEmpty.GetAlias() {
			continue
		}
		flowed, err := leg.RequireFlowedObjectValue()
		if err != nil || !values.ValuesStructurallyEqual(flowed, inner.GetResultValue()) {
			continue
		}
		return leg, inner
	}
	return expressions.Quantifier{}, nil
}

// outerJoinAlreadyFormed reports whether ref already holds this rule's yield
// for these two legs, directly or over a box. The box alias is minted per
// firing, so a second firing would otherwise add an equal member under a new
// name.
func outerJoinAlreadyFormed(ref *expressions.Reference, preserved, nullSupplying expressions.Quantifier) bool {
	sameLeg := func(a, b expressions.Quantifier) bool {
		return a.GetAlias() == b.GetAlias() && a.GetRangesOver().Canonical() == b.GetRangesOver().Canonical()
	}
	isOuterJoin := func(e expressions.RelationalExpression) bool {
		sel, ok := e.(*expressions.SelectExpression)
		if !ok || sel.GetJoinType() != expressions.JoinLeftOuter || len(sel.GetQuantifiers()) != 2 {
			return false
		}
		qs := sel.GetQuantifiers()
		return sameLeg(qs[0], preserved) && sameLeg(qs[1], nullSupplying)
	}
	for _, member := range ref.Members() {
		if isOuterJoin(member) {
			return true
		}
		sel, ok := member.(*expressions.SelectExpression)
		if !ok || len(sel.GetQuantifiers()) != 1 {
			continue
		}
		if box := sel.GetQuantifiers()[0].GetRangesOver(); box != nil {
			for _, inner := range box.Members() {
				if isOuterJoin(inner) {
					return true
				}
			}
		}
	}
	return false
}
