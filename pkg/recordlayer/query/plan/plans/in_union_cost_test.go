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
			plan := mustChecked(t, func() (*RecordQueryInUnionPlan, error) {
				return NewRecordQueryInUnionPlan(mustChecked(t, func() (*RecordQueryValuesPlan, error) {
					return NewRecordQueryValuesPlan(nil)
				}), test.bindings,
					nil,
					false,
				)
			})
			plan = plan.WithInSources(test.sources)

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
	plan := mustChecked(t, func() (*RecordQueryInUnionPlan, error) {
		return NewRecordQueryInUnionPlan(mustChecked(t, func() (*RecordQueryValuesPlan, error) {
			return NewRecordQueryValuesPlan(nil)
		}), bindings,
			nil,
			false,
		)
	})
	plan = plan.WithInSources(sources)
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
			plan := mustChecked(t, func() (*RecordQueryInUnionPlan, error) {
				return NewRecordQueryInUnionPlan(mustChecked(t, func() (*RecordQueryValuesPlan, error) {
					return NewRecordQueryValuesPlan(nil)
				}), test.bindings,
					nil,
					false,
				)
			})
			plan = plan.WithInSources(test.sources)
			if fanout, known := plan.LiteralFanout(); known {
				t.Fatalf("LiteralFanout() = (%d, true), want unknown mismatch", fanout)
			}
		})
	}
}

func TestDefaultOnEmptyHintCost_TransparentWithFinalOnlyChild(t *testing.T) {
	t.Parallel()

	scan := mustChecked(t, func() (*RecordQueryScanPlan, error) {
		return NewRecordQueryScanPlan([]string{"T"}, exactTestRecordType(), false)
	})
	defaultOnEmpty := mustChecked(t, func() (*RecordQueryDefaultOnEmptyPlan, error) {
		return NewRecordQueryDefaultOnEmptyPlan(
			scan,
			values.NewNullValue(values.WithNullability(exactTestRecordType(), true)),
		)
	})
	want := properties.EstimateCost(scan)
	if got := properties.EstimateCost(defaultOnEmpty); got != want {
		t.Fatalf("EstimateCost(DefaultOnEmpty(scan)) = %+v, want transparent child cost %+v", got, want)
	}
}

// TestInListBuildersPreserveNilVersusEmpty pins the distinction the copying
// builders must not erase.
//
// The builders copy their argument so two plans never share one identity-bearing
// array. The obvious way to write that copy — `append([]any(nil), src...)` — is
// wrong in one direction only: appending zero elements to a nil slice yields nil,
// so an EMPTY non-nil list silently becomes nil.
//
// That is a cost bug, not a cosmetic one. nil means "in-list size unknown at plan
// time" and empty means "known to be empty"; they produce different fanouts and so
// different plans. TestInUnionHintCost_UsesValueCombinationCount caught it through
// the cost, which is the right end-to-end signal but names the symptom. This names
// the invariant, and covers the nested case the cost test exercises only indirectly:
// an outer list holding one nil dimension beside one known-empty dimension.
func TestInListBuildersPreserveNilVersusEmpty(t *testing.T) {
	t.Parallel()

	t.Run("in-union sources", func(t *testing.T) {
		t.Parallel()
		plan := mustChecked(t, func() (*RecordQueryInUnionPlan, error) {
			return NewRecordQueryInUnionPlan(mustChecked(t, func() (*RecordQueryValuesPlan, error) {
				return NewRecordQueryValuesPlan(nil)
			}), []string{"a", "b"}, nil, false)
		})

		if got := plan.WithInSources(nil).GetInSources(); got != nil {
			t.Errorf("nil sources became %#v; the cost model would read a plan whose in-list "+
				"size is UNKNOWN as one known to have zero combinations", got)
		}

		mixed := plan.WithInSources([][]any{nil, {}}).GetInSources()
		if len(mixed) != 2 {
			t.Fatalf("got %d dimensions, want 2", len(mixed))
		}
		if mixed[0] != nil {
			t.Errorf("the unknown dimension became %#v", mixed[0])
		}
		if mixed[1] == nil {
			t.Error("the known-empty dimension became nil, turning a known-zero fanout into " +
				"an unknown one")
		}
		if len(mixed[1]) != 0 {
			t.Errorf("the known-empty dimension gained %d values", len(mixed[1]))
		}
	})

	t.Run("in-join values", func(t *testing.T) {
		t.Parallel()
		inner := mustChecked(t, func() (*RecordQueryValuesPlan, error) {
			return NewRecordQueryValuesPlan(nil)
		})
		plan := mustChecked(t, func() (*RecordQueryInJoinPlan, error) {
			return NewRecordQueryInJoinPlan(inner, "x", false, false)
		})

		if got := plan.WithInValues(nil).GetInValues(); got != nil {
			t.Errorf("nil values became %#v", got)
		}
		if got := plan.WithInValues([]any{}).GetInValues(); got == nil {
			t.Error("an empty non-nil value list became nil")
		}
	})

	t.Run("the copy does not alias its argument", func(t *testing.T) {
		t.Parallel()
		plan := mustChecked(t, func() (*RecordQueryInUnionPlan, error) {
			return NewRecordQueryInUnionPlan(mustChecked(t, func() (*RecordQueryValuesPlan, error) {
				return NewRecordQueryValuesPlan(nil)
			}), []string{"a", "b"}, nil, false)
		})

		src := [][]any{{int64(1), int64(2)}}
		built := plan.WithInSources(src)
		src[0][0] = int64(99) // a caller mutating what it handed in

		if got := built.GetInSources()[0][0]; got != int64(1) {
			t.Errorf("the plan saw %v after its caller mutated the slice it passed; the two "+
				"share a backing array, so the plan's structural key changed with its "+
				"pointer unchanged — invisible to the memo's owner check", got)
		}
	})
}
