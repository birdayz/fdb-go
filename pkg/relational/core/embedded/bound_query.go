package embedded

import (
	"maps"
	"strings"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/relational/api"
	antlrgen "fdb.dev/pkg/relational/core/parser/gen"
	"fdb.dev/pkg/relational/core/query/logical"
	"fdb.dev/pkg/relational/core/query/semantic"
)

type bindingSet map[values.CorrelationIdentifier]struct{}

// boundQuery is the return value of query construction, not visitor scratch.
// Plan owns resolved clauses and the retained FROM/CTE/attachment graph. Free is
// derived before existential normalization, including output-only references.
// Parent retains the actual lexical frame, independently of restored WITH maps.
type boundQuery struct {
	plan   logical.LogicalOperator
	parent []semantic.ScopeSource
	free   bindingSet
}

func (p *existsSubqueryPlanner) bindQuery(q antlrgen.IQueryContext) (*boundQuery, error) {
	// OVER is erased by aggregate construction. Bound classification must not
	// depend on a caller having already run the public SQL validation pre-pass.
	if err := rejectWindowedAggregate(q); err != nil {
		return nil, err
	}
	visitor, err := p.newSubqueryVisitor()
	if err != nil {
		return nil, err
	}
	plan, primaryUnnest, err := p.tryBuildCorrelatedPrimaryUnnest(q)
	if err != nil {
		return nil, err
	}
	if !primaryUnnest {
		plan, err = visitor.VisitQuery(q)
		if err != nil {
			return nil, err
		}
	}
	if plan == nil {
		return nil, api.NewError(api.ErrCodeUnsupportedQuery, "bound query has no logical plan")
	}
	if err := demoteSchemaQualifiedUnnest(plan, p.effectiveSchemaName(), p.md); err != nil {
		return nil, err
	}
	if err := resolveQualifiedTableNames(plan, p.effectiveSchemaName()); err != nil {
		return nil, err
	}
	plan = p.wrapWithOuterCTEs(plan)
	return newBoundQuery(plan, visitor.enclosingScope)
}

// newBoundQuery seals a constructed graph before either scalar or existential
// consumers classify it. Both use the same logical dependency property; memo
// translation is not a prerequisite for deciding whether a query is correlated.
func newBoundQuery(plan logical.LogicalOperator, enclosing *semantic.Scope) (*boundQuery, error) {
	property, err := boundDependencies(plan, nil)
	if err != nil {
		return nil, err
	}
	var parent []semantic.ScopeSource
	for frame := enclosing; frame != nil; frame = frame.Parent() {
		for _, source := range frame.Sources() {
			source.HiddenColumns = maps.Clone(source.HiddenColumns)
			source.AdditionalQualifiers = append([]semantic.Identifier(nil), source.AdditionalQualifiers...)
			parent = append(parent, source)
		}
	}
	if err := validateBoundDependencies(property.free, parent); err != nil {
		return nil, err
	}
	return &boundQuery{plan: plan, parent: parent, free: property.free}, nil
}

// validateBoundDependencies distinguishes an inherited binding from a missing
// attachment owner. An orphan generated identifier cannot classify as an
// independent subquery just because it is absent from the lexical parent.
func validateBoundDependencies(free bindingSet, parent []semantic.ScopeSource) error {
	inherited := make(bindingSet, len(parent))
	for _, source := range parent {
		name := source.CorrelationName
		if name == "" {
			name = source.Alias.Name()
		}
		inherited[values.NamedCorrelationIdentifier(name)] = struct{}{}
	}
	for id := range free {
		if _, found := inherited[id]; !found {
			return api.NewErrorf(api.ErrCodeInternalError, "bound query reference %s has no source or attachment owner", id)
		}
	}
	return nil
}

type boundProperty struct {
	free  bindingSet
	local bindingSet
}

func sourceBindingName(op logical.LogicalOperator) string {
	switch node := op.(type) {
	case *logical.LogicalScan:
		if node.Binding != "" {
			return node.Binding
		}
		if node.Alias != "" {
			return node.Alias
		}
		return node.Table
	case *logical.LogicalInlineValues:
		if node.Binding != "" {
			return node.Binding
		}
		return node.Alias
	case *logical.LogicalUnnest:
		return logical.UnnestBindingName(node.Binding, node.Alias, node.AtAlias)
	case *logical.LogicalCTE:
		if node.PreserveMainSource {
			return sourceBindingName(node.Main)
		}
		if node.Binding != "" {
			return node.Binding
		}
		return node.Name
	}
	return ""
}

func unionBindings(dst bindingSet, src bindingSet) {
	for id := range src {
		dst[id] = struct{}{}
	}
}

// boundDependencies is the logical analogue of Java's quantifier property:
// local value references lose only their satisfying binders; child dependencies
// cross non-correlating boundaries unchanged. CTE definitions are evaluated in
// their defining registry and contribute only at an actual scan of that body.
// No translation, parser walk, undefined-column retry, or SQL-name veto occurs.
func boundDependencies(op logical.LogicalOperator, ctes map[string]bindingSet) (boundProperty, error) {
	r := boundProperty{free: make(bindingSet), local: make(bindingSet)}
	own := make(bindingSet)
	addValue := func(v values.Value) { unionBindings(own, values.GetCorrelatedToOfValue(v)) }
	addPred := func(p predicates.QueryPredicate) {
		if p != nil {
			unionBindings(own, predicates.GetCorrelatedToOfPredicate(p))
		}
	}
	var scalar []logical.ScalarSubquery
	var correlated []logical.CorrelatedScalarSubquery
	var existential []logical.ExistsSubquery
	var children []logical.LogicalOperator
	switch node := op.(type) {
	case *logical.LogicalScan:
		unionBindings(r.free, ctes[strings.ToUpper(node.Table)])
		r.local[values.NamedCorrelationIdentifier(strings.ToUpper(sourceBindingName(node)))] = struct{}{}
	case *logical.LogicalInlineValues:
		addValue(node.CollectionValue())
		r.local[values.NamedCorrelationIdentifier(strings.ToUpper(sourceBindingName(node)))] = struct{}{}
	case *logical.LogicalUnnest:
		addValue(node.CorrelatedCollection)
		r.local[values.NamedCorrelationIdentifier(strings.ToUpper(sourceBindingName(node)))] = struct{}{}
	case *logical.LogicalCTE:
		definitions := maps.Clone(ctes)
		if definitions == nil {
			definitions = make(map[string]bindingSet)
		}
		if node.Recursive {
			definitions[strings.ToUpper(node.Name)] = make(bindingSet)
		}
		body, err := boundDependencies(node.Body, definitions)
		if err != nil {
			return r, err
		}
		definitions[strings.ToUpper(node.Name)] = body.free
		return boundDependencies(node.Main, definitions)
	case *logical.LogicalUnion:
		for _, branch := range node.Inputs {
			property, err := boundDependencies(branch, ctes)
			if err != nil {
				return r, err
			}
			unionBindings(r.free, property.free)
		}
		return r, nil
	case *logical.LogicalFilter:
		children = []logical.LogicalOperator{node.Input}
		addPred(node.Predicate)
		scalar, correlated, existential = node.ScalarSubqueries, node.CorrelatedScalarSubqueries, node.ExistsSubqueries
	case *logical.LogicalProject:
		children = []logical.LogicalOperator{node.Input}
		for _, value := range node.ProjectedValues {
			addValue(value)
		}
		scalar, correlated = node.ScalarSubqueries, node.CorrelatedScalarSubqueries
	case *logical.LogicalSort:
		children = []logical.LogicalOperator{node.Input}
		for _, key := range node.Keys {
			addValue(key.Value)
		}
	case *logical.LogicalLimit:
		children = []logical.LogicalOperator{node.Input}
		addValue(node.LimitValue)
	case *logical.LogicalDistinct:
		children = []logical.LogicalOperator{node.Input}
	case *logical.LogicalAggregate:
		children = []logical.LogicalOperator{node.Input}
		for _, key := range node.GroupKeys {
			addValue(key.Value)
		}
		for _, operand := range node.AggregateOperands {
			addValue(operand)
		}
		addPred(node.HavingPredicate)
		scalar, existential = node.HavingScalarSubqueries, node.HavingExistsSubqueries
	case *logical.LogicalJoin:
		children = []logical.LogicalOperator{node.Left, node.Right}
		if pred, ok := node.OnPredicate.(predicates.QueryPredicate); ok {
			addPred(pred)
		}
		existential = node.OnExistsSubqueries
	case *logical.LogicalValues:
	default:
		return r, api.NewErrorf(api.ErrCodeUnsupportedQuery, "no bound dependency property for %T", op)
	}
	for _, child := range children {
		property, err := boundDependencies(child, ctes)
		if err != nil {
			return r, err
		}
		unionBindings(r.free, property.free)
		unionBindings(r.local, property.local)
	}
	for _, edge := range scalar {
		property, err := boundDependencies(edge.Plan, ctes)
		if err != nil {
			return r, err
		}
		unionBindings(own, property.free)
		r.local[edge.Alias] = struct{}{}
		// Independent scalars are materialized before their owner's input is
		// evaluated. Their bindings satisfy surviving predicates inside that
		// input too; this is not permission to subtract source/CTE binders at
		// an ordinary unary boundary.
		delete(r.free, edge.Alias)
	}
	for _, edge := range correlated {
		property, err := boundDependencies(edge.InnerPlan, ctes)
		if err != nil {
			return r, err
		}
		unionBindings(own, property.free)
		r.local[edge.Alias] = struct{}{}
	}
	for _, edge := range existential {
		property, err := boundDependencies(edge.Plan, ctes)
		if err != nil {
			return r, err
		}
		refs := maps.Clone(property.free)
		if edge.JoinPredicate != nil {
			for id := range predicates.GetCorrelatedToOfPredicate(edge.JoinPredicate) {
				if _, local := property.local[id]; !local {
					refs[id] = struct{}{}
				}
			}
		}
		unionBindings(own, refs)
		r.local[edge.Alias] = struct{}{}
	}
	// FROM joins satisfy lateral dependencies; ordinary unary wrappers cannot
	// satisfy a dependency originating inside their input's definition.
	if _, join := op.(*logical.LogicalJoin); join {
		for id := range r.local {
			delete(r.free, id)
		}
	}
	for id := range own {
		if _, bound := r.local[id]; !bound {
			r.free[id] = struct{}{}
		}
	}
	return r, nil
}

func (b *boundQuery) correlated() bool {
	for _, source := range b.parent {
		name := source.CorrelationName
		if name == "" {
			name = source.Alias.Name()
		}
		if _, found := b.free[values.NamedCorrelationIdentifier(name)]; found {
			return true
		}
	}
	return false
}

type loweredExists struct {
	plan       logical.LogicalOperator
	join       predicates.QueryPredicate
	truth      predicates.TriBool
	constraint logical.ExistsConstraint
	scalars    []logical.ScalarSubquery
	retained   []logical.ExistsSubquery
	free       bindingSet
}

// lowerBoundExists consumes only retained operators and resolved Values. It
// never reparses the child or publishes to the parent planner's registrations.
func lowerBoundExists(bound *boundQuery) (loweredExists, error) {
	out := loweredExists{plan: bound.plan, free: maps.Clone(bound.free)}
	// Preserve the existing multi-source/UNNEST admission boundary even when
	// private IDs make the child independent. Dependency and admission are
	// different properties; changing the former must not widen the latter.
	for _, source := range bound.parent {
		if !source.Shadowing {
			continue
		}
		inner := boundSourceNames(bound.plan)
		if len(inner) > 1 {
			outer := parentLexicalNames(bound.parent)
			for _, leg := range inner {
				if _, collision := outer[leg.lexical]; collision {
					return out, &CorrelatedExistsError{Message: "EXISTS with a multi-source inner reusing an outer UNNEST-frame source name is not supported", Unsupported: true}
				}
			}
		}
		break
	}
	if !bound.correlated() {
		return out, nil
	}
	var strip func(logical.LogicalOperator) (logical.LogicalOperator, error)
	strip = func(op logical.LogicalOperator) (logical.LogicalOperator, error) {
		switch node := op.(type) {
		case *logical.LogicalCTE:
			copy := *node
			var err error
			copy.Main, err = strip(node.Main)
			return &copy, err
		case *logical.LogicalUnion:
			// Correlated set-operation bodies require branch-local attachment
			// predicates. Preserve their admission restriction before lowering;
			// a derived UNION source remains a separate, supported FROM producer.
			return nil, &CorrelatedExistsError{Message: "correlated EXISTS: unsupported query body shape", Unsupported: true}
		case *logical.LogicalProject:
			return strip(node.Input)
		case *logical.LogicalSort:
			return strip(node.Input)
		case *logical.LogicalDistinct:
			return strip(node.Input)
		case *logical.LogicalLimit:
			if node.LimitValue != nil {
				return nil, api.NewError(api.ErrCodeUnsupportedQuery, "a correlated EXISTS with a planning-time unresolved LIMIT/OFFSET is not supported")
			}
			input, err := strip(node.Input)
			if err != nil {
				return nil, err
			}
			if node.Limit == 0 || node.Offset > 0 && out.truth != nil {
				out.truth = predicates.TriFalse
			} else if node.Offset > 0 {
				return nil, api.NewError(api.ErrCodeUnsupportedQuery, "a correlated EXISTS with data-dependent OFFSET is not supported")
			}
			return input, nil
		case *logical.LogicalAggregate:
			if node.HasHaving {
				return nil, api.NewError(api.ErrCodeUnsupportedQuery, "correlated EXISTS over a GROUP BY / HAVING subquery is not supported")
			}
			if len(node.GroupKeys) == 0 {
				out.truth = predicates.TriTrue
			}
			return strip(node.Input)
		}
		return op, nil
	}
	var err error
	out.plan, err = strip(bound.plan)
	if err != nil {
		return out, err
	}
	var lower func(logical.LogicalOperator) (logical.LogicalOperator, error)
	lower = func(op logical.LogicalOperator) (logical.LogicalOperator, error) {
		if cte, ok := op.(*logical.LogicalCTE); ok {
			copy := *cte
			var err error
			copy.Main, err = lower(cte.Main)
			return &copy, err
		}
		filter, ok := op.(*logical.LogicalFilter)
		if !ok {
			filter = &logical.LogicalFilter{Input: op}
		}
		if filter.HasQualify {
			return nil, api.NewError(api.ErrCodeUnsupportedQuery, "correlated EXISTS over a GROUP BY / HAVING subquery is not supported")
		}
		from, on, err := lowerBoundOn(filter.Input, bound.parent)
		if err != nil {
			return nil, err
		}
		filterCopy := *filter
		filterCopy.Input = from
		filter = &filterCopy
		if len(filter.CorrelatedScalarSubqueries) != 0 {
			return nil, &CorrelatedExistsError{Message: "correlated EXISTS: a correlated scalar subquery inside an EXISTS WHERE clause is not supported"}
		}
		out.scalars = append(out.scalars, filter.ScalarSubqueries...)
		pred := predicates.SimplifyPredicateValues(andOfConjuncts(append(on, conjunctsOf(filter.Predicate)...)))
		if name := boundScopeAmbiguous(pred, filter.Input, bound.parent); name != "" {
			return nil, &CorrelatedExistsError{Message: "correlated EXISTS: inner FROM source " + name + " reuses an outer FROM name referenced by the subquery predicate (scope-ambiguous)", Unsupported: true}
		}
		if pred == nil {
			return filter.Input, nil
		}
		if _, collection := filter.Input.(*logical.LogicalUnnest); collection {
			// A primary collection is already evaluated in its owner's frame.
			// Keep its element predicate on Explode so the existing fan-out
			// access rewrite sees the collection and predicate together.
			copy := *filter
			copy.Predicate = pred
			copy.ScalarSubqueries = nil
			return &copy, nil
		}
		inner := innerSourceAliases(filter.Input)
		if len(filter.ExistsSubqueries) != 0 {
			nonExists := splitNonExistsPredicatesFromWalked(pred)
			// Existential associativity: ∃m∈M: ∃n∈N: P(m,n,o) is
			// ∃(m,n)∈M×N: P(m,n,o). Keep M even when only N reads o:
			// replacing the middle query by N loses M's empty-input constraint.
			// The inner producer is a retained edge, never retranslated here.
			if alias, positive := predicates.IsExistentialPredicate(pred); positive && len(filter.ExistsSubqueries) == 1 {
				edge := filter.ExistsSubqueries[0]
				if alias == edge.Alias && edge.KnownTruth == nil {
					out.join = edge.JoinPredicate
					out.constraint = edge.Constraint
					out.retained = append(out.retained, edge)
					return &logical.LogicalJoin{Left: filter.Input, Right: edge.Plan, Kind: logical.JoinInner}, nil
				}
			}
			out.join = nonExists
			if hasNonInnerConjunct(nonExists, inner) {
				out.constraint = logical.ExistsPositivePredicateOnly
			}
			copy := *filter
			copy.Predicate = stripNonExistsPredicates(pred)
			copy.ScalarSubqueries = nil
			return &copy, nil
		}
		outerOnly, rest := splitOuterOnlyConjuncts(pred, inner)
		out.join = rest
		if outerOnly != nil {
			return &logical.LogicalFilter{Input: filter.Input, Predicate: outerOnly}, nil
		}
		return filter.Input, nil
	}
	out.plan, err = lower(out.plan)
	if err != nil {
		return out, err
	}
	// Projection elision and predicate extraction change the evaluation graph.
	// Re-derive over every retained edge, including scalars moved to the owner
	// and the attachment predicate; neither Plan alone nor the old free set is
	// the residual property.
	residual, err := boundDependencies(&logical.LogicalFilter{
		Input: out.plan, Predicate: out.join, ScalarSubqueries: out.scalars,
	}, nil)
	if err != nil {
		return out, err
	}
	if err := validateBoundDependencies(residual.free, bound.parent); err != nil {
		return out, err
	}
	out.free = residual.free
	if out.truth != nil {
		// A cardinality proof substitutes the result without evaluating the
		// existential input; it does not waive consumer admission.
		out.free = make(bindingSet)
	}
	return out, nil
}
