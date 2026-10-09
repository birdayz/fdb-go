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
	return newMemoEquality().equalWithHashes(nil, a, aHash, nil, b, bHash, EmptyAliasMap())
}

// MemoComparison runs several MemoEqual tests against one unchanged graph,
// sharing what they derive. Discard it before the graph changes.
type MemoComparison struct {
	equality memoEquality
}

// MemberEqual is MemoEqualWithHashes for a member of ref, with the hash both
// sides share.
func (c *MemoComparison) MemberEqual(ref *Reference, member, expression RelationalExpression, hash uint64) bool {
	return c.equality.equalWithHashes(canonicalReferenceReadOnly(ref), member, hash, nil, expression, hash, EmptyAliasMap())
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
	if !expression.CanCorrelate() {
		return nil
	}
	// Quantifier dependencies follow from the children's correlations, which a
	// current expression snapshot fixes, so they are kept on the snapshot. A
	// custom correlation computer may not read every quantifier's group.
	if snapshot, ok := e.correlations.expressions[expression]; ok {
		if _, custom := expression.(correlationComputer); !custom {
			if order := snapshot.order.Load(); order != nil {
				return *order
			}
			deps := quantifierDependencies(expression.GetQuantifiers(), true, e.correlations.correlatedTo)
			snapshot.order.Store(&deps)
			return deps
		}
	}
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
	return &memoEquality{}
}

func (e *memoEquality) hash(expression RelationalExpression) uint64 {
	return e.hashIn(nil, expression)
}

// hashIn is hash for a member of ref, read from ref's memoized member hashes
// when it has one.
func (e *memoEquality) hashIn(ref *Reference, expression RelationalExpression) uint64 {
	if ref != nil {
		if hash, ok := ref.memberHash[expression]; ok {
			return hash
		}
	}
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
		e.hashIn(memberRef, member) != e.hashIn(expressionRef, expression) {
		return false
	}
	return e.equalBody(memberRef, member, expressionRef, expression, aliases)
}

// equalWithHashes is equalIn given both sides' node hashes.
func (e *memoEquality) equalWithHashes(memberRef *Reference, member RelationalExpression, memberHash uint64, expressionRef *Reference, expression RelationalExpression, expressionHash uint64, aliases *AliasMap) bool {
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
		memberHash != expressionHash {
		return false
	}
	return e.equalBody(memberRef, member, expressionRef, expression, aliases)
}

// equalBody is equalIn after the node shape and hashes agree.
func (e *memoEquality) equalBody(memberRef *Reference, member RelationalExpression, expressionRef *Reference, expression RelationalExpression, aliases *AliasMap) bool {
	if !childGroupsCanMatch(member, expression) {
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
	if !a.memberSignature().covers(b.memberSignature()) {
		return false
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
	quantifiers := e.GetQuantifiers()
	owned := func(alias values.CorrelationIdentifier) bool {
		for _, quantifier := range quantifiers {
			if quantifier.GetAlias() == alias {
				return true
			}
		}
		return false
	}
	for alias := range e.GetCorrelatedToWithoutChildren() {
		if !owned(alias) {
			result[alias] = struct{}{}
		}
	}
	canCorrelate := e.CanCorrelate()
	for _, quantifier := range quantifiers {
		for alias := range childCorrelations(quantifier.GetRangesOver()) {
			if !canCorrelate || !owned(alias) {
				result[alias] = struct{}{}
			}
		}
	}
	return result
}

// memberSignature is the set of member shapes a group's lanes hold: the
// (hash, arity, correlatability) every memo-equal pair agrees on.
type memberSignature struct {
	version            uint64
	exploratory, final []uint64
	// hashes are the exploratory members' hashes, sorted.
	hashes []uint64
}

// memberSignature returns r's signature for its current members. r must be
// canonical.
func (r *Reference) memberSignature() *memberSignature {
	if signature := r.signature.Load(); signature != nil && signature.version == r.memberVersion {
		return signature
	}
	signature := &memberSignature{version: r.memberVersion}
	signature.exploratory, signature.hashes = r.memberShapes(r.members, true)
	signature.final, _ = r.memberShapes(r.finalMembers, false)
	r.signature.Store(signature)
	return signature
}

// hasExploratoryHash reports whether an exploratory member has hash.
func (s *memberSignature) hasExploratoryHash(hash uint64) bool {
	_, found := slices.BinarySearch(s.hashes, hash)
	return found
}

func (r *Reference) memberShapes(members []RelationalExpression, withHashes bool) (shapes, hashes []uint64) {
	shapes = make([]uint64, 0, len(members))
	if withHashes {
		hashes = make([]uint64, 0, len(members))
	}
	for _, member := range members {
		hash, ok := r.memberHash[member]
		if !ok {
			hash = member.HashCodeWithoutChildren()
		}
		if withHashes {
			hashes = append(hashes, hash)
		}
		shape := (hash*31+uint64(len(member.GetQuantifiers())))*2 + 1
		if member.CanCorrelate() {
			shape++
		}
		shapes = append(shapes, shape)
	}
	slices.Sort(shapes)
	slices.Sort(hashes)
	return slices.Compact(shapes), hashes
}

// covers reports whether every shape of other occurs in s, lane by lane: a
// group can contain another only if each of its members has a candidate.
func (s *memberSignature) covers(other *memberSignature) bool {
	return shapesCover(s.exploratory, other.exploratory) && shapesCover(s.final, other.final)
}

func shapesCover(have, want []uint64) bool {
	i := 0
	for _, shape := range want {
		for i < len(have) && have[i] < shape {
			i++
		}
		if i == len(have) || have[i] != shape {
			return false
		}
	}
	return true
}

// childGroupsCanMatch reports whether the children of member and expression
// pair up, as the binding search may pair them, into groups whose signatures
// allow containment. The search needs such a pairing; finding none here spares
// deriving correlations and descending.
func childGroupsCanMatch(member, expression RelationalExpression) bool {
	left, right := member.GetQuantifiers(), expression.GetQuantifiers()
	if len(left) == 0 || len(left) != len(right) {
		return true
	}
	var buf [16]*memberSignature
	signatures := buf[:0]
	if 2*len(left) > len(buf) {
		signatures = make([]*memberSignature, 0, 2*len(left))
	}
	for _, quantifiers := range [][]Quantifier{left, right} {
		for _, quantifier := range quantifiers {
			var signature *memberSignature
			if ref := canonicalReferenceReadOnly(quantifier.rangesOver); ref != nil {
				signature = ref.memberSignature()
			}
			signatures = append(signatures, signature)
		}
	}
	have, want := signatures[:len(left)], signatures[len(left):]
	if !member.ChildrenAsSet() || !expression.ChildrenAsSet() {
		for i := range have {
			if !signatureCovers(have[i], want[i]) {
				return false
			}
		}
		return true
	}
	var used uint32
	var pair func(int) bool
	pair = func(i int) bool {
		if i == len(have) {
			return true
		}
		for j := range want {
			if used&(1<<j) == 0 && signatureCovers(have[i], want[j]) {
				used |= 1 << j
				if pair(i + 1) {
					return true
				}
				used &^= 1 << j
			}
		}
		return false
	}
	return len(want) > 32 || pair(0)
}

func signatureCovers(have, want *memberSignature) bool {
	if have == nil || want == nil {
		return have == nil && want == nil
	}
	return have.covers(want)
}
