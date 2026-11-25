package executor

// Go-only extension tests: LIMIT clause optimization.
// Java uses ExecuteProperties.setReturnedRowLimit() at the JDBC layer;
// Go supports LIMIT natively in SQL with Cascades-integrated optimization.

import (
	"errors"
	"fmt"
	"math"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

// --- Limit pushdown propagation tests ---

func TestExecuteLimit_PropagatesRowLimit(t *testing.T) {
	t.Parallel()

	// Create a limit plan with limit=5, offset=2.
	innerType := exactTestRowType(values.Field{Name: "ID", FieldType: values.NotNullLong})
	innerPlan := mustExecutorConstruct(plans.NewRecordQueryScanPlan(nil, innerType, false))
	limitPlan := mustExecutorConstruct(plans.NewRecordQueryLimitPlan(innerPlan, 5, 2))

	// The effective limit for the inner should be 5+2=7.
	// We can't easily test the propagation without FDB, but we can
	// verify the plan structure is correct.
	if limitPlan.GetLimit() != 5 {
		t.Fatalf("limit = %d, want 5", limitPlan.GetLimit())
	}
	if limitPlan.GetOffset() != 2 {
		t.Fatalf("offset = %d, want 2", limitPlan.GetOffset())
	}
	children := limitPlan.GetChildren()
	if len(children) != 1 {
		t.Fatalf("expected 1 child, got %d", len(children))
	}
}

func TestLimitPlan_RejectsNegativeOffset(t *testing.T) {
	t.Parallel()
	for _, offset := range []int64{-1, math.MinInt64} {
		t.Run(fmt.Sprint(offset), func(t *testing.T) {
			t.Parallel()
			rowType := exactTestRowType(values.Field{Name: "ID", FieldType: values.NotNullLong})
			inner := mustExecutorConstruct(plans.NewRecordQueryScanPlan(nil, rowType, false))
			var offsetErr *expressions.InvalidLimitOffsetError
			if _, err := plans.NewRecordQueryLimitPlan(inner, 5, offset); !errors.As(err, &offsetErr) || offsetErr.Offset != offset {
				t.Fatalf("static plan offset %d: got %v, want structured offset error", offset, err)
			}
			cap := &values.ConstantValue{Value: int64(5), Typ: values.NotNullLong}
			if _, err := plans.NewRecordQueryLimitPlanWithValue(inner, cap, offset); !errors.As(err, &offsetErr) || offsetErr.Offset != offset {
				t.Fatalf("runtime plan offset %d: got %v, want structured offset error", offset, err)
			}
			if _, err := plans.NewRecordQueryLimitPlanFromQuantifier(plans.QuantifierOverPlan(inner), 5, offset, nil); !errors.As(err, &offsetErr) || offsetErr.Offset != offset {
				t.Fatalf("quantifier plan offset %d: got %v, want structured offset error", offset, err)
			}
		})
	}
}

func TestLimitChildRowLimit(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name                             string
		parent, offset, limit, wantChild int
	}{
		{name: "finite", offset: 2, limit: 5, wantChild: 7},
		{name: "parent_smaller", parent: 1, offset: 2, limit: 5, wantChild: 3},
		{name: "parent_larger", parent: 10, offset: 2, limit: 5, wantChild: 7},
		{name: "unbounded", offset: 2, limit: -1, wantChild: 0},
		{name: "negative_parent_unbounded", parent: -1, offset: 2, limit: -1, wantChild: -1},
		{name: "unbounded_with_parent", parent: 3, offset: 2, limit: -1, wantChild: 5},
		{name: "zero", limit: 0, wantChild: 0},
		{name: "zero_with_offset", parent: 1, offset: 2, limit: 0, wantChild: 2},
		{name: "maximum", offset: 1, limit: math.MaxInt - 1, wantChild: math.MaxInt},
		{name: "overflow", offset: 1, limit: math.MaxInt, wantChild: math.MaxInt},
		{name: "both_maximum", offset: math.MaxInt, limit: math.MaxInt, wantChild: math.MaxInt},
		{name: "parent_avoids_overflow", parent: 1, offset: 2, limit: math.MaxInt, wantChild: 3},
		{name: "parent_overflows", parent: 1, offset: math.MaxInt, limit: -1, wantChild: math.MaxInt},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := limitChildRowLimit(tc.parent, tc.offset, tc.limit); got != tc.wantChild {
				t.Fatalf("child read budget = %d, want %d", got, tc.wantChild)
			}
		})
	}
}

func TestExecuteLimit_ZeroLimit(t *testing.T) {
	t.Parallel()

	// A LIMIT 0 plan should have limit=0.
	innerType := exactTestRowType(values.Field{Name: "ID", FieldType: values.NotNullLong})
	innerPlan := mustExecutorConstruct(plans.NewRecordQueryScanPlan(nil, innerType, false))
	limitPlan := mustExecutorConstruct(plans.NewRecordQueryLimitPlan(innerPlan, 0, 0))

	if limitPlan.GetLimit() != 0 {
		t.Fatalf("expected limit=0, got %d", limitPlan.GetLimit())
	}
	// LIMIT 0 lowers to RecordQueryLimitPlan(limit=0); at the executor level the
	// limitEnvelopeCursor short-circuits remLimit==0 to an empty, exhausted result.
}
