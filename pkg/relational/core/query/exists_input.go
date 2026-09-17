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
	ref, scalars, err := translateWithOwnedInputs(plan, md, inputs)
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
		return t.translateSubqueryRef(esq.Plan)
	}
	if esq.FlowedType != nil && !esq.FlowedType.Equals(esq.Input.ResultType()) {
		t.setTranslateErr(api.NewError(api.ErrCodeInternalError, "EXISTS attachment type disagrees with its owned producer"))
		return nil
	}
	for _, scalar := range esq.Input.Scalars() {
		t.scalarSubqueries = append(t.scalarSubqueries, ScalarSubqueryPlan{Alias: scalar.Alias, Plan: scalar.Plan})
	}
	return esq.Input.Reference()
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
	node := esq.Input.Reference().Get()
	withPredicates, ok := node.(expressions.RelationalExpressionWithPredicates)
	if !ok {
		return esq, false
	}
	old := withPredicates.GetPredicates()
	rebased := make([]predicates.QueryPredicate, len(old))
	for i, pred := range old {
		var ok bool
		rebased[i], ok = rebase(pred)
		if !ok {
			return esq, false
		}
	}
	var rebuilt expressions.RelationalExpression
	var err error
	switch typed := node.(type) {
	case *expressions.LogicalFilterExpression:
		rebuilt, err = expressions.NewLogicalFilterExpression(rebased, typed.GetInner())
	case *expressions.SelectExpression:
		rebuilt, err = expressions.NewSelectExpressionWithJoinType(typed.GetResultValue(), typed.GetQuantifiers(), rebased, typed.GetSourceAliases(), typed.GetJoinType())
	default:
		return esq, false
	}
	if err != nil || !esq.Input.ResultType().Equals(rebuilt.GetResultValue().Type()) {
		return esq, false
	}
	input, err := logical.NewExistsInput(expressions.InitialOf(rebuilt), esq.Input.Scalars())
	if err != nil {
		return esq, false
	}
	esq.Input = input
	return esq, true
}
