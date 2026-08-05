package embedded

import (
	"fmt"
	"sort"

	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
	"fdb.dev/pkg/relational/api"
)

// A planned query depends on the record store's INDEX STATES, not only on its
// metadata. Java says so explicitly and enforces it at execution rather than at
// planning: `DatabaseObjectDependenciesPredicate.eval`
// (DatabaseObjectDependenciesPredicate.java:87-105) re-reads the live store on
// every execution and invalidates the plan when an index the plan depends on has
// disappeared, has a different `lastModifiedVersion`, or is no longer
// `recordStoreState.isReadable(...)`. It is attached to every planned query as
// the CONTINUATION plan constraint (QueryPlan.java:667,
// computeContinuationPlanConstraint at :726-735), so it gates a resumed
// continuation and not only a first execution.
//
// Metadata version cannot stand in for it: an index-state transition does not
// bump `md.Version()`, so a plan-cache scope keyed on the metadata version
// serves a plan built under one store state to an execution running under
// another.
//
// The dependency is a SET OF INDEXES, and it is deliberately not the store's
// whole index-state snapshot. Two properties follow, and both are Java's:
//
//   - SCOPED. An index the plan does not depend on may change state freely.
//     Signing the whole snapshot instead makes an unrelated index build fail
//     every in-flight statement in the database with a retryable error, which is
//     a self-inflicted outage rather than a correctness measure.
//   - DIRECTIONAL. Only a dependency ceasing to be READABLE invalidates. An
//     index that was excluded at planning and has since BECOME readable leaves
//     the plan correct and merely less optimal, so it must not invalidate.
//     Java is emphatic about this: RecordStoreState.compatibleWith
//     (RecordStoreState.java:242-247) iterates the CURRENT state's index map,
//     which holds only the NON-readable exceptions, and requires each to be
//     non-readable in the planned state too — an index readable now and
//     excluded then is never even examined. Its javadoc names the reason: such
//     operations "will be less efficient with the older state information, but
//     they should not cause correctness problems".
//
// WHAT GOES IN THE SET. Java collects `plan.getUsedIndexes()` — the indexes the
// plan SCANS, gathered by delegation down the plan tree
// (DatabaseObjectDependenciesPredicate.java:183-197). Go collects that, and
// deliberately more: an index whose METADATA PROPERTY licensed a transformation
// is a dependency even when no leaf scans it. A uniqueness proof that elides a
// DISTINCT is the motivating case — the resulting plan reads base records and
// names the index nowhere, so a scan-only set would miss precisely the
// dependency that can produce WRONG ROWS rather than an error. See
// planIndexDependencies.

// planIndexDependencies is a plan's complete index dependency set. The element
// type is predicates.UsedIndex — the SAME type the ported
// DatabaseObjectDependenciesPredicate carries — because a second structurally
// identical type for one concept is how the two drift apart.
//
// The lastModifiedVersion is not redundant with the name. Execution opens the
// store with the CONNECTION's current metadata, which can be newer than the
// metadata the plan was built against, so an index can be dropped and recreated
// under the same name with a different definition between planning and
// execution. Java checks it for that reason
// (DatabaseObjectDependenciesPredicate.java:95-97).
//
// Sorted and deduplicated, so the set is a value with a stable identity.
type planIndexDependencies = []predicates.UsedIndex

// collectPlanIndexDependencies gathers the scanned indexes from a plan tree, the
// scanned indexes of every scalar subquery planned alongside it, and any
// proof-only dependencies the planner recorded.
//
// Scalar subqueries are collected explicitly because Go executes them as
// SEPARATE plans hanging off the same statement rather than as children of the
// main plan, so a walk from the root cannot reach them. Their leaves scan
// indexes exactly as the main plan's do, and an unguarded subquery dependency
// would be a hole in precisely the place a walk-based implementation looks
// complete.
func collectPlanIndexDependencies(
	md *recordlayer.RecordMetaData,
	plan plans.RecordQueryPlan,
	subqueries []PlannedScalarSubquery,
	proofOnly []string,
) planIndexDependencies {
	if md == nil {
		return nil
	}
	names := make(map[string]struct{})
	collectScannedIndexNames(plan, names)
	for _, sub := range subqueries {
		collectScannedIndexNames(sub.Plan, names)
	}
	for _, name := range proofOnly {
		if name != "" {
			names[name] = struct{}{}
		}
	}
	if len(names) == 0 {
		return nil
	}
	ordered := make([]string, 0, len(names))
	for name := range names {
		ordered = append(ordered, name)
	}
	sort.Strings(ordered)

	deps := make(planIndexDependencies, 0, len(ordered))
	for _, name := range ordered {
		// An index the PLANNING metadata does not name cannot be a dependency of
		// a plan built from that metadata; recording a version for it would mean
		// inventing one. This is not the drop case — that is a change between
		// planning and execution, and it is caught at execution by the store's
		// metadata not naming it.
		idx := md.GetIndex(name)
		if idx == nil {
			continue
		}
		deps = append(deps, predicates.UsedIndex{
			Name:                name,
			LastModifiedVersion: idx.LastModifiedVersion,
		})
	}
	if len(deps) == 0 {
		return nil
	}
	return deps
}

// indexNamedPlan is any physical plan node that reads a named index. Asking for
// the CAPABILITY rather than enumerating concrete plan types is what keeps this
// complete as plan types are added: a new index-reading plan that exposes
// GetIndexName is collected without this function being revisited, and one that
// does not expose it cannot be scanned by name in the first place.
type indexNamedPlan interface {
	GetIndexName() string
}

func collectScannedIndexNames(plan plans.RecordQueryPlan, into map[string]struct{}) {
	plans.Walk(plan, func(node plans.RecordQueryPlan) bool {
		if named, ok := node.(indexNamedPlan); ok {
			if name := named.GetIndexName(); name != "" {
				into[name] = struct{}{}
			}
		}
		return true
	})
}

// storeIndexAvailability adapts a live store's metadata and index states to the
// question the predicate asks. Both come from the SAME store open, so they
// cannot describe two different moments.
type storeIndexAvailability struct {
	md     *recordlayer.RecordMetaData
	states map[string]recordlayer.IndexState
}

func (a storeIndexAvailability) IndexLastModifiedVersion(name string) (int, bool) {
	if a.md == nil {
		return 0, false
	}
	idx := a.md.GetIndex(name)
	if idx == nil {
		return 0, false
	}
	return idx.LastModifiedVersion, true
}

// IsIndexReadable is strictly READABLE — Java's isReadable, which excludes
// READABLE_UNIQUE_PENDING even though it is scannable, because a unique-pending
// index has not proven the uniqueness a plan may have assumed from it.
func (a storeIndexAvailability) IsIndexReadable(name string) bool {
	state, present := a.states[name]
	return present && state == recordlayer.IndexStateReadable
}

// validatePlanIndexDependencies evaluates the plan's dependency predicate
// against the store it is about to run on.
//
// SQLSTATE 40001 (serialization failure) rather than a plan error, because the
// condition is a lost race between two transactions and the caller's correct
// response is a retry, which replans against the new state.
func validatePlanIndexDependencies(
	deps planIndexDependencies,
	md *recordlayer.RecordMetaData,
	states map[string]recordlayer.IndexState,
) error {
	if len(deps) == 0 {
		return nil
	}
	avail := storeIndexAvailability{md: md, states: states}
	pred := predicates.NewDatabaseObjectDependenciesPredicate(deps)
	holds, err := pred.Eval(avail)
	if err != nil {
		return err
	}
	if holds == predicates.TriTrue {
		return nil
	}
	// The truth value is the decision; the diagnostic is a separate question,
	// because a predicate that returned prose instead of a TriBool would not be
	// a predicate.
	name, reason, found := pred.UnavailableDependency(avail)
	if !found {
		name, reason = "(unknown)", "it is no longer available"
	}
	return api.NewError(api.ErrCodeSerializationFailure, fmt.Sprintf(
		"query plan is stale: it depends on index %s and %s; replan the statement",
		name, reason))
}
