package sqldriver_test

// Shared direct-executor harness helper: the scalar-subquery pre-bind pass.

import (
	"context"

	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/recordlayer/query/executor"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/relational/core/embedded"
)

// prebindScalarSubqueries evaluates each planned scalar subquery against the
// store and returns an EvaluationContext with the results bound — the
// direct-executor analog of the sql driver fetchPage's pre-pass. Executing a
// subquery-bearing plan WITHOUT this fails loudly with
// values.UnboundScalarSubqueryError (never a silent NULL that vanishes rows),
// so every direct-harness test that runs SQL through
// embedded.PlanRecordQueryWithSubqueries + executor.ExecutePlan threads its
// subqueries through here.
func prebindScalarSubqueries(ctx context.Context, store *recordlayer.FDBRecordStore, subs []embedded.PlannedScalarSubquery) (*executor.EvaluationContext, error) {
	evalCtx := executor.EmptyEvaluationContext()
	if len(subs) == 0 {
		return evalCtx, nil
	}
	scalarResults := make(map[values.CorrelationIdentifier]any, len(subs))
	for _, ssq := range subs {
		result, err := executor.EvaluateScalarSubquery(ctx, ssq.Plan, store, evalCtx, recordlayer.DefaultExecuteProperties())
		if err != nil {
			return nil, err
		}
		scalarResults[ssq.Alias] = result
	}
	return evalCtx.WithScalarSubqueries(scalarResults), nil
}
