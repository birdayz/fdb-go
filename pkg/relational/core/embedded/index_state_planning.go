package embedded

import (
	"encoding/base64"
	"encoding/binary"
	"sort"

	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/relational/api"
)

// A planned query depends on the record store's INDEX STATES, not only on its
// metadata. Java says so explicitly and enforces it at execution rather than at
// planning: `DatabaseObjectDependenciesPredicate.eval`
// (DatabaseObjectDependenciesPredicate.java:87-105) re-reads the live store on
// every execution and returns false — invalidating the plan — when an index the
// plan depends on has disappeared, has a different `lastModifiedVersion`, or is
// no longer `recordStoreState.isReadable(...)`. It is attached to every planned
// query as the CONTINUATION plan constraint
// (QueryPlan.java:667, computeContinuationPlanConstraint at :726-735), so the
// check also gates a resumed continuation, not just a first execution.
//
// Go has no equivalent, and metadata version alone cannot stand in: an index
// state transition does not bump `md.Version()`, so a plan-cache scope keyed on
// the metadata version serves a plan built under one store state to an
// execution running under another.
//
// DIVERGENCE FROM JAVA, deliberate and stated: Java scopes the dependency to
// the indexes the plan actually SCANS (`plan.getUsedIndexes()`,
// DatabaseObjectDependenciesPredicate.java:183-197). Go cannot use that
// scoping, because a Go plan can depend on an index it never reads — a
// uniqueness proof ELIDES an operator and then scans something else, leaving no
// leaf naming the index the proof rested on. The whole-snapshot signature below
// is the conservative superset: it can invalidate a plan that did not care,
// never miss one that did. Narrowing it to a recorded per-plan dependency set
// is the correct long-term shape and requires the planner to publish what each
// proof consumed.

// indexStatePlanningSignature identifies the store's index-state snapshot for
// plan-validity purposes. Only strictly READABLE is "unchanged" — Java's
// `isReadable`, exact equality with READABLE, which excludes
// READABLE_UNIQUE_PENDING even though it is scannable — so the sorted set of
// NON-readable index names is the minimal complete identity. Names are
// length-prefixed before encoding, so no name boundary can be forged.
//
// The snapshot it reads is in the METADATA domain — every index the metadata
// names, absent-state defaulted to READABLE — and that domain is part of the
// contract, not an implementation detail. The same function must produce this
// signature on both sides of the comparison, from the same domain; a planning
// side that could see a state key for an index the metadata does not name would
// disagree with the execution side forever. See fetchIndexStateSnapshot.
//
// nil returns "", which disables validation, and covers the two cases where
// there is nothing to validate: offline plan harnesses, and a schema with no
// indexes at all. A live snapshot over a non-empty index set returns a
// non-empty versioned signature even when every index is readable, so a live
// plan can never be mistaken for an offline one.
func indexStatePlanningSignature(states map[string]recordlayer.IndexState) string {
	if states == nil {
		return ""
	}
	nonReadable := make([]string, 0, len(states))
	for name, state := range states {
		if state != recordlayer.IndexStateReadable {
			nonReadable = append(nonReadable, name)
		}
	}
	sort.Strings(nonReadable)
	// Version byte makes the encoding evolvable. An unsigned 64-bit length
	// avoids architecture-dependent encodings and is injective for every Go
	// string that could name an index.
	payload := make([]byte, 1, 1+len(nonReadable)*8)
	payload[0] = 1
	var length [8]byte
	for _, name := range nonReadable {
		binary.BigEndian.PutUint64(length[:], uint64(len(name)))
		payload = append(payload, length[:]...)
		payload = append(payload, name...)
	}
	return base64.RawURLEncoding.EncodeToString(payload)
}

// validatePlanningIndexStateSignature is the Go spelling of
// DatabaseObjectDependenciesPredicate.eval's false return: the plan is no
// longer valid under the store it is about to run against.
//
// SQLSTATE 40001 (serialization failure) rather than a plan error, because the
// condition is a lost race between two transactions and the caller's correct
// response is to retry, which replans against the new state.
func validatePlanningIndexStateSignature(
	expected string,
	states map[string]recordlayer.IndexState,
) error {
	if expected == "" || indexStatePlanningSignature(states) == expected {
		return nil
	}
	return api.NewError(api.ErrCodeSerializationFailure,
		"query plan is stale because index states changed; replan the statement")
}
