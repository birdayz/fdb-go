package embedded

import (
	"strings"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/relational/core/query/logical"
	"fdb.dev/pkg/relational/core/query/semantic"
)

func parentLexicalNames(parent []semantic.ScopeSource) map[string]struct{} {
	names := make(map[string]struct{}, len(parent))
	for _, source := range parent {
		names[source.Alias.Name()] = struct{}{}
	}
	return names
}

func parentBindingNames(parent []semantic.ScopeSource) map[string]struct{} {
	names := make(map[string]struct{})
	for _, source := range parent {
		name := source.CorrelationName
		if name == "" {
			name = source.Alias.Name()
		}
		names[strings.ToUpper(name)] = struct{}{}
	}
	return names
}

// lowerBoundOn preserves ON placement and the pre-fold provenance. Identities
// decide dependence.
func lowerBoundOn(from logical.LogicalOperator, parent []semantic.ScopeSource) (logical.LogicalOperator, []predicates.QueryPredicate, error) {
	outer := parentBindingNames(parent)
	var lifted []predicates.QueryPredicate
	var walk func(logical.LogicalOperator, bool) (logical.LogicalOperator, error)
	walk = func(op logical.LogicalOperator, laterNullExtension bool) (logical.LogicalOperator, error) {
		join, ok := op.(*logical.LogicalJoin)
		if !ok {
			return op, nil
		}
		copy := *join
		var err error
		copy.Left, err = walk(join.Left, laterNullExtension || join.Kind == logical.JoinRight || join.Kind == logical.JoinFull)
		if err != nil {
			return nil, err
		}
		copy.Right, err = walk(join.Right, laterNullExtension)
		if err != nil {
			return nil, err
		}
		visible := innerSourceAliases(join)
		if origin := join.BoundOn; origin != nil {
			if len(origin.Exists) != 0 {
				return nil, &CorrelatedExistsError{Message: "correlated EXISTS: a nested subquery inside a JOIN ON clause is not supported", Unsupported: true}
			}
			visible = make(map[string]struct{}, len(origin.VisibleBindings))
			for _, id := range origin.VisibleBindings {
				visible[id.Name()] = struct{}{}
			}
		}
		pred, _ := join.OnPredicate.(predicates.QueryPredicate)
		correlation, inner := splitConjunctsByOuterRef(pred, outer, visible)
		if correlation == nil {
			return &copy, nil
		}
		if join.Kind != logical.JoinInner {
			return nil, &CorrelatedExistsError{Message: "correlated EXISTS: a correlation inside an OUTER (LEFT/RIGHT/FULL) JOIN ON clause is not supported", Unsupported: true}
		}
		if laterNullExtension {
			return nil, &CorrelatedExistsError{Message: "correlated EXISTS: a correlated ON before a later RIGHT/FULL join is not supported", Unsupported: true}
		}
		// A later inner source that reuses an outer name does not capture
		// this ON's reference: the ON resolved left-to-current against the
		// bindings visible at it (BoundOn), Java's order.
		copy.OnPredicate = inner
		lifted = append(lifted, correlation)
		return &copy, nil
	}
	op, err := walk(from, false)
	return op, lifted, err
}
