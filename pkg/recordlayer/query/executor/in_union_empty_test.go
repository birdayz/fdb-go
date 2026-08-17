package executor

import (
	"context"
	"testing"

	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

func TestExecuteInUnion_KnownEmptySourceSkipsInner(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		sources [][]any
	}{
		{name: "only empty", sources: [][]any{{}}},
		{name: "unknown then empty", sources: [][]any{nil, {}}},
		{name: "empty then unknown", sources: [][]any{{}, nil}},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			bindings := make([]string, len(test.sources))
			for i := range bindings {
				bindings[i] = "binding"
			}
			inUnion := mustExecutorConstruct(plans.NewRecordQueryInUnionPlan(
				mustExecutorConstruct(plans.NewRecordQueryValuesPlan(nil)),
				bindings,
				nil,
				false,
			))
			inUnion.SetInSources(test.sources)

			ctx := context.Background()
			cursor, err := executeInUnion(
				ctx,
				inUnion,
				nil,
				nil,
				nil,
				recordlayer.ExecuteProperties{},
			)
			if err != nil {
				t.Fatalf("executeInUnion() error = %v", err)
			}
			result, err := cursor.OnNext(ctx)
			if err != nil {
				t.Fatalf("empty cursor OnNext() error = %v", err)
			}
			if result.HasNext() {
				t.Fatal("known-empty InUnion unexpectedly produced a row")
			}
			if !result.GetNoNextReason().IsSourceExhausted() {
				t.Fatalf("known-empty InUnion stopped with %v, want SourceExhausted", result.GetNoNextReason())
			}
		})
	}
}

func TestExecuteInUnion_MultiBindingSingletonBindsCompleteContext(t *testing.T) {
	t.Parallel()

	const (
		firstBinding  = "first"
		secondBinding = "second"
	)
	inUnion := mustExecutorConstruct(plans.NewRecordQueryInUnionPlan(
		mustExecutorConstruct(plans.NewRecordQueryValuesPlan([]values.Value{
			mustTestQOV(t, values.NamedCorrelationIdentifier(firstBinding), values.NotNullLong),
			mustTestQOV(t, values.NamedCorrelationIdentifier(secondBinding), values.NotNullLong),
		})),
		[]string{firstBinding, secondBinding},
		nil,
		false,
	))
	inUnion.SetInSources([][]any{{int64(11)}, {int64(22)}})

	ctx := context.Background()
	cursor, err := executeInUnion(
		ctx,
		inUnion,
		nil,
		EmptyEvaluationContext(),
		nil,
		recordlayer.ExecuteProperties{},
	)
	if err != nil {
		t.Fatalf("executeInUnion() error = %v", err)
	}
	results, err := CollectAll(ctx, cursor)
	if err != nil {
		t.Fatalf("CollectAll() error = %v", err)
	}
	if len(results) != 1 || results[0].Positional == nil {
		t.Fatalf("results = %+v, want one positional row", results)
	}
	slots := results[0].Positional.Slots
	if len(slots) != 2 || slots[0] != int64(11) || slots[1] != int64(22) {
		t.Fatalf("bound slots = %#v, want [11 22]", slots)
	}
}

func TestExecuteInUnion_PreservesUniqueBindingIdentity(t *testing.T) {
	t.Parallel()
	binding := values.UniqueCorrelationIdentifier()
	inner := mustExecutorConstruct(plans.NewRecordQueryValuesPlan([]values.Value{
		mustTestQOV(t, binding, values.NotNullLong),
	}))
	inUnion := mustExecutorConstruct(plans.NewRecordQueryInUnionPlanWithBindingAliases(
		inner, []values.CorrelationIdentifier{binding}, nil, false))
	inUnion.SetInSources([][]any{{int64(11), int64(22)}})
	aliases := inUnion.GetBindingAliases()
	if len(aliases) != 1 || aliases[0] != binding {
		t.Fatalf("binding aliases = %v, want exact unique alias %v", aliases, binding)
	}
	if named := values.NamedCorrelationIdentifier(binding.Name()); named == binding {
		t.Fatal("fixture requires unique and named aliases with the same spelling to stay distinct")
	}

	cursor, err := executeInUnion(
		context.Background(), inUnion, nil, EmptyEvaluationContext(), nil,
		recordlayer.ExecuteProperties{})
	if err != nil {
		t.Fatalf("executeInUnion: %v", err)
	}
	results, err := CollectAll(context.Background(), cursor)
	if err != nil {
		t.Fatalf("CollectAll: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("results = %d, want 2", len(results))
	}
	for i, want := range []int64{11, 22} {
		if results[i].Positional == nil || len(results[i].Positional.Slots) != 1 ||
			results[i].Positional.Slots[0] != want {
			t.Fatalf("result %d = %+v, want [%d]", i, results[i], want)
		}
	}
}

func TestExecuteInUnion_SingletonAppliesSkip(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		bindings []string
		sources  [][]any
	}{
		{
			name:     "one binding",
			bindings: []string{"a"},
			sources:  [][]any{{1}},
		},
		{
			name:     "two bindings",
			bindings: []string{"a", "b"},
			sources:  [][]any{{1}, {2}},
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			inUnion := mustExecutorConstruct(plans.NewRecordQueryInUnionPlan(
				mustExecutorConstruct(plans.NewRecordQueryValuesPlan(nil)),
				test.bindings,
				nil,
				false,
			))
			inUnion.SetInSources(test.sources)

			ctx := context.Background()
			cursor, err := executeInUnion(
				ctx,
				inUnion,
				nil,
				EmptyEvaluationContext(),
				nil,
				recordlayer.ExecuteProperties{Skip: 1},
			)
			if err != nil {
				t.Fatalf("executeInUnion() error = %v", err)
			}
			results, err := CollectAll(ctx, cursor)
			if err != nil {
				t.Fatalf("CollectAll() error = %v", err)
			}
			if len(results) != 0 {
				t.Fatalf("Skip=1 returned %d singleton rows, want 0", len(results))
			}
		})
	}
}

func TestExecuteInUnion_RejectsMismatchedDimensions(t *testing.T) {
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
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			inUnion := mustExecutorConstruct(plans.NewRecordQueryInUnionPlan(
				mustExecutorConstruct(plans.NewRecordQueryValuesPlan(nil)),
				test.bindings,
				nil,
				false,
			))
			inUnion.SetInSources(test.sources)
			if _, err := executeInUnion(
				context.Background(),
				inUnion,
				nil,
				EmptyEvaluationContext(),
				nil,
				recordlayer.ExecuteProperties{},
			); err == nil {
				t.Fatal("executeInUnion() accepted mismatched dimensions")
			}
		})
	}
}
