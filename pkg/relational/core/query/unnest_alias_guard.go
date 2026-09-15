package query

import (
	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/core/query/logical"
)

// unnestCorrelationReject guards raw logical input as well as parsed SQL. SQL
// display aliases may repeat; two visible sources may not share a runtime
// correlation identity. Derived definitions are separate FROM scopes.
func unnestCorrelationReject(outer logical.LogicalOperator, u *logical.LogicalUnnest) *api.Error {
	seen := make(map[string]struct{})
	var failure *api.Error
	add := func(binding string) {
		if binding == "" {
			failure = api.NewError(api.ErrCodeUnsupportedQuery, "lateral source has no binding identity")
			return
		}
		if _, duplicate := seen[binding]; duplicate {
			failure = api.NewErrorf(api.ErrCodeDuplicateAlias, "lateral sources share correlation identity %q", binding)
		}
		seen[binding] = struct{}{}
	}
	var walk func(logical.LogicalOperator)
	walk = func(op logical.LogicalOperator) {
		if op == nil || failure != nil {
			return
		}
		switch node := op.(type) {
		case *logical.LogicalScan, *logical.LogicalInlineValues, *logical.LogicalUnnest:
			add(sourceBinding(op))
		case *logical.LogicalCTE:
			if node.PreserveMainSource {
				walk(node.Main)
			} else {
				add(sourceBinding(node))
			}
		default:
			for _, child := range op.Children() {
				walk(child)
			}
		}
	}
	walk(outer)
	if failure == nil {
		add(sourceBinding(u))
	}
	return failure
}
