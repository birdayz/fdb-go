package expressions

import "fdb.dev/pkg/recordlayer/query/plan/cascades/values"

// preparedSameChildReferences is the prepared-admission form of
// sameChildReferences. It resolves forwarding without path compression, so a
// batch that is later rejected cannot mutate child topology merely by running
// the fast equality tier.
func preparedSameChildReferences(a, b RelationalExpression) bool {
	aQs := a.GetQuantifiers()
	bQs := b.GetQuantifiers()
	if len(aQs) != len(bQs) {
		return false
	}
	for i := range aQs {
		if preparedQuantifierReference(aQs[i]) != preparedQuantifierReference(bQs[i]) {
			return false
		}
		if !quantifierAttributesEqual(aQs[i], bQs[i]) {
			return false
		}
	}
	return true
}

func preparedQuantifierReference(quantifier Quantifier) *Reference {
	return canonicalReferenceReadOnly(quantifier.rangesOver)
}

// preparedSemanticEquals is SemanticEquals with read-only Reference traversal.
// Expression methods remain safe here because root admission has already
// exact-recognized and result-type checked the complete population.
func preparedSemanticEquals(a, b RelationalExpression, aliases *AliasMap) bool {
	return preparedSemanticEqualsGuarded(a, b, aliases, nil)
}

func preparedSemanticEqualsGuarded(
	a, b RelationalExpression,
	aliases *AliasMap,
	visited map[refPair]struct{},
) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	if a == b {
		return true
	}
	if !a.EqualsWithoutChildren(b, aliases) {
		return false
	}
	aQs := a.GetQuantifiers()
	bQs := b.GetQuantifiers()
	if len(aQs) != len(bQs) {
		return false
	}
	if len(aQs) == 0 {
		return true
	}
	if a.ChildrenAsSet() && b.ChildrenAsSet() && len(aQs) <= MaxPermutationChildren {
		return preparedSemanticChildrenPermuted(aQs, bQs, aliases, visited)
	}
	return preparedSemanticChildrenPositional(aQs, bQs, aliases, visited)
}

func preparedSemanticChildrenPositional(
	aQs, bQs []Quantifier,
	aliases *AliasMap,
	visited map[refPair]struct{},
) bool {
	for i := range aQs {
		if !quantifierAttributesEqual(aQs[i], bQs[i]) {
			return false
		}
	}
	composed, ok := composeChildAliasPairs(aliases, aQs, bQs)
	if !ok {
		return false
	}
	for i := range aQs {
		if !preparedChildExpressionsEqual(
			preparedQuantifierReference(aQs[i]),
			preparedQuantifierReference(bQs[i]),
			composed,
			visited,
		) {
			return false
		}
	}
	return true
}

func preparedSemanticChildrenPermuted(
	aQs, bQs []Quantifier,
	aliases *AliasMap,
	visited map[refPair]struct{},
) bool {
	indices := make([]int, len(aQs))
	for i := range indices {
		indices[i] = i
	}
	permutedB := make([]Quantifier, len(bQs))
	return permute(indices, 0, func(permutation []int) bool {
		for i := range aQs {
			permutedB[i] = bQs[permutation[i]]
			if !quantifierAttributesEqual(aQs[i], permutedB[i]) {
				return false
			}
		}
		composed, ok := composeChildAliasPairs(aliases, aQs, permutedB)
		if !ok {
			return false
		}
		for i := range aQs {
			if !preparedChildExpressionsEqual(
				preparedQuantifierReference(aQs[i]),
				preparedQuantifierReference(permutedB[i]),
				composed,
				visited,
			) {
				return false
			}
		}
		return true
	})
}

func preparedChildExpressionsEqual(
	a, b *Reference,
	aliases *AliasMap,
	visited map[refPair]struct{},
) bool {
	a = canonicalReferenceReadOnly(a)
	b = canonicalReferenceReadOnly(b)
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	if a == b {
		return true
	}
	pair := refPair{a: a, b: b}
	if _, onPath := visited[pair]; onPath {
		return false
	}
	if visited == nil {
		visited = make(map[refPair]struct{}, 4)
	}
	visited[pair] = struct{}{}
	equal := preparedSemanticEqualsGuarded(
		preparedReferenceFirstMember(a),
		preparedReferenceFirstMember(b),
		aliases,
		visited,
	)
	delete(visited, pair)
	return equal
}

func preparedReferenceFirstMember(reference *Reference) RelationalExpression {
	reference = canonicalReferenceReadOnly(reference)
	if reference == nil {
		return nil
	}
	if len(reference.members) > 0 {
		return reference.members[0]
	}
	if len(reference.finalMembers) > 0 {
		return reference.finalMembers[0]
	}
	return nil
}

// preparedMemoEqual is MemoEqual with a transaction-local correlation reader
// and non-compressing Reference traversal. No shared cache or forwarding link
// is written while a batch is merely being prepared.
func preparedMemoEqual(a, b RelationalExpression) bool {
	correlations := &preparedCorrelationReader{
		memo:   make(map[*Reference]map[values.CorrelationIdentifier]struct{}),
		active: make(map[*Reference]struct{}),
	}
	return preparedMemoEqualGuarded(a, b, EmptyAliasMap(), nil, correlations)
}

func preparedMemoEqualGuarded(
	member, expression RelationalExpression,
	equivalent *AliasMap,
	visited map[refPair]struct{},
	correlations *preparedCorrelationReader,
) bool {
	if member == nil || expression == nil {
		return member == nil && expression == nil
	}
	if member == expression {
		return true
	}
	if member.HashCodeWithoutChildren() != expression.HashCodeWithoutChildren() {
		return false
	}
	memberQuantifiers := member.GetQuantifiers()
	expressionQuantifiers := expression.GetQuantifiers()
	if len(memberQuantifiers) != len(expressionQuantifiers) {
		return false
	}
	if !preparedCorrelatedToMatches(member, expression, equivalent, correlations) {
		return false
	}
	if member.CanCorrelate() != expression.CanCorrelate() {
		return false
	}
	equivalent = preparedCombineIdentities(equivalent, member, correlations)
	built, ok := preparedMatchChildrenInMemo(
		member,
		expression,
		memberQuantifiers,
		expressionQuantifiers,
		equivalent,
		visited,
		correlations,
	)
	return ok && member.EqualsWithoutChildren(expression, built)
}

func preparedMatchChildrenInMemo(
	member, expression RelationalExpression,
	memberQuantifiers, expressionQuantifiers []Quantifier,
	equivalent *AliasMap,
	visited map[refPair]struct{},
	correlations *preparedCorrelationReader,
) (*AliasMap, bool) {
	count := len(memberQuantifiers)
	if count == 0 {
		return equivalent, true
	}
	if member.ChildrenAsSet() && expression.ChildrenAsSet() && count <= MaxPermutationChildren {
		indices := make([]int, count)
		for i := range indices {
			indices[i] = i
		}
		var built *AliasMap
		found := permute(indices, 0, func(permutation []int) bool {
			candidate := equivalent
			for i := 0; i < count; i++ {
				expressionQuantifier := expressionQuantifiers[permutation[i]]
				if !quantifierAttributesEqual(memberQuantifiers[i], expressionQuantifier) ||
					!preparedChildRefsMatchInMemo(
						preparedQuantifierReference(memberQuantifiers[i]),
						preparedQuantifierReference(expressionQuantifier),
						candidate,
						visited,
						correlations,
					) {
					return false
				}
				next, ok := candidate.With(memberQuantifiers[i].GetAlias(), expressionQuantifier.GetAlias())
				if !ok {
					return false
				}
				candidate = next
			}
			built = candidate
			return true
		})
		return built, found
	}

	built := equivalent
	for i := 0; i < count; i++ {
		if !quantifierAttributesEqual(memberQuantifiers[i], expressionQuantifiers[i]) ||
			!preparedChildRefsMatchInMemo(
				preparedQuantifierReference(memberQuantifiers[i]),
				preparedQuantifierReference(expressionQuantifiers[i]),
				built,
				visited,
				correlations,
			) {
			return nil, false
		}
		next, ok := built.With(memberQuantifiers[i].GetAlias(), expressionQuantifiers[i].GetAlias())
		if !ok {
			return nil, false
		}
		built = next
	}
	return built, true
}

func preparedChildRefsMatchInMemo(
	a, b *Reference,
	equivalent *AliasMap,
	visited map[refPair]struct{},
	correlations *preparedCorrelationReader,
) bool {
	a = canonicalReferenceReadOnly(a)
	b = canonicalReferenceReadOnly(b)
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	if a == b {
		return true
	}
	pair := refPair{a: a, b: b}
	if _, onPath := visited[pair]; onPath {
		return false
	}
	if visited == nil {
		visited = make(map[refPair]struct{}, 4)
	}
	visited[pair] = struct{}{}
	matches := preparedMembersContainAllInMemo(a.members, b.members, equivalent, visited, correlations) &&
		preparedMembersContainAllInMemo(a.finalMembers, b.finalMembers, equivalent, visited, correlations)
	delete(visited, pair)
	return matches
}

func preparedMembersContainAllInMemo(
	have, want []RelationalExpression,
	equivalent *AliasMap,
	visited map[refPair]struct{},
	correlations *preparedCorrelationReader,
) bool {
	for _, wanted := range want {
		matched := false
		for _, candidate := range have {
			if preparedMemoEqualGuarded(candidate, wanted, equivalent, visited, correlations) {
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
	}
	return true
}

func preparedCorrelatedToMatches(
	member, expression RelationalExpression,
	equivalent *AliasMap,
	correlations *preparedCorrelationReader,
) bool {
	memberCorrelations := correlations.expression(member)
	expressionCorrelations := correlations.expression(expression)
	if len(memberCorrelations) != len(expressionCorrelations) {
		return false
	}
	identities := equivalent.DefinesOnlyIdentities()
	for alias := range memberCorrelations {
		mapped := alias
		if !identities {
			mapped = equivalent.GetTargetOrDefault(alias, alias)
		}
		if _, ok := expressionCorrelations[mapped]; !ok {
			return false
		}
	}
	return true
}

func preparedCombineIdentities(
	equivalent *AliasMap,
	member RelationalExpression,
	correlations *preparedCorrelationReader,
) *AliasMap {
	out := equivalent
	for alias := range correlations.expression(member) {
		if _, bound := out.GetTarget(alias); bound {
			continue
		}
		if next, ok := out.With(alias, alias); ok {
			out = next
		}
	}
	return out
}

type preparedCorrelationReader struct {
	memo   map[*Reference]map[values.CorrelationIdentifier]struct{}
	active map[*Reference]struct{}
}

func (r *preparedCorrelationReader) expression(expression RelationalExpression) map[values.CorrelationIdentifier]struct{} {
	boundHere := make(map[values.CorrelationIdentifier]struct{})
	for _, quantifier := range expression.GetQuantifiers() {
		boundHere[quantifier.GetAlias()] = struct{}{}
	}
	result := make(map[values.CorrelationIdentifier]struct{})
	for alias := range expression.GetCorrelatedToWithoutChildren() {
		if _, bound := boundHere[alias]; !bound {
			result[alias] = struct{}{}
		}
	}
	for _, quantifier := range expression.GetQuantifiers() {
		for alias := range r.reference(preparedQuantifierReference(quantifier)) {
			if _, bound := boundHere[alias]; !bound {
				result[alias] = struct{}{}
			}
		}
	}
	return result
}

func (r *preparedCorrelationReader) reference(reference *Reference) map[values.CorrelationIdentifier]struct{} {
	reference = canonicalReferenceReadOnly(reference)
	if reference == nil {
		return map[values.CorrelationIdentifier]struct{}{}
	}
	if cached, ok := r.memo[reference]; ok {
		return cached
	}
	if _, cycle := r.active[reference]; cycle {
		return map[values.CorrelationIdentifier]struct{}{}
	}
	r.active[reference] = struct{}{}
	result := make(map[values.CorrelationIdentifier]struct{})
	accumulate := func(member RelationalExpression) {
		boundHere := make(map[values.CorrelationIdentifier]struct{})
		for _, quantifier := range member.GetQuantifiers() {
			boundHere[quantifier.GetAlias()] = struct{}{}
		}
		for alias := range member.GetCorrelatedToWithoutChildren() {
			if _, bound := boundHere[alias]; !bound {
				result[alias] = struct{}{}
			}
		}
		for _, quantifier := range member.GetQuantifiers() {
			for alias := range r.reference(preparedQuantifierReference(quantifier)) {
				if _, bound := boundHere[alias]; !member.CanCorrelate() || !bound {
					result[alias] = struct{}{}
				}
			}
		}
	}
	for _, member := range reference.members {
		accumulate(member)
	}
	for _, member := range reference.finalMembers {
		accumulate(member)
	}
	delete(r.active, reference)
	r.memo[reference] = result
	return result
}
