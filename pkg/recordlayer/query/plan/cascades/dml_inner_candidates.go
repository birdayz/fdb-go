package cascades

import (
	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/properties"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

// dmlInnerCandidate is one physical access path a DML implementation rule may
// build its mutation over, paired with the two partition properties Java's
// ImplementUpdateRule / ImplementDeleteRule consult.
type dmlInnerCandidate struct {
	expr expressions.RelationalExpression
	// source is the reference expr was drawn from. The mutation must range over
	// a reference RESTRICTED to expr, and the restriction is minted from source
	// (Java's memoizeMemberPlansFromOther takes the same two arguments,
	// ImplementDeleteRule.java:78).
	source *expressions.Reference
	// distinctRecords is DistinctRecordsProperty.distinctRecords() for this
	// access path — the ONLY thing that lets ImplementDeleteRule skip the
	// primary-key dedup (ImplementDeleteRule.java:79-82). ImplementUpdateRule
	// does not consult it and neither does the update rule here.
	distinctRecords bool
}

// storedRecordDMLCandidates is the Go form of the reference matcher both DML
// implementation rules bind:
//
//	planPartitions(filterPlanPartitions(
//	        planPartition -> planPartition.getPartitionPropertyValue(
//	                StoredRecordProperty.storedRecord()),
//	        any(anyPlanPartition())))
//
// (ImplementUpdateRule.java:54-57, ImplementDeleteRule.java:55-57.)
//
// A DML plan mutates the STORED RECORD its inner presents, so an access path
// that does not flow stored records — an aggregate index scan, an explode, a
// temp-table scan — has nothing for the mutation to write back. Java expresses
// that as a partition filter on the matcher, which means a reference offering
// only non-stored partitions never binds and the rule never fires; the query
// then fails to plan rather than planning a mutation over rows that are not
// records. Declining here has exactly that effect.
//
// The enumeration is per MEMBER rather than per partition, which is the same
// choice ImplementFilterRule makes and for the same reason: Java fires an
// implementation rule once per (parent, child-member) pair, so every
// alternative the child offers is lifted into a parent alternative and COST
// decides among the resulting DML plans. Picking one member here would leave a
// non-winning access path invisible to cost rather than out-priced. The
// partition properties are per-plan values, so reading them per member is the
// same reading Java's partition gives — a partition is a grouping of members
// that share them, not a separate authority.
//
// The property source is the reference's PlanPropertiesMap when PLANNING has
// computed one, and computeWrapperProperties directly otherwise. Both are the
// same function; the map is its cache. Falling back rather than declining on a
// missing map matters because the child-delegating property arms read the map
// of the CHILD reference and answer false without one — conservative in both
// directions here (a false storedRecord declines the plan loudly, a false
// distinctRecords keeps a dedup that was not needed).
func storedRecordDMLCandidates(ref *expressions.Reference) []dmlInnerCandidate {
	if ref == nil {
		return nil
	}
	pm := GetRefPlanPropertiesMap(ref)
	var out []dmlInnerCandidate
	for _, m := range physicalMembersForParentEnumeration(ref) {
		var props properties.PropertyMap
		if pm != nil {
			props = pm.GetProperties(m)
		}
		if props == nil {
			ph, ok := m.(physicalPlanExpression)
			if !ok {
				continue
			}
			props = computeWrapperProperties(ph)
		}
		if !props.GetBool(properties.PropStoredRecord) {
			continue
		}
		out = append(out, dmlInnerCandidate{
			expr:            m,
			source:          ref,
			distinctRecords: props.GetBool(properties.PropDistinctRecords),
		})
	}
	return out
}

// dmlDedupedInnerQuantifier returns the quantifier a DML plan should range
// over: the candidate's access path with a primary-key dedup interposed, or the
// access path directly when the caller's rule is licensed to skip it.
//
// The dedup is Java's memoizePlan(new RecordQueryUnorderedPrimaryKeyDistinctPlan(
// Quantifier.physical(planPartitionReference))) — ImplementUpdateRule.java:79-80
// and ImplementDeleteRule.java:79-82. It guarantees the mutation sees each
// stored record AT MOST ONCE, which is a property the mutation needs from the
// PLAN and cannot get from the executor:
//
//   - Java's DML executor is streaming and pipelined
//     (RecordQueryAbstractDataModificationPlan.executePlan:195-209 maps the
//     inner results through mutateRecord and mapPipelined's saveRecordAsync),
//     so a record whose index entry MOVES under the cursor because the
//     statement rewrote a key column becomes visible to the ongoing scan
//     through read-your-writes and is revisited. The dedup drops the revisit.
//     That is the Halloween problem, and Go's executor closes it a second and
//     stronger way by pre-materializing the whole target set before the first
//     write (executor.go executeUpdate / executeDelete).
//   - Independently of any write, an access path can present ONE stored record
//     on SEVERAL of its own iterations — a fan-out index entry per repeated
//     element, an IN-join value list holding a repeated value, an unordered
//     union of overlapping legs. Pre-materialization faithfully buffers every
//     copy, so nothing downstream removes them. Only this dedup does, which is
//     why it belongs in the plan even though Go's executor is not pipelined.
//
// Java's memoizePlan is a NEW single-member final reference, not an interning
// memo lookup, so MemoizeFinalExpression is the twin: the dedup is a
// compensating operator this rule just built and must not be deduped against an
// unrelated group.
//
// The ACCESS PATH underneath is restricted the same way, for a reason that is a
// correctness one rather than an enumeration one. The dedup decision is read
// per candidate, so the reference the mutation ranges over must offer exactly
// that candidate. Interning hands back the group CONTAINING the candidate — the
// whole child group — and then a candidate that reported DistinctRecords, and
// therefore skipped the dedup, produces a DELETE ranging over a group that also
// holds NON-distinct members. Extraction may resolve it to one of those, and
// the mutation sees a stored record more than once with nothing in the plan to
// drop the repeat: the Halloween dedup bypassed. Cost makes that outcome more
// likely, not less, because the undeduped plan is the cheaper one.
//
// Java's property reading and its reference always describe the same set of
// plans — both come from one PlanPartition (ImplementDeleteRule.java:78-82) —
// so the two cannot drift apart there.
func dmlDedupedInnerQuantifier(
	call *ExpressionRuleCall, candidate dmlInnerCandidate, alreadyDistinct bool,
) expressions.Quantifier {
	innerQ := expressions.ForEachQuantifier(call.MemoizeMemberPlansFromOther(
		candidate.source, []expressions.RelationalExpression{candidate.expr}))
	if alreadyDistinct {
		return innerQ
	}
	dedup := plans.NewRecordQueryUnorderedPrimaryKeyDistinctPlanFromQuantifier(innerQ)
	return expressions.ForEachQuantifier(call.MemoizeFinalExpression(dedup))
}
