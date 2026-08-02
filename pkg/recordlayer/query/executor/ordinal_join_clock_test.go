package executor

// The ordinal join's BAKED result-value build evaluates rows through its own
// RowEvalContext (evaluateOrdinalJoinRow / evaluateOrdinalJoinBareRow), not
// through the cursor frontier's clock-aware wrap. These pins hold that path to
// the same statement-stability contract as every other evaluation site: a
// CURRENT_TIMESTAMP-family value folded into the baked build must observe the
// statement clock, never RowEvalContext's wall-clock fallback (which drifts
// across rows). Two layers:
//
//   - the eval primitives honour the clock they are handed;
//   - both cursor constructors (NLJ here, FlatMap alongside) thread the
//     EvaluationContext onto ordinalJoinBuild.Clock.

import (
	"testing"
	"time"

	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

// ojFixedInstant is an instant no wall clock will ever produce again, so a
// wall-clock fallback can never accidentally match the expectation.
var ojFixedInstant = time.Date(2001, 2, 3, 4, 5, 6, 0, time.UTC)

// ojFixedTimestamp is ojFixedInstant in the engine's timestamp string form
// (the CURRENT_TIMESTAMP family evaluates to a formatted string).
const ojFixedTimestamp = "2001-02-03 04:05:06"

func TestOrdinalJoinRow_ClockedFieldObservesStatementClock(t *testing.T) {
	t.Parallel()
	rc := values.NewRecordConstructorValue(
		values.RecordConstructorField{
			Name:  "TS",
			Value: values.NewScalarFunctionValue("CURRENT_TIMESTAMP", values.NullableTimestamp),
		},
	)
	clock := EmptyEvaluationContext().WithStatementTime(ojFixedInstant)
	pos, err := evaluateOrdinalJoinRow(rc, rcOutputType(rc), stubBinder{}, clock)
	if err != nil {
		t.Fatalf("evaluateOrdinalJoinRow: %v", err)
	}
	got, ok := pos.Get(0)
	if !ok || got != ojFixedTimestamp {
		t.Fatalf("baked-build CURRENT_TIMESTAMP = (%v, %v), want (%q, true) — the build's RowEvalContext must carry the statement clock, not fall back to the wall clock (which drifts across rows)", got, ok, ojFixedTimestamp)
	}
}

func TestOrdinalJoinBareRow_ClockedValueObservesStatementClock(t *testing.T) {
	t.Parallel()
	bare := values.NewScalarFunctionValue("CURRENT_TIMESTAMP", values.NullableTimestamp)
	clock := EmptyEvaluationContext().WithStatementTime(ojFixedInstant)
	pos, err := evaluateOrdinalJoinBareRow(bare, stubBinder{}, clock)
	if err != nil {
		t.Fatalf("evaluateOrdinalJoinBareRow: %v", err)
	}
	got, ok := pos.Get(0)
	if !ok || got != ojFixedTimestamp {
		t.Fatalf("bare baked CURRENT_TIMESTAMP = (%v, %v), want (%q, true) — the bare arm's RowEvalContext must carry the statement clock", got, ok, ojFixedTimestamp)
	}
}

// TestOrdinalJoinBuild_ConstructorsThreadClock pins that BOTH cursor
// constructors set ordinalJoinBuild.Clock from their EvaluationContext. The
// eval-primitive pins above cannot catch a constructor that forgets the
// assignment — the primitives would be handed a nil clock and silently fall
// back to the wall clock.
func TestOrdinalJoinBuild_ConstructorsThreadClock(t *testing.T) {
	t.Parallel()
	_, _, _, _, seed := ojWiringLegs(t)
	evalCtx := EmptyEvaluationContext().WithStatementTime(ojFixedInstant)

	t.Run("NLJ", func(t *testing.T) {
		t.Parallel()
		c := mustNLJCursor(t, nil, nil, plans.JoinInner,
			values.NamedCorrelationIdentifier("A"), values.NamedCorrelationIdentifier("B"),
			nil, seed, evalCtx, recordlayer.NewExecuteState(0))
		if c.build == nil || c.build.Clock != values.StatementClock(evalCtx) {
			t.Fatal("newNLJCursor must thread the EvaluationContext onto ordinalJoinBuild.Clock — a clockless build evaluates CURRENT_TIMESTAMP against the drifting wall clock")
		}
	})

	t.Run("FlatMap", func(t *testing.T) {
		t.Parallel()
		fc, err := newFlatMapCursorWithOuterProperties(nil, nil, nil, nil, evalCtx,
			values.NamedCorrelationIdentifier("A"), values.NamedCorrelationIdentifier("B"),
			seed, recordlayer.ExecuteProperties{}, false)
		if err != nil {
			t.Fatalf("newFlatMapCursorWithOuterProperties: %v", err)
		}
		if fc.build == nil || fc.build.Clock != values.StatementClock(evalCtx) {
			t.Fatal("newFlatMapCursorWithOuterProperties must thread the EvaluationContext onto ordinalJoinBuild.Clock — a clockless build evaluates CURRENT_TIMESTAMP against the drifting wall clock")
		}
	})
}
