package executor

import (
	"context"
	"errors"
	"math"
	"testing"

	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

func inUnionMaxSizeSource(n int) []any {
	source := make([]any, n)
	for i := range source {
		source[i] = int64(i)
	}
	return source
}

func TestInUnionValuesSize(t *testing.T) {
	t.Parallel()

	wide := make([]any, 65536)
	tests := []struct {
		name    string
		sources [][]any
		size    int64
		known   bool
	}{
		{name: "one source", sources: [][]any{inUnionMaxSizeSource(3)}, size: 3, known: true},
		{name: "product", sources: [][]any{inUnionMaxSizeSource(4), inUnionMaxSizeSource(6)}, size: 24, known: true},
		{name: "empty source is zero", sources: [][]any{inUnionMaxSizeSource(5), {}}, size: 0, known: true},
		{name: "empty before unknown", sources: [][]any{{}, nil}, size: 0, known: true},
		{name: "unknown source", sources: [][]any{inUnionMaxSizeSource(5), nil}, known: false},
		// Java's int product wraps here to 0 and answers as if a source were
		// empty; Go's does not wrap.
		{name: "no int wrap", sources: [][]any{wide, wide}, size: 1 << 32, known: true},
		{name: "saturates", sources: [][]any{wide, wide, wide, wide}, size: math.MaxInt64, known: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			size, known := inUnionValuesSize(test.sources)
			if known != test.known || (known && size != test.size) {
				t.Fatalf("inUnionValuesSize() = (%d, %t), want (%d, %t)", size, known, test.size, test.known)
			}
		})
	}
}

// RecordQueryInUnionPlan.java:151-153 refuses a product of source sizes above
// the plan's maximum before any child opens; the relational layer plans with
// a maximum of 24.
func TestExecuteInUnion_MaxSizeBoundsProduct(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		sizes   []int
		refused bool
	}{
		{name: "24 values run", sizes: []int{24}},
		{name: "25 values refused", sizes: []int{25}, refused: true},
		{name: "4 by 6 runs", sizes: []int{4, 6}},
		{name: "5 by 5 refused", sizes: []int{5, 5}, refused: true},
		{name: "wrapping product refused", sizes: []int{65536, 65536}, refused: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			aliases := make([]values.CorrelationIdentifier, len(test.sizes))
			sources := make([][]any, len(test.sizes))
			for i, n := range test.sizes {
				aliases[i] = values.NamedCorrelationIdentifier("in" + string(rune('a'+i)))
				sources[i] = inUnionMaxSizeSource(n)
			}
			inUnion := mustExecutorConstruct(plans.NewRecordQueryInUnionPlanWithBindingAliasesAndMaxSize(
				mustExecutorConstruct(plans.NewRecordQueryValuesPlan(nil)),
				aliases,
				nil,
				false,
				24,
			)).WithInSources(sources)

			ctx := context.Background()
			cursor, err := executeInUnion(ctx, inUnion, nil, EmptyEvaluationContext(), nil, recordlayer.ExecuteProperties{})
			if test.refused {
				var coreErr *recordlayer.RecordCoreError
				if !errors.As(err, &coreErr) || coreErr.Message != "too many IN values" {
					t.Fatalf("executeInUnion() error = %v, want RecordCoreError %q", err, "too many IN values")
				}
				return
			}
			if err != nil {
				t.Fatalf("executeInUnion() error = %v", err)
			}
			// The inner yields one empty row per child execution.
			rows, err := CollectAll(ctx, cursor)
			if err != nil {
				t.Fatalf("CollectAll() error = %v", err)
			}
			if len(rows) == 0 {
				t.Fatal("the in-union ran no child")
			}
		})
	}
}
