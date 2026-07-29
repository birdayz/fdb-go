package cascades

import (
	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

const (
	// Strict shared visit budget. An independent eight-leg permutation trie has
	// more than 8! prefix transitions, so exhaustion may truncate even that
	// exact tier; truncation is always a deterministic optimization miss.
	matchIntermediateMaxVisitedStates = 40_320
	// One rule attempt may expose at most this many distinct semantic results.
	matchIntermediateMaxUniqueResults = 64
)

// quantifierMapping pairs one selected query quantifier with one selected
// candidate quantifier. Pair semantics are intentionally left to the consumer:
// structural matching requires equal edge kinds, while Select subsumption in
// RFC-190.4c will permit narrowly defined cross-kind pairs.
type quantifierMapping struct {
	queryIndex     int
	candidateIndex int
}

// matchIntermediateSearchBudget is a strict shared work counter. Subsets,
// topological pair extensions, child-PM choices, and semantic-result
// candidates all consume visits. Exhaustion is a safe optimization miss: no
// partial state is emitted and no check is relaxed.
type matchIntermediateSearchBudget struct {
	visitedStates int
	uniqueResults int
	exhausted     bool
	results       []*PartialMatchImpl
}

func (b *matchIntermediateSearchBudget) chargeState() bool {
	if b.visitedStates >= matchIntermediateMaxVisitedStates {
		b.exhausted = true
		return false
	}
	b.visitedStates++
	return true
}

func (b *matchIntermediateSearchBudget) shouldStop() bool {
	return b.exhausted || b.uniqueResults >= matchIntermediateMaxUniqueResults
}

// recordResult counts semantic results per invocation, independent of whether
// the Reference already stores them. Refires therefore revisit the same first
// 64 deterministic alternatives instead of paging through later groups.
func (b *matchIntermediateSearchBudget) recordResult(pm *PartialMatchImpl) bool {
	for _, existing := range b.results {
		if partialMatchesSemanticallyEqual(existing, pm) {
			return false
		}
	}
	b.results = append(b.results, pm)
	b.uniqueResults++
	return true
}

type quantifierDependencyOrder struct {
	direct     []map[int]struct{}
	transitive []map[int]struct{}
	stableTopo []int
	ok         bool
}

// buildQuantifierDependencyOrder builds an ordinal-indexed dependency graph.
// It includes the merge-seed dependencies used by Select partitioning, closes
// the graph transitively, filters the owned alias a quantifier may re-expose,
// and rejects duplicate aliases and cycles rather than producing an incomplete
// order.
func buildQuantifierDependencyOrder(
	quantifiers []expressions.Quantifier,
) quantifierDependencyOrder {
	aliasToIndex := make(map[values.CorrelationIdentifier]int, len(quantifiers))
	for i, quantifier := range quantifiers {
		alias := quantifier.GetAlias()
		if _, duplicate := aliasToIndex[alias]; duplicate {
			return quantifierDependencyOrder{}
		}
		aliasToIndex[alias] = i
	}

	direct := make([]map[int]struct{}, len(quantifiers))
	for i, quantifier := range quantifiers {
		direct[i] = make(map[int]struct{})
		addDependency := func(alias values.CorrelationIdentifier) bool {
			dependencyIndex, owned := aliasToIndex[alias]
			if !owned {
				return true
			}
			if dependencyIndex == i {
				// Human-readable alias reuse can re-expose the parent alias
				// even though it is not a semantic self-dependency. Mirror
				// Select's correlation-order builder and filter it.
				return true
			}
			direct[i][dependencyIndex] = struct{}{}
			return true
		}
		for alias := range quantifier.GetCorrelatedTo() {
			if !addDependency(alias) {
				return quantifierDependencyOrder{}
			}
		}
	}

	all := make([]int, len(quantifiers))
	for i := range all {
		all[i] = i
	}
	stableTopo, ok := stableTopologicalOrder(all, direct)
	if !ok {
		return quantifierDependencyOrder{}
	}

	transitive := make([]map[int]struct{}, len(quantifiers))
	for i := range quantifiers {
		transitive[i] = make(map[int]struct{})
	}
	// Dependencies precede their users in stableTopo, so every dependency's
	// closure is complete when its user is processed. This is linear in the
	// graph plus the materialized closure and avoids repeatedly walking
	// diamond-shaped DAGs.
	for _, index := range stableTopo {
		for dependency := range direct[index] {
			transitive[index][dependency] = struct{}{}
			for ancestor := range transitive[dependency] {
				transitive[index][ancestor] = struct{}{}
			}
		}
	}

	return quantifierDependencyOrder{
		direct:     direct,
		transitive: transitive,
		stableTopo: stableTopo,
		ok:         true,
	}
}

// enumerateQuantifierMappings visits dependency-sound equal-sized partial
// bijections in deterministic max-k-first order. The structural consumer uses
// completeOnly=true; partial subsets are exposed only as infrastructure until
// Select-specific semantics land in RFC-190.4c.
func enumerateQuantifierMappings(
	queryQuantifiers []expressions.Quantifier,
	candidateQuantifiers []expressions.Quantifier,
	completeOnly bool,
	budget *matchIntermediateSearchBudget,
	visit func([]quantifierMapping) bool,
) {
	if budget == nil || budget.shouldStop() ||
		len(queryQuantifiers) == 0 || len(candidateQuantifiers) == 0 {
		return
	}
	if completeOnly && len(queryQuantifiers) != len(candidateQuantifiers) {
		return
	}

	queryOrder := buildQuantifierDependencyOrder(queryQuantifiers)
	candidateOrder := buildQuantifierDependencyOrder(candidateQuantifiers)
	if !queryOrder.ok || !candidateOrder.ok {
		return
	}

	maxSize := len(queryQuantifiers)
	if len(candidateQuantifiers) < maxSize {
		maxSize = len(candidateQuantifiers)
	}
	minSize := 1 // k=0 never reaches Java expression subsumption.
	if completeOnly {
		minSize = maxSize
	}

	for size := maxSize; size >= minSize && !budget.shouldStop(); size-- {
		enumerateOrdinalCombinations(
			len(candidateQuantifiers),
			size,
			func(candidateSet []int) bool {
				if !budget.chargeState() {
					return false
				}
				if !dependencyConvex(
					candidateSet,
					candidateOrder.transitive,
				) {
					return !budget.shouldStop()
				}
				candidateTopo := filterTopologicalOrder(
					candidateOrder.stableTopo,
					candidateSet,
				)

				keepGoing := true
				enumerateOrdinalCombinations(
					len(queryQuantifiers),
					size,
					func(querySet []int) bool {
						if !budget.chargeState() {
							keepGoing = false
							return false
						}
						if !dependencyConvex(
							querySet,
							queryOrder.transitive,
						) {
							keepGoing = !budget.shouldStop()
							return keepGoing
						}

						enumerateTopologicalOrders(
							querySet,
							queryOrder.direct,
							budget.chargeState,
							func(queryTopo []int) bool {
								mapping := make(
									[]quantifierMapping,
									len(queryTopo),
								)
								for i := range queryTopo {
									mapping[i] = quantifierMapping{
										queryIndex:     queryTopo[i],
										candidateIndex: candidateTopo[i],
									}
								}
								if mappingDependenciesCompatible(
									mapping,
									queryOrder.transitive,
									candidateOrder.transitive,
								) {
									keepGoing = visit(mapping)
								}
								return keepGoing && !budget.shouldStop()
							},
						)
						return keepGoing && !budget.shouldStop()
					},
				)
				return keepGoing && !budget.shouldStop()
			},
		)
	}
}

// enumerateOrdinalCombinations streams lexicographic ordinal subsets.
func enumerateOrdinalCombinations(
	count, size int,
	visit func([]int) bool,
) bool {
	if size < 0 || size > count {
		return true
	}
	combination := make([]int, size)
	var recurse func(depth, next int) bool
	recurse = func(depth, next int) bool {
		if depth == size {
			snapshot := append([]int(nil), combination...)
			return visit(snapshot)
		}
		last := count - (size - depth)
		for value := next; value <= last; value++ {
			combination[depth] = value
			if !recurse(depth+1, value+1) {
				return false
			}
		}
		return true
	}
	return recurse(0, 0)
}

// dependencyConvex rejects a subset that leaves the dependency graph and
// later re-enters it: selected a depends transitively on omitted b which
// depends transitively on selected c. Boundary omissions remain legal.
func dependencyConvex(
	selected []int,
	transitive []map[int]struct{},
) bool {
	inSet := make(map[int]struct{}, len(selected))
	for _, index := range selected {
		inSet[index] = struct{}{}
	}
	for _, outer := range selected {
		for omitted := range transitive[outer] {
			if _, selectedOmitted := inSet[omitted]; selectedOmitted {
				continue
			}
			for inner := range transitive[omitted] {
				if _, selectedInner := inSet[inner]; selectedInner {
					return false
				}
			}
		}
	}
	return true
}

func filterTopologicalOrder(order, selected []int) []int {
	inSet := make(map[int]struct{}, len(selected))
	for _, index := range selected {
		inSet[index] = struct{}{}
	}
	filtered := make([]int, 0, len(selected))
	for _, index := range order {
		if _, ok := inSet[index]; ok {
			filtered = append(filtered, index)
		}
	}
	return filtered
}

// stableTopologicalOrder returns one Kahn order, breaking every eligible tie
// by original ordinal. Map iteration can therefore never perturb matching.
func stableTopologicalOrder(
	selected []int,
	direct []map[int]struct{},
) ([]int, bool) {
	inSet := make(map[int]struct{}, len(selected))
	for _, index := range selected {
		inSet[index] = struct{}{}
	}
	result := make([]int, 0, len(selected))
	chosen := make(map[int]struct{}, len(selected))
	for len(result) < len(selected) {
		found := -1
		for _, index := range selected {
			if _, already := chosen[index]; already {
				continue
			}
			eligible := true
			for dependency := range direct[index] {
				if _, relevant := inSet[dependency]; !relevant {
					continue
				}
				if _, done := chosen[dependency]; !done {
					eligible = false
					break
				}
			}
			if eligible && (found < 0 || index < found) {
				found = index
			}
		}
		if found < 0 {
			return nil, false
		}
		chosen[found] = struct{}{}
		result = append(result, found)
	}
	return result, true
}

// enumerateTopologicalOrders streams every dependency-valid permutation,
// choosing eligible ordinals in stable ascending order.
func enumerateTopologicalOrders(
	selected []int,
	direct []map[int]struct{},
	chargeExtension func() bool,
	visit func([]int) bool,
) bool {
	inSet := make(map[int]struct{}, len(selected))
	for _, index := range selected {
		inSet[index] = struct{}{}
	}
	order := make([]int, 0, len(selected))
	chosen := make(map[int]struct{}, len(selected))
	var recurse func() bool
	recurse = func() bool {
		if len(order) == len(selected) {
			return visit(append([]int(nil), order...))
		}
		hadEligible := false
		for _, index := range selected {
			if _, already := chosen[index]; already {
				continue
			}
			eligible := true
			for dependency := range direct[index] {
				if _, relevant := inSet[dependency]; !relevant {
					continue
				}
				if _, done := chosen[dependency]; !done {
					eligible = false
					break
				}
			}
			if !eligible {
				continue
			}
			hadEligible = true
			if chargeExtension != nil && !chargeExtension() {
				return false
			}
			chosen[index] = struct{}{}
			order = append(order, index)
			if !recurse() {
				return false
			}
			order = order[:len(order)-1]
			delete(chosen, index)
		}
		return hadEligible
	}
	return recurse()
}

// mappingDependenciesCompatible implements Java BaseMatcher's asymmetric
// dependency check: every selected query dependency, once mapped, must be a
// transitive dependency of the paired candidate quantifier. Candidate-only
// dependencies are left for expression-specific subsumption semantics.
func mappingDependenciesCompatible(
	mapping []quantifierMapping,
	queryTransitive []map[int]struct{},
	candidateTransitive []map[int]struct{},
) bool {
	queryToCandidate := make(map[int]int, len(mapping))
	for _, pair := range mapping {
		queryToCandidate[pair.queryIndex] = pair.candidateIndex
	}
	for _, pair := range mapping {
		for queryDependency := range queryTransitive[pair.queryIndex] {
			candidateDependency, selected := queryToCandidate[queryDependency]
			if !selected {
				continue
			}
			if _, ok := candidateTransitive[pair.candidateIndex][candidateDependency]; !ok {
				return false
			}
		}
	}
	return true
}
