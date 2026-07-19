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

var (
	_ properties.CostHinter = (*RecordQueryScanPlan)(nil)
	_ properties.CostHinter = (*RecordQueryIndexPlan)(nil)
	_ properties.CostHinter = (*RecordQueryVectorIndexPlan)(nil)
	_ properties.CostHinter = (*RecordQueryAggregateIndexPlan)(nil)
	_ properties.CostHinter = (*RecordQueryValuesPlan)(nil)
	_ properties.CostHinter = (*RecordQueryExplodePlan)(nil)
	_ properties.CostHinter = (*RecordQueryTempTableScanPlan)(nil)
	_ properties.CostHinter = (*RecordQueryTableFunctionPlan)(nil)
	_ properties.CostHinter = (*RecordQueryFilterPlan)(nil)
	_ properties.CostHinter = (*RecordQueryPredicatesFilterPlan)(nil)
	_ properties.CostHinter = (*RecordQueryTypeFilterPlan)(nil)
	_ properties.CostHinter = (*RecordQueryFetchFromPartialRecordPlan)(nil)
	_ properties.CostHinter = (*RecordQueryFirstOrDefaultPlan)(nil)
	_ properties.CostHinter = (*RecordQueryInMemorySortPlan)(nil)
	_ properties.CostHinter = (*RecordQueryDistinctPlan)(nil)
	_ properties.CostHinter = (*RecordQueryMapPlan)(nil)
	_ properties.CostHinter = (*RecordQueryProjectionPlan)(nil)
	_ properties.CostHinter = (*RecordQueryDefaultOnEmptyPlan)(nil)
	_ properties.CostHinter = (*RecordQueryTempTableInsertPlan)(nil)
	_ properties.CostHinter = (*RecordQueryLimitPlan)(nil)
	_ properties.CostHinter = (*RecordQueryStreamingAggregationPlan)(nil)
	_ properties.CostHinter = (*RecordQueryInsertPlan)(nil)
	_ properties.CostHinter = (*RecordQueryDeletePlan)(nil)
	_ properties.CostHinter = (*RecordQueryUpdatePlan)(nil)
	_ properties.CostHinter = (*RecordQueryUnionPlan)(nil)
	_ properties.CostHinter = (*RecordQueryUnorderedUnionPlan)(nil)
	_ properties.CostHinter = (*RecordQueryMergeSortUnionPlan)(nil)
	_ properties.CostHinter = (*RecordQueryIntersectionPlan)(nil)
	_ properties.CostHinter = (*RecordQueryMultiIntersectionOnValuesPlan)(nil)
	_ properties.CostHinter = (*RecordQueryFlatMapPlan)(nil)
	_ properties.CostHinter = (*RecordQueryNestedLoopJoinPlan)(nil)
	_ properties.CostHinter = (*RecordQueryInJoinPlan)(nil)
	_ properties.CostHinter = (*RecordQueryInUnionPlan)(nil)
	_ properties.CostHinter = (*RecordQueryRecursiveDfsJoinPlan)(nil)
	_ properties.CostHinter = (*RecordQueryRecursiveLevelUnionPlan)(nil)
)

var (
	_ properties.OrderingHinter = (*RecordQueryFilterPlan)(nil)
	_ properties.OrderingHinter = (*RecordQueryPredicatesFilterPlan)(nil)
	_ properties.OrderingHinter = (*RecordQueryTypeFilterPlan)(nil)
	_ properties.OrderingHinter = (*RecordQueryDistinctPlan)(nil)
	_ properties.OrderingHinter = (*RecordQueryProjectionPlan)(nil)
	_ properties.OrderingHinter = (*RecordQueryMapPlan)(nil)
	_ properties.OrderingHinter = (*RecordQueryLimitPlan)(nil)
	_ properties.OrderingHinter = (*RecordQueryDefaultOnEmptyPlan)(nil)
	_ properties.OrderingHinter = (*RecordQueryFetchFromPartialRecordPlan)(nil)
	_ properties.OrderingHinter = (*RecordQueryScanPlan)(nil)
	_ properties.OrderingHinter = (*RecordQueryIndexPlan)(nil)
	_ properties.OrderingHinter = (*RecordQueryInMemorySortPlan)(nil)
	_ properties.OrderingHinter = (*RecordQueryMergeSortUnionPlan)(nil)
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
