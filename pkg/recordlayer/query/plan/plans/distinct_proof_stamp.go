package plans

// A DISTINCT that a secondary UNIQUE index proved redundant leaves a plan that
// names that index NOWHERE — the whole point of the elision is that the winning
// access path is the base-record scan, not the index. The index is nonetheless a
// dependency of the plan: if it stops being strictly READABLE between planning
// and execution, the elided operator was elided on a proof that no longer holds,
// and the statement returns DUPLICATE ROWS rather than an error.
//
// So the proof's identity is recorded ON the plan, as a stamp. The alternative —
// a side-channel list threaded out of the fresh-planning path — is dropped on
// every plan-cache hit, because a cache hit derives dependencies from the cached
// plan alone (that invariant is what makes the cache transparent). It is also
// absent on the metadata-only planning harness, which bypasses the cache
// entirely and is where the unit-level pins are written. A stamp is present on
// every path that produces a plan, and it rides the continuation a resumed page
// re-executes.
//
// The stamp is asked for as a CAPABILITY rather than enumerated as a set of
// concrete plan types, the way indexNamedPlan is on the dependency-collecting
// side: a plan type that gains the ability to carry a proof is collected without
// the walk being revisited, and a plan type that cannot carry one cannot be
// handed one — the eliding rule declines to elide rather than dropping the
// dependency on the floor.

// DistinctProofStamped is any physical plan that can report the secondary
// UNIQUE index whose uniqueness licensed eliding a DISTINCT above it. The empty
// string means the plan carries no such proof, which is the overwhelmingly
// common case: the elision is licensed by the distinct-records property or by
// primary-key coverage on every other shape, and neither stamps.
type DistinctProofStamped interface {
	GetDistinctProofIndexName() string
}

// DistinctProofStampable is DistinctProofStamped plus the struct-copy setter
// the eliding rule uses, in the shape WithStrictlySorted already established
// for stamping a proved planning fact onto a plan node.
//
// The stamp is IDENTITY-BEARING — every carrier folds it into structuralKey.
// The tempting analogy is RecordQueryProjectionPlan.aliasMinted, which is
// deliberately excluded, and the analogy fails in the direction that matters:
// aliasMinted is a display tag, so two plans differing only in it compute
// identical rows and splitting the memo group would buy nothing. This stamp is a
// PROVED FACT THAT LICENSED THE PLAN'S SHAPE — one of the two plans is correct
// only while an index stays READABLE and the other is correct unconditionally,
// so they are not interchangeable. If the stamp did not split the group, the
// eliding rule's stamped copy would collapse into the unstamped original already
// in the memo, the survivor could be the unstamped one, and the dependency would
// vanish silently — which is exactly the unguarded elision the stamp exists to
// prevent.
//
// The cost is stated rather than hidden: the memo can hold a stamped and an
// unstamped member for the same physical work, costing identically. One extra
// group member on one query shape against a wrong-answer bug is not a close
// trade.
type DistinctProofStampable interface {
	RecordQueryPlan
	DistinctProofStamped
	// WithDistinctProofIndexName returns a shallow copy carrying the proof.
	WithDistinctProofIndexName(indexName string) RecordQueryPlan
}

// explainDistinctProofSuffix renders the stamp for EXPLAIN. The optimization's
// evidence is otherwise the ABSENCE of an operator, and an absence is a weak
// acceptance assertion — a rule that silently died produces one too. Rendering
// the licensing index lets a test assert positively that the rule fired AND
// which index licensed it.
func explainDistinctProofSuffix(indexName string) string {
	if indexName == "" {
		return ""
	}
	return " distinct-by:" + indexName
}
