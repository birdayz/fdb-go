package embedded

import (
	"maps"

	recordlayer "fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/relational/api"
	antlrgen "fdb.dev/pkg/relational/core/parser/gen"
	"fdb.dev/pkg/relational/core/query/logical"
	"fdb.dev/pkg/relational/core/query/semantic"
)

// The unit tests' builders, under the planner harness's template name
// (defaultEmbeddedTemplate). Production builds with the connection's template
// (cascadesGenerator.sessionTemplate) or, for an index definition, the template
// being created.

// buildLogicalPlanForQueryWithCatalog is buildLogicalPlanForQueryWithTemplate
// under the planner harness's template name, defaultEmbeddedTemplate.
func buildLogicalPlanForQueryWithCatalog(q antlrgen.IQueryContext, md *recordlayer.RecordMetaData) (logical.LogicalOperator, error) {
	return buildLogicalPlanForQueryWithTemplate(q, md, defaultEmbeddedTemplate)
}

// buildDerivedTableSource prepares a standalone query through its owning visitor
// and publishes the retained body's output under alias. Callers that already
// own a prepared body use boundDerivedSource directly.
func buildDerivedTableSource(
	md *recordlayer.RecordMetaData,
	alias string,
	inner antlrgen.IQueryContext,
) (semantic.ScopeSource, bool) {
	return buildDerivedTableSourceWithCTEs(md, alias, inner, nil)
}

// buildDerivedTableSourceWithCTEs is buildDerivedTableSource with the enclosing
// WITH bindings in hand, so a derived body that READS a CTE (`FROM (SELECT *
// FROM c) c` under `WITH c AS (...)`) can be typed at all. Without them the body
// resolves against the CATALOG only, `c` is not a table, the whole source
// declines, and the outer projection over it comes back with no resolved Value.
func buildDerivedTableSourceWithCTEs(
	md *recordlayer.RecordMetaData,
	alias string,
	inner antlrgen.IQueryContext,
	cteScopes map[string]semantic.ScopeSource,
) (semantic.ScopeSource, bool) {
	source, err := buildDerivedTableSourceWithCTEsChecked(md, alias, inner, defaultEmbeddedTemplate, cteScopes)
	return source, err == nil
}

// buildLogicalPlanForQueryBodyWithCatalog dispatches simple SELECT
// vs UNION, threading md through both arms. Mirrors the text
// builder's QueryTermDefault / SetQuery split.
func buildLogicalPlanForQueryBodyWithCatalog(
	body antlrgen.IQueryExpressionBodyContext,
	md *recordlayer.RecordMetaData,
) (logical.LogicalOperator, error) {
	if body == nil {
		return nil, nil
	}
	switch b := body.(type) {
	case *antlrgen.QueryTermDefaultContext:
		// A parenthesized query operand — `(SELECT … LIMIT n)` as a UNION
		// branch — surfaces as a ParenthesisQueryContext. Recurse into the
		// inner query body so the branch's own clauses (notably LIMIT) are
		// built and not silently dropped (RFC-128 §4.7). Without this a
		// parenthesized branch fell through to nil here.
		if paren, ok := b.QueryTerm().(*antlrgen.ParenthesisQueryContext); ok {
			if inner := paren.Query(); inner != nil {
				return buildLogicalPlanForQueryBodyWithCatalog(inner.QueryExpressionBody(), md)
			}
			return nil, nil
		}
		simpleTable, ok := b.QueryTerm().(*antlrgen.SimpleTableContext)
		if !ok {
			return nil, nil
		}
		sq, err := extractFromSimpleTable(simpleTable)
		if err != nil {
			return nil, err
		}
		if fn := findUnsupportedFunctionInSelectQuery(sq); fn != "" {
			return nil, api.NewError(api.ErrCodeUnsupportedQuery,
				"Unsupported operator "+fn)
		}
		return buildLogicalPlanForSelectWithCatalog(sq, md, defaultEmbeddedTemplate)
	case *antlrgen.SetQueryContext:
		return buildLogicalPlanForUnionWithCatalog(b, md)
	}
	return nil, nil
}

// buildLogicalPlanForUnionWithCatalog mirrors buildLogicalPlanForUnion
// — same flattening logic, threads md to each branch.
//
// Trailing ORDER BY: the ANTLR grammar greedily attaches a trailing
// ORDER BY to the rightmost SimpleTable, but SQL standard says it
// applies to the whole UNION result. Mirror the lift in execUnion
// (union.go): strip ORDER BY from the right branch's selectQuery
// before building it, then wrap the final LogicalUnion in a
// LogicalSort using the lifted keys.
func buildLogicalPlanForUnionWithCatalog(
	setQ *antlrgen.SetQueryContext,
	md *recordlayer.RecordMetaData,
) (logical.LogicalOperator, error) {
	if setQ == nil {
		return nil, nil
	}
	if setQ.ALL() == nil {
		return nil, api.NewError(api.ErrCodeUnsupportedQuery, "only UNION ALL is supported")
	}
	left, err := buildLogicalPlanForQueryBodyWithCatalog(setQ.GetLeft(), md)
	if err != nil {
		return nil, err
	}

	// Same ORDER BY / LIMIT stripping as the CTE-catalog variant.
	var lifted unionLiftedClauses
	var right logical.LogicalOperator
	right, lifted, err = buildUnionRightBranchStrippingOrderBy(setQ.GetRight(), md, defaultEmbeddedTemplate, nil, nil)
	if err != nil {
		return nil, err
	}
	if left == nil || right == nil {
		return nil, nil
	}

	if len(lifted.sortKeys) == 0 {
		if s, ok := right.(*logical.LogicalSort); ok {
			lifted.sortKeys = s.Keys
			right = s.Input
		} else if p, ok := right.(*logical.LogicalProject); ok {
			if s, ok := p.Input.(*logical.LogicalSort); ok {
				lifted.sortKeys = s.Keys
				p.Input = s.Input
			}
		}
	}

	inputs := []logical.LogicalOperator{left, right}
	if innerUnion, ok := left.(*logical.LogicalUnion); ok && !innerUnion.Distinct {
		inputs = append(append([]logical.LogicalOperator(nil), innerUnion.Inputs...), right)
	}
	if err := validateUnionColumnCounts(inputs); err != nil {
		return nil, err
	}
	if err := validateUnionColumnTypes(inputs, md); err != nil {
		return nil, err
	}
	if len(lifted.sortKeys) > 0 {
		liftedSort := &logical.LogicalSort{Keys: lifted.sortKeys}
		if err := validateUnionOrderByColumns(liftedSort, inputs[0], lifted.orderBy, md, nil); err != nil {
			return nil, err
		}
	}
	var result logical.LogicalOperator = logical.NewUnion(inputs, false)
	if len(lifted.sortKeys) > 0 {
		result = logical.NewSort(result, lifted.sortKeys)
	}
	if lifted.limit >= 0 || lifted.offset > 0 {
		result = logical.NewLimit(result, lifted.limit, lifted.offset)
	}
	return result, nil
}

// buildUnionRightBranchStrippingOrderBy builds the right branch of a
// UNION, stripping any trailing ORDER BY and LIMIT/OFFSET from the
// simpleTable before building the logical plan. Returns the built
// plan and the stripped clauses (empty if none). For non-simpleTable
// right branches (e.g. nested UNION), falls through to the normal
// builder and returns empty clauses.
func buildUnionRightBranchStrippingOrderBy(
	body antlrgen.IQueryExpressionBodyContext,
	md *recordlayer.RecordMetaData,
	templateName string,
	cteScopes map[string]semantic.ScopeSource,
	cteOnScopes map[string]semantic.ScopeSource,
) (logical.LogicalOperator, unionLiftedClauses, error) {
	visitor := NewPlanVisitorWithTemplate(md, templateName)
	visitor.cteScopes, visitor.cteOnScopes = maps.Clone(cteScopes), maps.Clone(cteOnScopes)
	visitor.cteProducers = *cteRegistryFromScopes(cteScopes, cteOnScopes)
	return visitor.buildUnionRightBranchStrippingOrderBy(body)
}
