package executor

import (
	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

// newAggregateCursor is the terse legacy fixture used by direct cursor tests.
// Production enters through newAggregateCursorWithOutputType so it must state
// the exact input QOV and output record type selected by the plan.
func newAggregateCursor(
	inner recordlayer.RecordCursor[QueryResult],
	groupingKeys []values.Value,
	aggregates []expressions.AggregateSpec,
	innerPlan plans.RecordQueryPlan,
	evalCtx *EvaluationContext,
) *aggregateCursor {
	return newAggregateCursorWithOutputType(inner, groupingKeys, aggregates, innerPlan, evalCtx, nil, nil)
}
