package expressions

import (
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
)

// MemoEqual reports whether two expressions are the SAME memo member —
// alias-aware structural equality where two sub-expressions identical up to a
// consistent renaming of quantifier aliases are recognized as one member.
//
// Faithful port of Java Reference.isMemoizedExpression (RFC-039). Sequence:
// hashCodeWithoutChildren → quantifier count → external-correlation guard →
// canCorrelate → bindIdentities/combine → directional in-memo child match
// (containsAllInMemo; ChildrenAsSet via capped permutation) →
// equalsWithoutChildren under the BUILT (node's own) quantifier-alias map.
//
// This is the activation (RFC-038 PR-A) of the RFC-040 foundation: the
// foundation made EqualsWithoutChildren alias-aware and HashCodeWithoutChildren
// alias-invariant; memoEqual builds the node's own quantifier-alias map and
// feeds it to EqualsWithoutChildren — which the memo's compare sites use so
// rule-rewritten equivalents (fresh aliases) intern/merge together.
//
// Distinct from SemanticEquals (which passes only the INCOMING alias map to the
// top-level EqualsWithoutChildren, missing same-level quantifier-alias
// canonicalization — exactly why interning was alias-limited before).
func MemoEqual(a, b RelationalExpression) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return memoEqual(a, b, EmptyAliasMap(), map[[2]*Reference]bool{})
}

func memoEqual(member, expr RelationalExpression, equiv *AliasMap, seen map[[2]*Reference]bool) bool {
	if member == expr {
		return true
	}
	if member.HashCodeWithoutChildren() != expr.HashCodeWithoutChildren() {
		return false
	}
	mq := member.GetQuantifiers()
	eq := expr.GetQuantifiers()
	if len(mq) != len(eq) {
		return false
	}
	// External-correlation guard (Java Reference.java:764–773): correlations
	// these nodes depend on (excluding their own quantifiers) must correspond
	// under equiv — prevents interning nodes that differ only in an outer
	// correlation (…=a.x vs …=b.y).
	if !correlatedToMatches(member, expr, equiv) {
		return false
	}
	if member.CanCorrelate() != expr.CanCorrelate() {
		return false
	}
	// bindIdentities + combine: fold the shared external correlations into
	// equiv as identities so a node correlated to a sibling/outer quantifier
	// is canonicalized identically on both sides.
	equiv = combineIdentities(equiv, member)
	// Match children, building the node's own quantifier-alias map.
	built, ok := matchChildrenInMemo(member, expr, mq, eq, equiv, seen)
	if !ok {
		return false
	}
	return member.EqualsWithoutChildren(expr, built)
}

// matchChildrenInMemo matches member's and expr's child Quantifiers, building
// the node's own quantifier-alias map (member.q[i]↦expr.q[i]). Order-significant
// children pair positionally; ChildrenAsSet nodes try permutations (capped at
// MaxPermutationChildren). Each child pair is compared via DIRECTIONAL
// childRefsMatchInMemo. Returns the built map.
func matchChildrenInMemo(member, expr RelationalExpression, mq, eq []Quantifier, equiv *AliasMap, seen map[[2]*Reference]bool) (*AliasMap, bool) {
	n := len(mq)
	if n == 0 {
		return equiv, true
	}
	if member.ChildrenAsSet() && expr.ChildrenAsSet() && n <= MaxPermutationChildren {
		indices := make([]int, n)
		for i := range indices {
			indices[i] = i
		}
		var built *AliasMap
		found := permute(indices, 0, func(perm []int) bool {
			b := equiv
			for i := 0; i < n; i++ {
				if !childRefsMatchInMemo(mq[i].GetRangesOver(), eq[perm[i]].GetRangesOver(), b, seen) {
					return false
				}
				nb, ok := b.With(mq[i].GetAlias(), eq[perm[i]].GetAlias())
				if !ok {
					return false
				}
				b = nb
			}
			built = b
			return true
		})
		if found {
			return built, true
		}
		return nil, false
	}
	b := equiv
	for i := 0; i < n; i++ {
		if !childRefsMatchInMemo(mq[i].GetRangesOver(), eq[i].GetRangesOver(), b, seen) {
			return nil, false
		}
		nb, ok := b.With(mq[i].GetAlias(), eq[i].GetAlias())
		if !ok {
			return nil, false
		}
		b = nb
	}
	return b, true
}

// childRefsMatchInMemo is DIRECTIONAL containsAllInMemo (Java
// Reference.containsAllInMemo): every member of `b` must be matched by SOME
// member of `a` under equiv. Pointer-canonical equality is the fast path;
// otherwise it recurses (the self-cycle guard yields fresh InitialOf children
// for the PushFilterThroughDistinct family, so equal children can be distinct
// pointers). `seen` is a recursion STACK guard (added on entry, removed on
// exit) against cyclic DAGs — defensive; the live DAG is acyclic.
func childRefsMatchInMemo(a, b *Reference, equiv *AliasMap, seen map[[2]*Reference]bool) bool {
	if a == nil || b == nil {
		return a == b
	}
	a = a.Canonical()
	b = b.Canonical()
	if a == b {
		return true
	}
	key := [2]*Reference{a, b}
	if seen[key] {
		return true
	}
	seen[key] = true
	defer delete(seen, key)
	for _, mb := range b.Members() {
		matched := false
		for _, ma := range a.Members() {
			if memoEqual(ma, mb, equiv, seen) {
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

// correlatedToMatches ports Java Reference.java:764–773: maps member's external
// correlations through equiv (identity fast path when equiv defines only
// identities) and requires the mapped set to equal expr's external correlations.
func correlatedToMatches(member, expr RelationalExpression, equiv *AliasMap) bool {
	mc := expressionCorrelatedTo(member)
	ec := expressionCorrelatedTo(expr)
	if len(mc) != len(ec) {
		return false
	}
	identities := equiv.DefinesOnlyIdentities()
	for alias := range mc {
		mapped := alias
		if !identities {
			mapped = equiv.GetTargetOrDefault(alias, alias)
		}
		if _, ok := ec[mapped]; !ok {
			return false
		}
	}
	return true
}

// combineIdentities binds member's external correlations into equiv as identity
// bindings (alias↦alias) when not already bound — Java's bindIdentities +
// combine. correlatedToMatches has verified the correspondence.
func combineIdentities(equiv *AliasMap, member RelationalExpression) *AliasMap {
	out := equiv
	for alias := range expressionCorrelatedTo(member) {
		if _, bound := out.GetTarget(alias); bound {
			continue
		}
		if next, ok := out.With(alias, alias); ok {
			out = next
		}
	}
	return out
}

// expressionCorrelatedTo returns the full external correlation set of an
// expression — its own correlations plus children's, minus the aliases bound by
// its own quantifiers. Mirrors the per-member body of Reference.GetCorrelatedTo.
func expressionCorrelatedTo(e RelationalExpression) map[values.CorrelationIdentifier]struct{} {
	own := make(map[values.CorrelationIdentifier]struct{})
	for _, q := range e.GetQuantifiers() {
		own[q.GetAlias()] = struct{}{}
	}
	result := make(map[values.CorrelationIdentifier]struct{})
	// EXTERNAL correlations only: a node's own node-info (e.g. a Filter's
	// predicate) may reference its own quantifier alias, which is LOCALLY
	// BOUND, not external — exclude own aliases from every contribution.
	for k := range e.GetCorrelatedToWithoutChildren() {
		if _, bound := own[k]; !bound {
			result[k] = struct{}{}
		}
	}
	for _, q := range e.GetQuantifiers() {
		child := q.GetRangesOver()
		if child == nil {
			continue
		}
		for k := range child.GetCorrelatedTo() {
			if _, bound := own[k]; !bound {
				result[k] = struct{}{}
			}
		}
	}
	return result
}
