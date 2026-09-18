package embedded

import (
	"strings"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/relational/core/query/logical"
	"fdb.dev/pkg/relational/core/query/semantic"
)

type boundSourceName struct{ lexical, binding string }

func boundSourceNames(op logical.LogicalOperator) []boundSourceName {
	switch node := op.(type) {
	case *logical.LogicalJoin:
		return append(boundSourceNames(node.Left), boundSourceNames(node.Right)...)
	case *logical.LogicalScan:
		name := node.Alias
		if name == "" {
			name = node.Table
		}
		return []boundSourceName{{name, strings.ToUpper(sourceBindingName(node))}}
	case *logical.LogicalCTE:
		if node.PreserveMainSource {
			return boundSourceNames(node.Main)
		}
		name := node.Alias
		if name == "" {
			name = node.Name()
		}
		return []boundSourceName{{name, strings.ToUpper(sourceBindingName(node))}}
	case *logical.LogicalUnnest:
		return []boundSourceName{{node.Alias, strings.ToUpper(sourceBindingName(node))}}
	case *logical.LogicalInlineValues:
		return []boundSourceName{{node.Alias, strings.ToUpper(sourceBindingName(node))}}
	}
	if children := op.Children(); len(children) == 1 {
		return boundSourceNames(children[0])
	}
	return nil
}

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

func boundScopeAmbiguous(pred predicates.QueryPredicate, from logical.LogicalOperator, parent []semantic.ScopeSource) string {
	sources := boundSourceNames(from)
	if len(sources) < 2 || pred == nil {
		return ""
	}
	refs := predicates.GetCorrelatedToOfPredicate(pred)
	for _, source := range sources {
		if _, read := refs[values.NamedCorrelationIdentifier(source.binding)]; !read {
			continue
		}
		lexicalBinding := strings.ToUpper(source.lexical)
		for _, outer := range parent {
			// Lexical names are already SQL-normalized; quoted case remains
			// significant. The same parent must also supply the runtime-name
			// collision, so private bindings keep their existing exemption.
			if outer.Alias.Name() != source.lexical {
				continue
			}
			binding := outer.CorrelationName
			if binding == "" {
				binding = outer.Alias.Name()
			}
			if strings.ToUpper(binding) == lexicalBinding {
				return source.lexical
			}
		}
	}
	return ""
}

// lowerBoundOn preserves ON placement and the pre-fold provenance. Identities
// decide dependence; lexical collision admission remains its separate existing
// contract, so allocating an ID never silently widens accepted SQL.
func lowerBoundOn(from logical.LogicalOperator, parent []semantic.ScopeSource) (logical.LogicalOperator, []predicates.QueryPredicate, error) {
	outer := parentBindingNames(parent)
	parentNames := make(map[values.CorrelationIdentifier]string, len(parent))
	for _, source := range parent {
		binding := source.CorrelationName
		if binding == "" {
			binding = source.Alias.Name()
		}
		parentNames[values.NamedCorrelationIdentifier(binding)] = source.Alias.Name()
	}
	allSources := boundSourceNames(from)
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
		for id := range predicates.GetCorrelatedToOfPredicate(correlation) {
			lexical, inherited := parentNames[id]
			if !inherited {
				continue
			}
			for _, source := range allSources {
				if source.lexical != lexical {
					continue
				}
				if _, present := visible[source.binding]; !present {
					return nil, &CorrelatedExistsError{Message: "correlated EXISTS: a JOIN ON references an alias reused as a later inner join source (outer/inner alias collision) is not supported", Unsupported: true}
				}
			}
		}
		copy.OnPredicate = inner
		lifted = append(lifted, correlation)
		return &copy, nil
	}
	op, err := walk(from, false)
	return op, lifted, err
}
