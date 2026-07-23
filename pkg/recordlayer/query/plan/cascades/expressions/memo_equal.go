package expressions

import (
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
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
	return memoEqual(a, b, EmptyAliasMap(), nil)
}

// refPair keys the on-path cycle guard for childRefsMatchInMemo (see
// childRefsMatchInMemo for why).
type refPair struct{ a, b *Reference }

func memoEqual(member, expr RelationalExpression, equiv *AliasMap, visited map[refPair]struct{}) bool {
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
	built, ok := matchChildrenInMemo(member, expr, mq, eq, equiv, visited)
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
func matchChildrenInMemo(member, expr RelationalExpression, mq, eq []Quantifier, equiv *AliasMap, visited map[refPair]struct{}) (*AliasMap, bool) {
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
				if !quantifierAttributesEqual(mq[i], eq[perm[i]]) {
					return false
				}
				if !childRefsMatchInMemo(mq[i].GetRangesOver(), eq[perm[i]].GetRangesOver(), b, visited) {
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
		// Edge attributes (kind / null-on-empty / strict-single) are part
		// of memo identity — see sameChildReferences and
		// quantifierAttributesEqual.
		if !quantifierAttributesEqual(mq[i], eq[i]) {
			return nil, false
		}
		if !childRefsMatchInMemo(mq[i].GetRangesOver(), eq[i].GetRangesOver(), b, visited) {
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
// Reference.containsAllInMemo, Reference.java:433-454): every member of `b`
// must be matched by SOME member of `a` under equiv. Pointer-canonical equality
// is the fast path (Java's `this == otherRef`); otherwise it recurses — rule
// rewrites yield fresh InitialOf children (e.g. PushFilterThroughDistinct), so
// equal children can be distinct pointers. Recursion descends strictly through
// child References, which form an acyclic DAG (the cross-group merge guard in
// memo_merge.go, walking AllMembers(), forbids creating a cycle), so it
// terminates. The `visited` on-path pair set is defense-in-depth for that
// invariant: in normal (acyclic) operation no reference pair is revisited on a
// single descent path, so it never triggers and behavior is identical; should a
// cycle ever reach here (a merge-guard under-approximation, a future bug), the
// re-entered pair is treated as NOT matching — a missed intern (harmless
// duplicate member), never a non-terminating recursion. A hang is worse than a
// wrong row. The set is backtracking (delete on return) so it stays a strict
// on-path set: a pair reachable via two SIBLING paths (a memo diamond) still
// compares normally.
//
// Exploratory and final members match SEPARATELY (exploratory↦exploratory,
// final↦final) — exactly Java's two-call structure (Reference.java:439-440),
// never cross-matching a logical rewrite against a physical plan. During the
// REWRITING-phase interning that drives the activation, finalMembers are empty
// so the second check is a no-op; the explicit final↦final pass keeps the port
// faithful for any future PLANNING-phase caller.
func childRefsMatchInMemo(a, b *Reference, equiv *AliasMap, visited map[refPair]struct{}) bool {
	if a == nil || b == nil {
		return a == b
	}
	a = a.Canonical()
	b = b.Canonical()
	if a == b {
		return true
	}
	key := refPair{a, b}
	if _, onPath := visited[key]; onPath {
		return false
	}
	if visited == nil {
		visited = make(map[refPair]struct{}, 4)
	}
	visited[key] = struct{}{}
	ok := membersContainAllInMemo(a.Members(), b.Members(), equiv, visited) &&
		membersContainAllInMemo(a.FinalMembers(), b.FinalMembers(), equiv, visited)
	delete(visited, key)
	return ok
}

// membersContainAllInMemo reports whether every expression in want is matched
// by SOME expression in have under equiv (Java Members.containsInMemo loop,
// Reference.java:444-453 / 1013-1018). Directional, not bidirectional.
func membersContainAllInMemo(have, want []RelationalExpression, equiv *AliasMap, visited map[refPair]struct{}) bool {
	for _, w := range want {
		matched := false
		for _, h := range have {
			if memoEqual(h, w, equiv, visited) {
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
		// Java filters own aliases from the children contribution only when
		// canCorrelate() (RelationalExpression.computeCorrelatedTo, line 72:
		// `!canCorrelate() || ...`). Subtracting unconditionally here is safe:
		// a child Reference cannot correlate to the alias that names it, so the
		// own-alias set never intersects the children's correlations — the
		// guard is structurally unreachable, and dropping it can only ever
		// remove an alias that wasn't present.
		for k := range child.GetCorrelatedTo() {
			if _, bound := own[k]; !bound {
				result[k] = struct{}{}
			}
		}
	}
	return result
}

// GetCorrelatedToOfExpression returns the exact external correlations of one
// expression member. Unlike Reference.GetCorrelatedTo, it does not union the
// correlations of alternate members in the same memo group. Match metadata
// pull-up needs the member-local answer: treating an alias exposed only by an
// alternative member as constant can preserve a value that this expression
// does not actually hold constant.
func GetCorrelatedToOfExpression(
	e RelationalExpression,
) map[values.CorrelationIdentifier]struct{} {
	if e == nil {
		return nil
	}
	if recursiveUnion, ok := e.(*RecursiveUnionExpression); ok {
		// RecursiveUnion satisfies the two temporary-table correlations
		// itself. Java overrides computeCorrelatedTo for this expression
		// instead of using the generic with-children implementation.
		result := make(map[values.CorrelationIdentifier]struct{})
		for _, quantifier := range recursiveUnion.GetQuantifiers() {
			for alias := range quantifier.GetCorrelatedTo() {
				if alias != recursiveUnion.GetTempTableScanAlias() &&
					alias != recursiveUnion.GetTempTableInsertAlias() {
					result[alias] = struct{}{}
				}
			}
		}
		return result
	}

	ownAliases := make(map[values.CorrelationIdentifier]struct{})
	for _, quantifier := range e.GetQuantifiers() {
		ownAliases[quantifier.GetAlias()] = struct{}{}
	}
	result := make(map[values.CorrelationIdentifier]struct{})
	for alias := range e.GetCorrelatedToWithoutChildren() {
		if _, bound := ownAliases[alias]; !bound {
			result[alias] = struct{}{}
		}
	}
	for _, quantifier := range e.GetQuantifiers() {
		for alias := range quantifier.GetCorrelatedTo() {
			_, bound := ownAliases[alias]
			if !e.CanCorrelate() || !bound {
				result[alias] = struct{}{}
			}
		}
	}
	return result
}
