package executor

import (
	"context"

	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
	"fdb.dev/pkg/relational/api"
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
	props recordlayer.ExecuteProperties,
) (any, error) {
	// props carries the statement's scan limits (RFC-106a) so an uncorrelated
	// subquery respects the same cap as the outer plan. ctx carries the
	// statement timeout.
	cursor, err := ExecutePlan(ctx, plan, store, evalCtx, nil, props)
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
			// An out-of-band (resource-limit) stop means the subquery's input was
			// truncated — error (→ 54F01) rather than fabricate a no-row / wrong
			// scalar value from a partial scan (RFC-106a).
			if lerr := errIfBufferTruncated(result); lerr != nil {
				return nil, lerr
			}
			break
		}
		rows = append(rows, result.GetValue())
	}

	if len(rows) > 1 {
		return nil, api.NewErrorf(api.ErrCodeCardinalityViolation,
			"scalar subquery returned more than one row")
	}
	if len(rows) == 0 {
		return nil, nil
	}

	// Extract the single column value from the row. The ordinal
	// Positional row is authoritative — a scalar subquery projects exactly one
	// column, so its single slot IS the scalar value, read by ordinal (no name
	// lookup).
	row := rows[0]
	if row.Positional == nil {
		return nil, nil
	}
	slots := row.Positional.Slots
	if len(slots) == 0 {
		return nil, nil
	}
	if len(slots) != 1 {
		return nil, api.NewErrorf(api.ErrCodeSyntaxError,
			"scalar subquery must return exactly one column, got %d", len(slots))
	}
	return slots[0], nil
}
