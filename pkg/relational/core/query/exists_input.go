package query

import (
	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/core/query/logical"
)

// LowerExistsInput lowers an already-bound, self-contained child exactly once.
// The resulting edge owns both the producer Reference and its exact result type.
// No parent registrations are published here.
func LowerExistsInput(plan logical.LogicalOperator, md *recordlayer.RecordMetaData, retained ...logical.ExistsSubquery) (*logical.ExistsInput, error) {
	inputs := make(map[logical.LogicalOperator]*logical.ExistsInput, len(retained))
	for _, edge := range retained {
		if edge.Plan == nil || edge.Input == nil || edge.FlowedType == nil || !edge.FlowedType.Equals(edge.Input.ResultType()) {
			return nil, api.NewError(api.ErrCodeInternalError, "retained EXISTS edge has no matching owned producer")
		}
		if prior := inputs[edge.Plan]; prior != nil && prior != edge.Input {
			return nil, api.NewError(api.ErrCodeInternalError, "retained EXISTS source has competing producers")
		}
		inputs[edge.Plan] = edge.Input
	}
	ref, scalars, err := translateWithOwnedInputs(plan, md, inputs, translateSubquery)
	if err != nil {
		return nil, err
	}
	owned := make([]logical.ScalarSubquery, len(scalars))
	for i, scalar := range scalars {
		owned[i] = logical.ScalarSubquery{Alias: scalar.Alias, Plan: scalar.Plan}
	}
	input, err := logical.NewExistsInput(ref, owned)
	if err != nil {
		return nil, api.NewErrorf(api.ErrCodeUnsupportedQuery, "%v", err)
	}
	return input, nil
}

func (t *cascadesTranslator) existsInputRef(esq logical.ExistsSubquery) *expressions.Reference {
	if err := esq.ValidateAdmission(); err != nil {
		t.setTranslateErr(api.NewError(api.ErrCodeUnsupportedOperation, err.Error()))
		return nil
	}
	if esq.Input == nil {
		// Programmatically constructed logical plans enter at the translator,
		// without the SQL query owner's lowering boundary.
		plan := esq.Plan
		if esq.JoinPredicate != nil {
			plan = &logical.LogicalFilter{Input: plan, Predicate: esq.JoinPredicate}
		}
		return t.translateSubqueryRef(plan)
	}
	if esq.FlowedType != nil && !esq.FlowedType.Equals(esq.Input.ResultType()) {
		t.setTranslateErr(api.NewError(api.ErrCodeInternalError, "EXISTS attachment type disagrees with its owned producer"))
		return nil
	}
	input := esq.Input
	if esq.JoinPredicate != nil {
		// A subquery WHERE belongs below FirstOrDefault. Retain its owned FROM
		// producer while attaching the correlation inside the existential input.
		var err error
		input, err = LowerExistsInput(&logical.LogicalFilter{Input: esq.Plan, Predicate: esq.JoinPredicate}, t.md,
			logical.ExistsSubquery{Plan: esq.Plan, Input: input, FlowedType: input.ResultType()})
		if err != nil {
			t.setTranslateErr(err)
			return nil
		}
	}
	for _, scalar := range input.Scalars() {
		t.scalarSubqueries = append(t.scalarSubqueries, ScalarSubqueryPlan{Alias: scalar.Alias, Plan: scalar.Plan})
	}
	return input.Reference()
}

// rebaseExistsInputPredicates preserves the owned producer when its enclosing
// gathered/UNNEST boundary moves external references onto a merged row. Those
// routes only rewrite the top-level bound filter, never the child's FROM graph.
// Rebuild its corresponding relational predicate node over the SAME child
// References, keeping the exact output row unchanged. Do not discard Input and
// retranslate Plan: that would recreate the competing producer this edge removes.
func rebaseExistsInputPredicates(esq logical.ExistsSubquery, rebase func(predicates.QueryPredicate) (predicates.QueryPredicate, bool)) (logical.ExistsSubquery, bool) {
	if esq.Input == nil {
		return esq, true
	}
	ref, ok := rebaseBoundFilterChain(esq.Input.Reference(), rebase)
	if !ok {
		return esq, false
	}
	if ref == esq.Input.Reference() {
		return esq, true
	}
	if !esq.Input.ResultType().Equals(ref.Get().GetResultValue().Type()) {
		return esq, false
	}
	input, err := logical.NewExistsInput(ref, esq.Input.Scalars())
	if err != nil {
		return esq, false
	}
	esq.Input = input
	return esq, true
}

// rebaseBoundFilterChain rewrites the bound predicates of the producer's top
// relational node and of each filter stacked directly beneath it: a WHERE with
// a nested scalar lowers to one filter per conjunct group, and an outer-only
// conjunct can sit in the lower one.
func rebaseBoundFilterChain(
	ref *expressions.Reference,
	rebase func(predicates.QueryPredicate) (predicates.QueryPredicate, bool),
) (*expressions.Reference, bool) {
	node := ref.Get()
	withPredicates, ok := node.(expressions.RelationalExpressionWithPredicates)
	if !ok {
		return ref, false
	}
	old := withPredicates.GetPredicates()
	rebased := make([]predicates.QueryPredicate, len(old))
	changed := false
	for i, pred := range old {
		var ok bool
		rebased[i], ok = rebase(pred)
		if !ok {
			return ref, false
		}
		changed = changed || rebased[i] != pred
	}
	var rebuilt expressions.RelationalExpression
	var err error
	switch typed := node.(type) {
	case *expressions.LogicalFilterExpression:
		inner := typed.GetInner()
		if innerRef := inner.GetRangesOver(); innerRef != nil {
			if _, stacked := innerRef.Get().(*expressions.LogicalFilterExpression); stacked {
				rebasedInner, ok := rebaseBoundFilterChain(innerRef, rebase)
				if !ok {
					return ref, false
				}
				if rebasedInner != innerRef {
					inner = expressions.RebuildQuantifier(inner, rebasedInner)
					changed = true
				}
			}
		}
		if !changed {
			return ref, true
		}
		rebuilt, err = expressions.NewLogicalFilterExpression(rebased, inner)
	case *expressions.SelectExpression:
		if !changed {
			return ref, true
		}
		rebuilt, err = expressions.NewSelectExpressionWithJoinType(typed.GetResultValue(), typed.GetQuantifiers(), rebased, typed.GetSourceAliases(), typed.GetJoinType())
	default:
		return ref, false
	}
	if err != nil || !ref.Get().GetResultValue().Type().Equals(rebuilt.GetResultValue().Type()) {
		return ref, false
	}
	return expressions.InitialOf(rebuilt), true
}
