package expressions

import (
	"slices"
	"strings"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// MemoEqual implements Java Reference.isMemoizedExpression: external bindings
// stay fixed, while child bindings are matched before comparing node information.
func MemoEqual(a, b RelationalExpression) bool {
	return newMemoEquality().equal(a, b, EmptyAliasMap())
}

// MemoEqualWithHashes reuses the caller's verified immutable node hashes.
func MemoEqualWithHashes(a, b RelationalExpression, aHash, bHash uint64) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	equality := newMemoEquality()
	equality.hashes[a] = aHash
	equality.hashes[b] = bHash
	return equality.equal(a, b, EmptyAliasMap())
}

type refPair struct{ a, b *Reference }

type memoEquality struct {
	correlations referenceCorrelationReader
	hashes       map[RelationalExpression]uint64
	active       map[refPair]struct{}
	// matched remembers reference comparisons on a stable graph. A comparison
	// depends on the bindings only through the left reference's free aliases:
	// the bindings it is given are its ancestors', which no descendant
	// quantifier shares in an acyclic memo.
	matched map[refPair][]refMatch
	// cycles counts comparisons cut short by the cycle guard; a result that
	// relied on one is not remembered.
	cycles int
	// quantifierOrder caches each expression's quantifier dependencies.
	quantifierOrder map[RelationalExpression][][]int
}

func (e *memoEquality) dependencies(expression RelationalExpression) [][]int {
	if deps, ok := e.quantifierOrder[expression]; ok {
		return deps
	}
	deps := quantifierDependencies(expression.GetQuantifiers(), expression.CanCorrelate(), e.correlations.correlatedTo)
	if e.quantifierOrder == nil {
		e.quantifierOrder = make(map[RelationalExpression][][]int)
	}
	e.quantifierOrder[expression] = deps
	return deps
}

type refMatch struct {
	bindings []values.AliasPair
	result   bool
}

func newMemoEquality() *memoEquality {
	return &memoEquality{hashes: make(map[RelationalExpression]uint64)}
}

func (e *memoEquality) hash(expression RelationalExpression) uint64 {
	if hash, ok := e.hashes[expression]; ok {
		return hash
	}
	hash := expression.HashCodeWithoutChildren()
	if e.hashes == nil {
		e.hashes = make(map[RelationalExpression]uint64)
	}
	e.hashes[expression] = hash
	return hash
}

func (e *memoEquality) equal(member, expression RelationalExpression, aliases *AliasMap) bool {
	return e.equalIn(nil, member, nil, expression, aliases)
}

// equalIn is equal for a member of memberRef and an expression of
// expressionRef, either nil when unknown; a member's group carries its
// correlation snapshot from earlier reads.
func (e *memoEquality) equalIn(memberRef *Reference, member RelationalExpression, expressionRef *Reference, expression RelationalExpression, aliases *AliasMap) bool {
	if member == nil || expression == nil {
		return member == nil && expression == nil
	}
	if aliases == nil {
		aliases = EmptyAliasMap()
	}
	if member == expression && aliases.DefinesOnlyIdentities() {
		return true
	}
	if member.CanCorrelate() != expression.CanCorrelate() ||
		len(member.GetQuantifiers()) != len(expression.GetQuantifiers()) ||
		e.hash(member) != e.hash(expression) {
		return false
	}
	memberCorrelations := e.correlations.expressionIn(memberRef, member)
	otherCorrelations := e.correlations.expressionIn(expressionRef, expression)
	if len(memberCorrelations) != len(otherCorrelations) {
		return false
	}
	bound := aliases
	for source := range memberCorrelations {
		target := aliases.GetTargetOrDefault(source, source)
		if _, present := otherCorrelations[target]; !present {
			return false
		}
		var ok bool
		bound, ok = bound.With(source, target)
		if !ok {
			return false
		}
	}
	return matchQuantifierBindings(member, expression, bound, e.dependencies, e.references, nil)
}

// References compare complete exploratory and final populations separately.
// Containment is directional: an existing group may expose more alternatives.
func (e *memoEquality) references(a, b *Reference, aliases *AliasMap) bool {
	a, b = canonicalReferenceReadOnly(a), canonicalReferenceReadOnly(b)
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	if a == b && aliases.DefinesOnlyIdentities() {
		return true
	}
	pair := refPair{a, b}
	if _, cycle := e.active[pair]; cycle {
		e.cycles++
		return false
	}
	bindings := e.boundFreeAliases(a, aliases)
	for _, m := range e.matched[pair] {
		if slices.Equal(m.bindings, bindings) {
			return m.result
		}
	}
	if e.active == nil {
		e.active = make(map[refPair]struct{})
	}
	e.active[pair] = struct{}{}
	cycles := e.cycles
	result := e.members(a, a.members, b, b.members, aliases) && e.members(a, a.finalMembers, b, b.finalMembers, aliases)
	delete(e.active, pair)
	if e.cycles == cycles {
		if e.matched == nil {
			e.matched = make(map[refPair][]refMatch)
		}
		e.matched[pair] = append(e.matched[pair], refMatch{bindings, result})
	}
	return result
}

// boundFreeAliases is aliases restricted to ref's free aliases, ordered by
// name; an order tie only costs a cache miss.
func (e *memoEquality) boundFreeAliases(ref *Reference, aliases *AliasMap) []values.AliasPair {
	free := e.correlations.correlatedTo(ref)
	if len(free) == 0 || aliases.IsEmpty() {
		return nil
	}
	var pairs []values.AliasPair
	for source := range free {
		if target, ok := aliases.GetTarget(source); ok {
			pairs = append(pairs, values.AliasPair{Source: source, Target: target})
		}
	}
	slices.SortFunc(pairs, func(x, y values.AliasPair) int {
		if c := strings.Compare(x.Source.Name(), y.Source.Name()); c != 0 {
			return c
		}
		return strings.Compare(x.Target.Name(), y.Target.Name())
	})
	return pairs
}

func (e *memoEquality) members(haveRef *Reference, have []RelationalExpression, wantRef *Reference, want []RelationalExpression, aliases *AliasMap) bool {
	for _, wanted := range want {
		found := false
		for _, member := range have {
			if e.equalIn(haveRef, member, wantRef, wanted, aliases) {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

// GetCorrelatedToOfExpression returns member-local external correlations rather
// than the union of correlations over the member's entire memo group.
func GetCorrelatedToOfExpression(e RelationalExpression) map[values.CorrelationIdentifier]struct{} {
	return expressionCorrelations(e, func(ref *Reference) map[values.CorrelationIdentifier]struct{} {
		if ref == nil {
			return nil
		}
		return ref.GetCorrelatedTo()
	})
}

// Java overrides computeCorrelatedTo for operators with non-quantifier bindings.
// Child reads must stay in the caller's dependency-tracking traversal.
type correlationComputer interface {
	ComputeCorrelatedTo(func(*Reference) map[values.CorrelationIdentifier]struct{}) map[values.CorrelationIdentifier]struct{}
}

func expressionCorrelations(e RelationalExpression, childCorrelations func(*Reference) map[values.CorrelationIdentifier]struct{}) map[values.CorrelationIdentifier]struct{} {
	if e == nil {
		return nil
	}
	if custom, ok := e.(correlationComputer); ok {
		return custom.ComputeCorrelatedTo(childCorrelations)
	}
	result := make(map[values.CorrelationIdentifier]struct{})
	owned := make(map[values.CorrelationIdentifier]struct{})
	for _, quantifier := range e.GetQuantifiers() {
		owned[quantifier.GetAlias()] = struct{}{}
	}
	for alias := range e.GetCorrelatedToWithoutChildren() {
		if _, bound := owned[alias]; !bound {
			result[alias] = struct{}{}
		}
	}
	for _, quantifier := range e.GetQuantifiers() {
		for alias := range childCorrelations(quantifier.GetRangesOver()) {
			if _, bound := owned[alias]; !e.CanCorrelate() || !bound {
				result[alias] = struct{}{}
			}
		}
	}
	return result
}
