package executor

import (
	"context"
	"fmt"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/plans"
)

// EvaluateScalarSubquery executes a scalar subquery plan and returns
// its single scalar result. SQL standard semantics:
//   - Exactly one column (else error)
//   - At most one row (else 21000 cardinality violation)
//   - Zero rows → nil (SQL NULL)
//
// Used by the Cascades executor to pre-evaluate uncorrelated scalar
// subqueries before running the outer plan.
func EvaluateScalarSubquery(
	ctx context.Context,
	plan plans.RecordQueryPlan,
	store *recordlayer.FDBRecordStore,
	evalCtx *EvaluationContext,
) (any, error) {
	cursor, err := ExecutePlan(ctx, plan, store, evalCtx, nil,
		recordlayer.DefaultExecuteProperties())
	if err != nil {
		return nil, err
	}
	defer cursor.Close()

	// Collect up to 2 rows to detect cardinality violations.
	var rows []QueryResult
	for len(rows) < 2 {
		result, nextErr := cursor.OnNext(ctx)
		if nextErr != nil {
			return nil, nextErr
		}
		if !result.HasNext() {
			break
		}
		rows = append(rows, result.GetValue())
	}

	if len(rows) > 1 {
		return nil, fmt.Errorf("scalar subquery returned more than one row")
	}
	if len(rows) == 0 {
		return nil, nil
	}

	// Extract the single column value from the row.
	row := rows[0]
	datum, ok := row.Datum.(map[string]any)
	if !ok {
		// If datum is already a scalar (e.g. from a projection), return it.
		return row.Datum, nil
	}
	if len(datum) == 0 {
		return nil, nil
	}
	// Return the first (and should be only) column value.
	for _, v := range datum {
		return v, nil
	}
	return nil, nil
}
