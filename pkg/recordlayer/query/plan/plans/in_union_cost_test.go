package plans

import (
	"math"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/properties"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

func TestInUnionHintCost_UsesValueCombinationCount(t *testing.T) {
	t.Parallel()

	child := properties.Cost{Cardinality: 20, CPU: 5}
	tests := []struct {
		name     string
		bindings []string
		sources  [][]any
		fanout   float64
	}{
		{
			name:     "one dimension",
			bindings: []string{"a"},
			sources:  [][]any{{1, 2, 3, 4}},
			fanout:   4,
		},
		{
			name:     "Cartesian product",
			bindings: []string{"a", "b"},
			sources:  [][]any{{1, 2}, {"x", "y", "z"}},
			fanout:   6,
		},
		{
			name:     "known empty",
			bindings: []string{"a"},
			sources:  [][]any{{}},
			fanout:   0,
		},
		{
			name:     "unknown then known empty",
			bindings: []string{"a", "b"},
			sources:  [][]any{nil, {}},
			fanout:   0,
		},
		{
			name:     "known empty then unknown",
			bindings: []string{"a", "b"},
			sources:  [][]any{{}, nil},
			fanout:   0,
		},
		{
			name:     "unknown dimension",
			bindings: []string{"a"},
			sources:  [][]any{nil},
			fanout:   10,
		},
		{
			name:     "known times unknown",
			bindings: []string{"a", "b"},
			sources:  [][]any{{1, 2}, nil},
			fanout:   20,
		},
		{
			name:     "single combination bypass",
			bindings: []string{"a"},
			sources:  [][]any{{1}},
			fanout:   1,
		},
		{
			name:     "absent outer sources with binding bypass",
			bindings: []string{"a"},
			sources:  nil,
			fanout:   1,
		},
		{
			name:     "empty outer sources with binding bypass",
			bindings: []string{"a"},
			sources:  [][]any{},
			fanout:   1,
		},
		{
			name:    "zero dimensions bypass",
			fanout:  1,
			sources: nil,
		},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			plan := NewRecordQueryInUnionPlan(
				NewRecordQueryValuesPlan(nil),
				test.bindings,
				nil,
				false,
			)
			plan.SetInSources(test.sources)

			got := plan.HintCost([]properties.Cost{child}, properties.DefaultStatistics{})
			var want properties.Cost
			switch test.fanout {
			case 0:
				want = properties.Cost{}
			case 1:
				want = child
			default:
				// NO PhysicalWrapperCostMultiplier on Cardinality — CPU only
				// (cost_formulas.go): the row count is a logical-group
				// property and cannot shrink from wrapping the plan.
				want = properties.Cost{
					Cardinality: child.Cardinality * test.fanout,
					CPU: (child.CPU*test.fanout +
						child.Cardinality*test.fanout*properties.UnionCPU) *
						properties.PhysicalWrapperCostMultiplier,
				}
			}
			if got != want {
				t.Fatalf("HintCost() = %+v, want %+v", got, want)
			}
		})
	}
}

func TestInUnionHintCost_SaturatesUnknownFanoutOverflow(t *testing.T) {
	t.Parallel()

	const dimensions = 63 // 2^63 cannot fit in int64.
	bindings := make([]string, dimensions)
	sources := make([][]any, dimensions)
	for i := range bindings {
		bindings[i] = "binding"
		sources[i] = []any{0, 1}
	}
	plan := NewRecordQueryInUnionPlan(
		NewRecordQueryValuesPlan(nil),
		bindings,
		nil,
		false,
	)
	plan.SetInSources(sources)
	if _, known := plan.LiteralFanout(); known {
		t.Fatal("overflowing literal fanout unexpectedly reported exact")
	}

	child := properties.Cost{Cardinality: 20, CPU: 5}
	got := plan.HintCost([]properties.Cost{child}, properties.DefaultStatistics{})
	wantFanout := float64(math.MaxInt64)
	want := properties.Cost{
		Cardinality: child.Cardinality * wantFanout,
		CPU: (child.CPU*wantFanout +
			child.Cardinality*wantFanout*properties.UnionCPU) *
			properties.PhysicalWrapperCostMultiplier,
	}
	if math.IsNaN(got.Cardinality) || math.IsInf(got.Cardinality, 0) ||
		math.IsNaN(got.CPU) || math.IsInf(got.CPU, 0) {
		t.Fatalf("overflow heuristic must remain finite, got %+v", got)
	}
	if got != want {
		t.Fatalf("HintCost() = %+v, want saturated fanout cost %+v", got, want)
	}
}

func TestLiteralFanout_RejectsMismatchedDimensions(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		bindings []string
		sources  [][]any
	}{
		{
			name:     "extra empty source",
			bindings: []string{"a"},
			sources:  [][]any{{1}, {}},
		},
		{
			name:     "missing source",
			bindings: []string{"a", "b"},
			sources:  [][]any{{1}},
		},
		{
			name:    "sources without bindings",
			sources: [][]any{{1}},
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			plan := NewRecordQueryInUnionPlan(
				NewRecordQueryValuesPlan(nil),
				test.bindings,
				nil,
				false,
			)
			plan.SetInSources(test.sources)
			if fanout, known := plan.LiteralFanout(); known {
				t.Fatalf("LiteralFanout() = (%d, true), want unknown mismatch", fanout)
			}
		})
	}
}

func TestDefaultOnEmptyHintCost_TransparentWithFinalOnlyChild(t *testing.T) {
	t.Parallel()

	scan := NewRecordQueryScanPlan([]string{"T"}, values.UnknownType, false)
	defaultOnEmpty := NewRecordQueryDefaultOnEmptyPlan(
		scan,
		values.NewNullValue(values.UnknownType),
	)
	want := properties.EstimateCost(scan)
	if got := properties.EstimateCost(defaultOnEmpty); got != want {
		t.Fatalf("EstimateCost(DefaultOnEmpty(scan)) = %+v, want transparent child cost %+v", got, want)
	}
}
