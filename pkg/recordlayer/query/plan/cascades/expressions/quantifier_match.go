package expressions

import "fdb.dev/pkg/recordlayer/query/plan/cascades/values"

// Like Java Quantifiers.findMatches, reject failed prefixes and compare node
// information under every completed binding, not merely the first child match.
// pairable, when set, restricts which quantifier pairs may bind.
func matchQuantifierBindings(
	member, other RelationalExpression,
	aliases *AliasMap,
	dependencies func(RelationalExpression) [][]int,
	match func(*Reference, *Reference, *AliasMap) bool,
	pairable func(left, right Quantifier) bool,
) bool {
	left, right := member.GetQuantifiers(), other.GetQuantifiers()
	if len(left) != len(right) {
		return false
	}
	if len(left) == 0 {
		return member.EqualsWithoutChildren(other, aliases)
	}
	canCorrelate := member.CanCorrelate()
	asSet := member.ChildrenAsSet() && other.ChildrenAsSet()
	leftDependencies, rightDependencies := dependencies(member), dependencies(other)
	leftDone, rightDone := make([]bool, len(left)), make([]bool, len(right))
	var search func(int, *AliasMap) bool
	search = func(depth int, bound *AliasMap) bool {
		if depth == len(left) {
			return member.EqualsWithoutChildren(other, bound)
		}
		i := depth
		if asSet {
			i = -1
			for candidate := range left {
				if !leftDone[candidate] && (len(leftDependencies) == 0 || quantifierReady(leftDependencies[candidate], leftDone)) {
					i = candidate
					break
				}
			}
			if i < 0 {
				return false
			}
		}
		for j := range right {
			if rightDone[j] || (!asSet && j != i) || (len(rightDependencies) != 0 && !quantifierReady(rightDependencies[j], rightDone)) ||
				!quantifierAttributesEqual(left[i], right[j]) || pairable != nil && !pairable(left[i], right[j]) {
				continue
			}
			source, target := left[i].GetAlias(), right[j].GetAlias()
			if existing, ok := bound.GetTarget(source); ok && existing != target {
				continue
			}
			if existing, ok := bound.GetSource(target); ok && existing != source {
				continue
			}
			childAliases := aliases
			if canCorrelate {
				childAliases = bound
			}
			if !match(left[i].GetRangesOver(), right[j].GetRangesOver(), childAliases) {
				continue
			}
			// Java's FindingMatcher extends the binding only after a child matches.
			next, ok := bound.With(source, target)
			if !ok {
				continue
			}
			leftDone[i], rightDone[j] = true, true
			if search(depth+1, next) {
				return true
			}
			leftDone[i], rightDone[j] = false, false
		}
		return false
	}
	return search(0, aliases)
}

func quantifierDependencies(quantifiers []Quantifier, canCorrelate bool, correlations func(*Reference) map[values.CorrelationIdentifier]struct{}) [][]int {
	if !canCorrelate {
		return nil
	}
	var dependencies [][]int
	owned := make(map[values.CorrelationIdentifier]int, len(quantifiers))
	for i, quantifier := range quantifiers {
		owned[quantifier.GetAlias()] = i
	}
	for i, quantifier := range quantifiers {
		for alias := range correlations(quantifier.GetRangesOver()) {
			if j, local := owned[alias]; local {
				if dependencies == nil {
					dependencies = make([][]int, len(quantifiers))
				}
				dependencies[i] = append(dependencies[i], j)
			}
		}
	}
	return dependencies
}

func quantifierReady(dependencies []int, matched []bool) bool {
	for _, index := range dependencies {
		if !matched[index] {
			return false
		}
	}
	return true
}
