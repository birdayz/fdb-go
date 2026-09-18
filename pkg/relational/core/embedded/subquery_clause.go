package embedded

import (
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/relational/api"
	antlrgen "fdb.dev/pkg/relational/core/parser/gen"
	"fdb.dev/pkg/relational/core/query/expr"
	"fdb.dev/pkg/relational/core/query/logical"
)

// subqueryClause is a private construction result. Expression callbacks can
// allocate typed edges here, but cannot publish into the owner's admitted lists.
// The entire resolved clause, not a child query's syntax, decides consumption.
type subqueryClause struct {
	*existsSubqueryPlanner
	subqueries                 []logical.ExistsSubquery
	scalarSubqueries           []logical.ScalarSubquery
	correlatedScalarSubqueries []logical.CorrelatedScalarSubquery
	published                  bool
}

func (p *existsSubqueryPlanner) newClause() *subqueryClause {
	return &subqueryClause{existsSubqueryPlanner: p}
}

// walkSubqueryPredicate completes normalization and admission before publication.
// A later resolution error cannot leak earlier callbacks' provisional edges.
func walkSubqueryPredicate(resolver *expr.Resolver, owner *existsSubqueryPlanner, syntax antlrgen.IExpressionContext) (predicates.QueryPredicate, error) {
	if owner == nil {
		return resolver.WalkPredicate(syntax)
	}
	clause := owner.newClause()
	resolver.SetSubqueryPlanner(clause)
	defer resolver.SetSubqueryPlanner(nil)
	pred, err := resolver.WalkPredicate(syntax)
	if err != nil {
		return nil, err
	}
	pred = predicates.SimplifyPredicateValues(pred)
	if err := clause.admitPredicate(pred); err != nil {
		return nil, err
	}
	return pred, nil
}

func (c *subqueryClause) validateUse(alias values.CorrelationIdentifier, use logical.ExistsConsumer) error {
	for i, edge := range c.subqueries {
		if edge.Alias != alias {
			continue
		}
		if edge.Input == nil || edge.FlowedType == nil || !edge.FlowedType.Equals(edge.Input.ResultType()) {
			return api.NewError(api.ErrCodeInternalError, "EXISTS clause edge has no matching owned producer")
		}
		// Known truth can be folded in predicates, but projection substitution
		// is unsupported. Reject before this clause publishes any attachment.
		if edge.KnownTruth != nil && use == logical.ExistsProjectedValue {
			return api.NewError(api.ErrCodeUnsupportedQuery, "a projected cardinality-known EXISTS is not yet supported")
		}
		admitted, err := edge.AdmitConsumer(use)
		if err != nil {
			return api.NewError(api.ErrCodeUnsupportedOperation, err.Error())
		}
		c.subqueries[i] = admitted
		return nil
	}
	return api.NewError(api.ErrCodeInternalError, "EXISTS consumer has no clause-owned attachment")
}

func (c *subqueryClause) admitPredicate(pred predicates.QueryPredicate) error {
	if existsUnderDisjunction(pred) {
		return api.NewError(api.ErrCodeUnsupportedOperation, "EXISTS within an OR (disjunction) is not supported")
	}
	var check func(predicates.QueryPredicate) error
	check = func(p predicates.QueryPredicate) error {
		if p == nil {
			return nil
		}
		if alias, ok := predicates.IsExistentialPredicate(p); ok {
			return c.validateUse(alias, logical.ExistsPositivePredicate)
		}
		if alias, ok := predicates.IsNotExistentialPredicate(p); ok {
			return c.validateUse(alias, logical.ExistsNegativePredicate)
		}
		if and, ok := p.(*predicates.AndPredicate); ok {
			for _, child := range and.SubPredicates {
				if err := check(child); err != nil {
					return err
				}
			}
			return nil
		}
		refs := predicates.GetCorrelatedToOfPredicate(p)
		for _, edge := range c.subqueries {
			if _, used := refs[edge.Alias]; used {
				return api.NewError(api.ErrCodeUnsupportedOperation, "EXISTS in this query shape is not yet supported: unclassified predicate consumption")
			}
		}
		return nil
	}
	if err := check(pred); err != nil {
		return err
	}
	return c.publish()
}

func (c *subqueryClause) admitValues(outputs []values.Value) error {
	for _, output := range outputs {
		var useErr error
		values.WalkValue(output, func(node values.Value) bool {
			if useErr != nil {
				return false
			}
			if exists, ok := node.(*values.ExistsValue); ok {
				qov, ok := values.AsQuantifiedObjectValue(exists.Value)
				if !ok {
					useErr = api.NewError(api.ErrCodeUnsupportedOperation, "unclassified EXISTS value consumption")
				} else {
					useErr = c.validateUse(qov.Correlation(), logical.ExistsProjectedValue)
				}
				return false
			}
			if qov, ok := values.AsQuantifiedObjectValue(node); ok {
				for _, edge := range c.subqueries {
					if qov.Correlation() == edge.Alias {
						useErr = api.NewError(api.ErrCodeUnsupportedOperation, "EXISTS attachment used outside its boolean consumer")
						return false
					}
				}
			}
			return true
		})
		if useErr != nil {
			return useErr
		}
	}
	return c.publish()
}

func (c *subqueryClause) publish() error {
	if c.published {
		return api.NewError(api.ErrCodeInternalError, "subquery clause already published")
	}
	aliases := make(map[values.CorrelationIdentifier]struct{})
	checkEdge := func(alias values.CorrelationIdentifier, plan logical.LogicalOperator) error {
		if alias.IsZero() || plan == nil {
			return api.NewError(api.ErrCodeInternalError, "subquery clause edge has no identity or child")
		}
		if _, duplicate := aliases[alias]; duplicate {
			return api.NewError(api.ErrCodeInternalError, "subquery clause has competing attachment owners")
		}
		aliases[alias] = struct{}{}
		return nil
	}
	for _, edge := range c.subqueries {
		if err := checkEdge(edge.Alias, edge.Plan); err != nil {
			return err
		}
		if edge.Input == nil || edge.FlowedType == nil || !edge.FlowedType.Equals(edge.Input.ResultType()) {
			return api.NewError(api.ErrCodeInternalError, "EXISTS clause edge has no matching owned producer")
		}
		if err := edge.ValidateAdmission(); err != nil {
			return api.NewError(api.ErrCodeUnsupportedOperation, err.Error())
		}
	}
	for _, edge := range c.scalarSubqueries {
		if err := checkEdge(edge.Alias, edge.Plan); err != nil {
			return err
		}
	}
	for _, edge := range c.correlatedScalarSubqueries {
		if err := checkEdge(edge.Alias, edge.InnerPlan); err != nil {
			return err
		}
	}
	owner := c.existsSubqueryPlanner
	owner.subqueries = append(owner.subqueries, c.subqueries...)
	owner.scalarSubqueries = append(owner.scalarSubqueries, c.scalarSubqueries...)
	owner.correlatedScalarSubqueries = append(owner.correlatedScalarSubqueries, c.correlatedScalarSubqueries...)
	c.published = true
	return nil
}
