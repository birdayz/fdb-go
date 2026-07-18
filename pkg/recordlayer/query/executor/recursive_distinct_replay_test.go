package executor

import (
	"context"
	"testing"

	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

// drainOneRowPages drives plan page-by-page with ReturnedRowLimit=1,
// resuming purely from each page's continuation — the deterministic
// executor-level equivalent of the driver's paging loop.
func drainOneRowPages(t *testing.T, plan plans.RecordQueryPlan, pageCap int) []int64 {
	t.Helper()
	ctx := context.Background()
	props := recordlayer.DefaultExecuteProperties()
	props.ReturnedRowLimit = 1
	var out []int64
	var continuation []byte
	for page := 0; ; page++ {
		if page > pageCap {
			t.Fatalf("page cap (%d) hit — the replay continuation never advanced", pageCap)
		}
		cur, err := ExecutePlan(ctx, plan, nil, EmptyEvaluationContext(), continuation, props)
		if err != nil {
			t.Fatalf("page %d: %v", page, err)
		}
		gotRow := false
		for {
			r, err := cur.OnNext(ctx)
			if err != nil {
				t.Fatalf("page %d next: %v", page, err)
			}
			if r.HasNext() {
				gotRow = true
				v := r.GetValue().Positional.Slots[0]
				out = append(out, v.(int64))
				continue
			}
			if r.GetNoNextReason().IsSourceExhausted() {
				cur.Close()
				return out
			}
			cb, cerr := r.GetContinuation().ToBytes()
			if cerr != nil {
				t.Fatalf("page %d continuation: %v", page, cerr)
			}
			if cb == nil {
				t.Fatalf("page %d: out-of-band stop with no continuation", page)
			}
			continuation = cb
			break
		}
		cur.Close()
		if !gotRow && page > 0 {
			t.Fatalf("page %d made no progress", page)
		}
	}
}

// TestRecursiveDistinctReplay_ResumesMidStream pins the DISTINCT
// recursion extension's position-replay resume on both plan shapes
// (Java rejects recursive UNION distinct — the extension's seen-set is
// the emission history, so resume re-executes deterministically, skips
// the emitted prefix, and integrity-checks the boundary row). Driven
// page-by-page with ReturnedRowLimit=1: every row crosses a resume.
func TestRecursiveDistinctReplay_ResumesMidStream(t *testing.T) {
	t.Parallel()
	scanAlias := values.NamedCorrelationIdentifier("scan")
	insertAlias := values.NamedCorrelationIdentifier("insert")
	seedList := func() values.Value {
		return &values.ConstantValue{Value: []any{int64(1), int64(2), int64(3)}}
	}

	t.Run("level_union", func(t *testing.T) {
		t.Parallel()
		// Seed [1,2,3]; the recursive leg echoes the frontier — every
		// echoed row dedups away, so the recursion terminates at level 1.
		plan := plans.NewRecordQueryRecursiveLevelUnionPlanDistinct(
			plans.NewRecordQueryTempTableInsertPlan(plans.NewRecordQueryExplodePlan(seedList()), insertAlias, false),
			plans.NewRecordQueryTempTableInsertPlan(plans.NewRecordQueryTempTableScanPlan(scanAlias), insertAlias, false),
			scanAlias, insertAlias)
		got := drainOneRowPages(t, plan, 16)
		want := []int64{1, 2, 3}
		if len(got) != len(want) {
			t.Fatalf("paged DISTINCT level union = %v, want %v", got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("paged DISTINCT level union = %v, want %v", got, want)
			}
		}
	})

	t.Run("dfs", func(t *testing.T) {
		t.Parallel()
		// Root [1,2,3]; each node's child echoes the node itself — the
		// echo dedups away, so every node is a leaf.
		plan := plans.NewRecordQueryRecursiveDfsJoinPlanDistinct(
			plans.NewRecordQueryExplodePlan(seedList()),
			plans.NewRecordQueryTempTableScanPlan(scanAlias),
			scanAlias, plans.DfsPreorder)
		got := drainOneRowPages(t, plan, 16)
		want := []int64{1, 2, 3}
		if len(got) != len(want) {
			t.Fatalf("paged DISTINCT DFS = %v, want %v", got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("paged DISTINCT DFS = %v, want %v", got, want)
			}
		}
	})

	// Drift under the token fails LOUDLY: a token whose boundary row no
	// longer matches the replay (here: a token from the [1,2,3] run
	// presented to a recursion over [9,2,3]) must error, never silently
	// re-emit or skip.
	t.Run("drift_is_loud", func(t *testing.T) {
		t.Parallel()
		ctx := context.Background()
		props := recordlayer.DefaultExecuteProperties()
		props.ReturnedRowLimit = 1
		mk := func(first int64) plans.RecordQueryPlan {
			return plans.NewRecordQueryRecursiveLevelUnionPlanDistinct(
				plans.NewRecordQueryTempTableInsertPlan(
					plans.NewRecordQueryExplodePlan(&values.ConstantValue{Value: []any{first, int64(2), int64(3)}}), insertAlias, false),
				plans.NewRecordQueryTempTableInsertPlan(plans.NewRecordQueryTempTableScanPlan(scanAlias), insertAlias, false),
				scanAlias, insertAlias)
		}
		cur, err := ExecutePlan(ctx, mk(1), nil, EmptyEvaluationContext(), nil, props)
		if err != nil {
			t.Fatalf("first page: %v", err)
		}
		r, err := cur.OnNext(ctx)
		if err != nil || !r.HasNext() {
			t.Fatalf("first row: %v", err)
		}
		r2, err := cur.OnNext(ctx)
		if err != nil || r2.HasNext() {
			t.Fatalf("expected the in-band limit stop, got %v / %v", r2, err)
		}
		token, _ := r2.GetContinuation().ToBytes()
		cur.Close()

		drifted, err := ExecutePlan(ctx, mk(9), nil, EmptyEvaluationContext(), token, props)
		if err == nil {
			_, err = drifted.OnNext(ctx)
			drifted.Close()
		}
		if err == nil {
			t.Fatal("a drifted replay token must fail loudly, not serve rows")
		}
	})
}

// TestListShapedPlans_ResumeMidList pins the Java ListCursor resume
// semantics (a 4-byte big-endian position) on every list-shaped plan the
// executor serves: explode, temp-table scan, and VALUES — each formerly a
// loud resume decline, diverging from Java (RecordQueryExplodePlan:
// RecordCursor.fromList(list, continuation); TempTableScanPlan:
// ListCursor(table.getList(), continuation)).
func TestListShapedPlans_ResumeMidList(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	props := recordlayer.DefaultExecuteProperties()

	drain := func(t *testing.T, plan plans.RecordQueryPlan, evalCtx *EvaluationContext, continuation []byte) []int64 {
		t.Helper()
		cur, err := ExecutePlan(ctx, plan, nil, evalCtx, continuation, props)
		if err != nil {
			t.Fatalf("execute: %v", err)
		}
		defer cur.Close()
		var out []int64
		for {
			r, err := cur.OnNext(ctx)
			if err != nil {
				t.Fatalf("next: %v", err)
			}
			if !r.HasNext() {
				return out
			}
			out = append(out, r.GetValue().Positional.Slots[0].(int64))
		}
	}

	pos := func(n int) []byte { return []byte{byte(n >> 24), byte(n >> 16), byte(n >> 8), byte(n)} }

	t.Run("explode", func(t *testing.T) {
		t.Parallel()
		plan := plans.NewRecordQueryExplodePlan(&values.ConstantValue{Value: []any{int64(10), int64(20), int64(30)}})
		if got := drain(t, plan, EmptyEvaluationContext(), pos(1)); len(got) != 2 || got[0] != 20 || got[1] != 30 {
			t.Fatalf("explode resumed at 1 = %v, want [20 30]", got)
		}
	})

	t.Run("temp_table_scan", func(t *testing.T) {
		t.Parallel()
		alias := values.NamedCorrelationIdentifier("tt")
		evalCtx := EmptyEvaluationContext()
		tt := evalCtx.GetOrCreateTempTable(alias, props.State)
		for _, v := range []int64{7, 8, 9} {
			if err := tt.Add(QueryResult{Positional: &PositionalRow{Type: positionalTypeFromNames([]string{"N"}), Slots: []any{v}}}); err != nil {
				t.Fatalf("seed: %v", err)
			}
		}
		if got := drain(t, plans.NewRecordQueryTempTableScanPlan(alias), evalCtx, pos(2)); len(got) != 1 || got[0] != 9 {
			t.Fatalf("temp-table scan resumed at 2 = %v, want [9]", got)
		}
	})

	t.Run("values_past_end", func(t *testing.T) {
		t.Parallel()
		plan := plans.NewRecordQueryValuesPlan([]values.Value{&values.ConstantValue{Value: int64(5)}})
		if got := drain(t, plan, EmptyEvaluationContext(), pos(1)); len(got) != 0 {
			t.Fatalf("VALUES resumed past its row = %v, want []", got)
		}
	})
}
