package plans

import "fdb.dev/pkg/recordlayer/query/plan/cascades/properties"

// Compile-time proof that every physical plan answers the cost and ordering
// questions the memo asks (RFC-183 P5).
//
// What these assertions do NOT do is guard a runtime dispatch — though NOT for
// the reason an earlier version of this comment gave.
//
// The dispatch does not gate on cascades.physicalPlanExpression. It gates on
// the hint interfaces themselves: `e.(CostHinter)` (properties/cost.go:645)
// and `e.(OrderingHinter)` (properties/ordering.go:145). Plans DO satisfy
// those — that is what the assertions below assert — so the old claim that a
// plan "would simply never be asked" because it lacks GetRecordQueryPlan was
// wrong about the mechanism.
//
// The bodies are nevertheless unreachable today, for a different reason:
// nothing ever presents a BARE plan as the expression being costed or ordered.
// Every such receiver is still a physical wrapper. Measured, not assumed —
// instrumenting both dispatch sites to count `*plans.*` receivers over the
// full 2407-query corpus yields ZERO on each. A plan that failed to implement
// CostHinter would therefore still not reach the pessimistic default arm; it
// would just never be consulted.
//
// This distinction matters for the deletion: the day a bare plan is yielded in
// place of a wrapper, these bodies go live all at once, and it will be the
// hint interfaces that route to them — not any wrapper-shaped gate.
//
// What these assertions ARE is a COMPLETENESS check on the plan-side bodies,
// which are staging for the wrapper deletion RFC-183 §11 defers. The bodies
// are unreachable today (see ordering.go's header), so nothing at runtime
// would notice a newly added plan type that answers no hint — it would go
// missing silently and surface only when the deletion flips the caller over.
// A build break here is the substitute for the runtime signal that does not
// exist yet.

// CostedPlan is the pair of questions the cost model asks every physical plan:
// what does it COST, and what does it PROVE about its own row count. They are
// one interface deliberately (RFC-195). The six impossible cost estimates that
// RFC exists to correct each accumulated because a plan could answer the first
// without the second — "this operator structurally guarantees a row" lived in a
// different file from "this operator costs in*0.7", and nothing required the
// two to agree, or even to both exist.
//
// Registering a plan as one pair rather than two independent assertions means a
// new operator cannot gain a cost formula while silently proving nothing: the
// single line that registers it demands both, at compile time.
type CostedPlan interface {
	properties.CostHinter
	properties.CardinalityProver
}

// CostedPlanPrototypes is one typed-nil instance of every plan that answers the
// CostedPlan contract. It serves two jobs at once: the slice's element type is
// the compile-time completeness check (a plan missing either method will not go
// in), and the slice itself is what the derivation-agreement and
// logical/physical parity tests ENUMERATE.
//
// Enumeration is what keeps those tests from passing vacuously. A hand-written
// pairing table checked only against itself stops testing anything the day
// operator 38 is added; driven off this list, an operator with no table entry
// fails with a message naming it, so an unpaired arm gets an explicit listed
// reason rather than silence.
//
// Typed nils are safe here: every ProvenCardinalities implementation is
// nil-receiver tolerant, which the enumeration tests exercise directly.
var CostedPlanPrototypes = []CostedPlan{
	(*RecordQueryScanPlan)(nil),
	(*RecordQueryIndexPlan)(nil),
	(*RecordQueryVectorIndexPlan)(nil),
	(*RecordQueryAggregateIndexPlan)(nil),
	(*RecordQueryValuesPlan)(nil),
	(*RecordQueryExplodePlan)(nil),
	(*RecordQueryTempTableScanPlan)(nil),
	(*RecordQueryTableFunctionPlan)(nil),
	(*RecordQueryFilterPlan)(nil),
	(*RecordQueryPredicatesFilterPlan)(nil),
	(*RecordQueryTypeFilterPlan)(nil),
	(*RecordQueryFetchFromPartialRecordPlan)(nil),
	(*RecordQueryFirstOrDefaultPlan)(nil),
	(*RecordQueryInMemorySortPlan)(nil),
	(*RecordQueryDistinctPlan)(nil),
	(*RecordQueryUnorderedPrimaryKeyDistinctPlan)(nil),
	(*RecordQueryMapPlan)(nil),
	(*RecordQueryProjectionPlan)(nil),
	(*RecordQueryDefaultOnEmptyPlan)(nil),
	(*RecordQueryTempTableInsertPlan)(nil),
	(*RecordQueryLimitPlan)(nil),
	(*RecordQueryStreamingAggregationPlan)(nil),
	(*RecordQueryInsertPlan)(nil),
	(*RecordQueryDeletePlan)(nil),
	(*RecordQueryUpdatePlan)(nil),
	(*RecordQueryUnionPlan)(nil),
	(*RecordQueryUnorderedUnionPlan)(nil),
	(*RecordQueryMergeSortUnionPlan)(nil),
	(*RecordQueryIntersectionPlan)(nil),
	(*RecordQueryMultiIntersectionOnValuesPlan)(nil),
	(*RecordQueryFlatMapPlan)(nil),
	(*RecordQueryNestedLoopJoinPlan)(nil),
	(*RecordQueryInJoinPlan)(nil),
	(*RecordQueryInUnionPlan)(nil),
	(*RecordQueryRecursiveDfsJoinPlan)(nil),
	(*RecordQueryRecursiveLevelUnionPlan)(nil),
}

var (
	_ properties.OrderingHinter = (*RecordQueryFilterPlan)(nil)
	_ properties.OrderingHinter = (*RecordQueryPredicatesFilterPlan)(nil)
	_ properties.OrderingHinter = (*RecordQueryTypeFilterPlan)(nil)
	_ properties.OrderingHinter = (*RecordQueryDistinctPlan)(nil)
	_ properties.OrderingHinter = (*RecordQueryUnorderedPrimaryKeyDistinctPlan)(nil)
	_ properties.OrderingHinter = (*RecordQueryProjectionPlan)(nil)
	_ properties.OrderingHinter = (*RecordQueryMapPlan)(nil)
	_ properties.OrderingHinter = (*RecordQueryLimitPlan)(nil)
	_ properties.OrderingHinter = (*RecordQueryDefaultOnEmptyPlan)(nil)
	_ properties.OrderingHinter = (*RecordQueryFetchFromPartialRecordPlan)(nil)
	_ properties.OrderingHinter = (*RecordQueryScanPlan)(nil)
	_ properties.OrderingHinter = (*RecordQueryIndexPlan)(nil)
	_ properties.OrderingHinter = (*RecordQueryInMemorySortPlan)(nil)
	_ properties.OrderingHinter = (*RecordQueryMergeSortUnionPlan)(nil)
	_ properties.OrderingHinter = (*RecordQueryIntersectionPlan)(nil)
	_ properties.OrderingHinter = (*RecordQueryInUnionPlan)(nil)
	_ properties.OrderingHinter = (*RecordQueryMultiIntersectionOnValuesPlan)(nil)
	_ properties.OrderingHinter = (*RecordQueryStreamingAggregationPlan)(nil)
	_ properties.OrderingHinter = (*RecordQueryAggregateIndexPlan)(nil)
	_ properties.OrderingHinter = (*RecordQueryInJoinPlan)(nil)
	_ properties.OrderingHinter = (*RecordQueryUnorderedUnionPlan)(nil)
	_ properties.OrderingHinter = (*RecordQueryVectorIndexPlan)(nil)
	_ properties.OrderingHinter = (*RecordQueryExplodePlan)(nil)
	_ properties.OrderingHinter = (*RecordQueryFlatMapPlan)(nil)
	_ properties.OrderingHinter = (*RecordQueryNestedLoopJoinPlan)(nil)
	_ properties.OrderingHinter = (*RecordQueryRecursiveDfsJoinPlan)(nil)
	_ properties.OrderingHinter = (*RecordQueryRecursiveLevelUnionPlan)(nil)
	_ properties.OrderingHinter = (*RecordQueryTableFunctionPlan)(nil)
	_ properties.OrderingHinter = (*RecordQueryTempTableInsertPlan)(nil)
	_ properties.OrderingHinter = (*RecordQueryTempTableScanPlan)(nil)
)

// Rich orderings are answered only by the plans whose ordering actually
// carries bindings (the scans) plus the delegator that inherits one. Every
// other plan is served by the caller's synthesize-from-HintOrdering fallback,
// so this list is deliberately short rather than exhaustive.
var (
	_ properties.RichOrderingHinter = (*RecordQueryScanPlan)(nil)
	_ properties.RichOrderingHinter = (*RecordQueryIndexPlan)(nil)
	_ properties.RichOrderingHinter = (*RecordQueryVectorIndexPlan)(nil)
	_ properties.RichOrderingHinter = (*RecordQueryFetchFromPartialRecordPlan)(nil)
)
