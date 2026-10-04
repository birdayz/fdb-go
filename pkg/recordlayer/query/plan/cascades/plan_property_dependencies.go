package cascades

import "fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"

type planPropertyDependency struct {
	ref              *expressions.Reference
	memberVersion    uint64
	winner           expressions.RelationalExpression
	properties       *PlanPropertiesMap
	propertyRevision uint64
}

func planPropertyDependenciesUnchanged(dependencies []planPropertyDependency) bool {
	for _, dependency := range dependencies {
		if dependency.ref.IsForwarded() || dependency.ref.MemberVersion() != dependency.memberVersion ||
			dependency.ref.Winner() != dependency.winner {
			return false
		}
		pm := GetRefPlanPropertiesMap(dependency.ref)
		if pm != dependency.properties || (pm != nil && pm.revision != dependency.propertyRevision) {
			return false
		}
	}
	return true
}

// Java retains properties per immutable expression. Go's live memo edges also
// require invalidation when descendants or their published properties change.
func capturePlanPropertyDependencies(expression expressions.RelationalExpression) []planPropertyDependency {
	var dependencies []planPropertyDependency
	seen := make(map[*expressions.Reference]struct{})
	var visit func(expressions.RelationalExpression)
	visit = func(expression expressions.RelationalExpression) {
		for _, quantifier := range expression.GetQuantifiers() {
			ref := quantifier.GetRangesOver().Canonical()
			if ref == nil {
				continue
			}
			if _, visited := seen[ref]; visited {
				continue
			}
			seen[ref] = struct{}{}
			dependency := planPropertyDependency{
				ref:           ref,
				memberVersion: ref.MemberVersion(),
				winner:        ref.Winner(),
				properties:    GetRefPlanPropertiesMap(ref),
			}
			if dependency.properties != nil {
				dependency.propertyRevision = dependency.properties.revision
			}
			dependencies = append(dependencies, dependency)
			for _, member := range ref.AllMembers() {
				visit(member)
			}
			if dependency.winner != nil {
				visit(dependency.winner)
			}
		}
	}
	visit(expression)
	return dependencies
}
