package executor

import (
	"context"
	"fmt"
	"testing"

	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

// drainPaged executes plan page by page with a returned-row limit of
// rowsPerPage, round-tripping the real continuation BYTES between pages exactly
// as the SQL statement's paging loop does, and returns the emitted rows plus
// the byte length of every page's continuation.
//
// evalCtx decides whether the execution carries an ExecutionScratch, which is
// the whole subject of these tests: with one, the seen-set is handed forward
// live and the continuation is O(1); without one it is written into every page.
func drainPaged(
	t *testing.T,
	ctx context.Context,
	plan plans.RecordQueryPlan,
	evalCtx *EvaluationContext,
	rowsPerPage int,
) (rows []QueryResult, contSizes []int) {
	t.Helper()
	// One statement-wide state, as the SQL layer mints it, so a per-page charge
	// that is never released shows up as a growing MemUsed rather than being
	// hidden by a fresh counter per page.
	state := recordlayer.NewExecuteState(1 << 30)
	var cont []byte
	for page := 0; page < 100000; page++ {
		props := recordlayer.DefaultExecuteProperties()
		props.State = state
		props = props.WithReturnedRowLimit(rowsPerPage)
		cursor, err := ExecutePlan(ctx, plan, nil, evalCtx, cont, props)
		if err != nil {
			t.Fatalf("page %d ExecutePlan: %v", page, err)
		}
		var last recordlayer.RecordCursorResult[QueryResult]
		for {
			res, err := cursor.OnNext(ctx)
			if err != nil {
				t.Fatalf("page %d OnNext: %v", page, err)
			}
			last = res
			if !res.HasNext() {
				break
			}
			rows = append(rows, res.GetValue())
		}
		nextCont := last.GetContinuation()
		var b []byte
		if nextCont != nil && !nextCont.IsEnd() {
			var berr error
			// Serialize BEFORE Close: the paging loop reads the terminal
			// continuation off a live result set whose Close is deferred.
			b, berr = nextCont.ToBytes()
			if berr != nil {
				t.Fatalf("page %d continuation ToBytes: %v", page, berr)
			}
		}
		if err := cursor.Close(); err != nil {
			t.Fatalf("page %d Close: %v", page, err)
		}
		if b == nil {
			return rows, contSizes
		}
		contSizes = append(contSizes, len(b))
		cont = b
	}
	t.Fatal("paged drain did not terminate")
	return nil, nil
}

// distinctFixture seeds n rows whose dedup value repeats every `distinct`
// values, so the drain must suppress duplicates that are spread across pages
// rather than adjacent.
func distinctFixture(t *testing.T, n, distinct int) (*EvaluationContext, plans.RecordQueryPlan) {
	t.Helper()
	alias := values.NamedCorrelationIdentifier(fmt.Sprintf("distinct_%s", t.Name()))
	evalCtx := EmptyEvaluationContext()
	table := evalCtx.GetOrCreateTempTable(alias, nil)
	rowType := exactTestRowType(values.Field{Name: "V", FieldType: values.NullableLong})
	for i := 0; i < n; i++ {
		if err := table.Add(QueryResult{Positional: &PositionalRow{
			Type:  rowType,
			Slots: []any{int64(i % distinct)},
		}}); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	return evalCtx, mustExecutorConstruct(plans.NewRecordQueryDistinctPlan(mustTempTableScan(t, evalCtx, alias)))
}

// TestDistinctHashContinuationSizeIsBoundedWithScratch pins that the unordered
// hash DISTINCT's continuation does NOT grow with the number of pages drained.
//
// THE DEFECT IT DETECTS: the executor used to serialize EVERY key seen so far
// into EVERY page's continuation, so page P's continuation was O(P) bytes and a
// P-page drain wrote and re-parsed O(P^2) keys in total. Measured before the
// fix on this exact shape: 9 bytes at page 1, 809 at page 200, 1749 at page
// 400. The dedup state now rides the statement's ExecutionScratch by reference.
//
// Every value is DISTINCT here on purpose — that is the worst case (the shape a
// UNIQUE column produces), where the set grows by one key on every single row
// and nothing can ever be dropped from it.
func TestDistinctHashContinuationSizeIsBoundedWithScratch(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	const n = 400
	evalCtx, plan := distinctFixture(t, n, n)
	evalCtx = evalCtx.WithExecutionScratch(NewExecutionScratch())

	rows, sizes := drainPaged(t, ctx, plan, evalCtx, 1)
	if len(rows) != n {
		t.Fatalf("paged drain returned %d rows, want %d", len(rows), n)
	}
	if len(sizes) < n-1 {
		t.Fatalf("expected ~%d pages at one row per page, got %d", n, len(sizes))
	}
	// A continuation carrying the inner position plus one varint token is a
	// couple of dozen bytes and does not depend on how many rows preceded it.
	// The bound is generous but FAR below the ~1.7 KB the by-value encoding
	// reached at page 400 — and, crucially, it is a CONSTANT, so it fails the
	// moment size regains a per-page term.
	const bound = 64
	for i, s := range sizes {
		if s > bound {
			t.Fatalf(
				"page %d continuation is %d bytes (> %d): the seen-set is being "+
					"serialized into every page again, which is O(pages^2) work "+
					"across the drain (page 0 was %d bytes)",
				i, s, bound, sizes[0],
			)
		}
	}
	// And it must not creep: the last page's continuation is the same size as
	// the first's, up to varint width on the token.
	if grew := sizes[len(sizes)-1] - sizes[0]; grew > 4 {
		t.Fatalf(
			"continuation grew %d bytes from page 0 (%d) to page %d (%d) — size "+
				"must not depend on how many distinct values have been emitted",
			grew, sizes[0], len(sizes)-1, sizes[len(sizes)-1],
		)
	}
	t.Logf("pages=%d continuation bytes: first=%d mid=%d last=%d",
		len(sizes), sizes[0], sizes[len(sizes)/2], sizes[len(sizes)-1])
}

// TestDistinctHashPagedDrainIsExact pins the correctness half: across a
// many-page drain resumed through real continuation round-trips, every distinct
// value is emitted EXACTLY once. Duplicates are spread (i % distinct), so every
// duplicate run straddles page boundaries — an implementation that loses its
// dedup state on resume re-admits them, and one that over-suppresses drops a
// value. Both failures are named separately.
func TestDistinctHashPagedDrainIsExact(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	const n, distinct = 300, 37
	evalCtx, plan := distinctFixture(t, n, distinct)
	evalCtx = evalCtx.WithExecutionScratch(NewExecutionScratch())

	rows, _ := drainPaged(t, ctx, plan, evalCtx, 1)
	seen := map[any]int{}
	for _, r := range rows {
		seen[r.Positional.Slots[0]]++
	}
	for v, count := range seen {
		if count != 1 {
			t.Fatalf("value %v emitted %d times across the paged drain — the "+
				"resume lost its seen-set and re-admitted a duplicate", v, count)
		}
	}
	if len(seen) != distinct {
		t.Fatalf("paged drain emitted %d distinct values, want %d — the resume "+
			"DROPPED a value it had not yet emitted", len(seen), distinct)
	}
	if len(rows) != distinct {
		t.Fatalf("paged drain emitted %d rows, want %d", len(rows), distinct)
	}
}

// TestDistinctHashScratchlessResumeStaysSelfContained pins the fallback: an
// execution with NO scratch still resumes correctly, because its continuation
// writes the seen-set by value. That encoding is quadratic and is why the
// scratch exists, so this test also documents the price by asserting the
// continuation DOES grow — if a future change makes the scratch-less path
// bounded too, this test is the one that must be revisited deliberately rather
// than a bound silently going unenforced.
func TestDistinctHashScratchlessResumeStaysSelfContained(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	const n, distinct = 200, 23
	evalCtx, plan := distinctFixture(t, n, distinct)

	rows, sizes := drainPaged(t, ctx, plan, evalCtx, 1)
	if len(rows) != distinct {
		t.Fatalf("scratch-less paged drain emitted %d rows, want %d distinct values",
			len(rows), distinct)
	}
	if sizes[len(sizes)-1] <= sizes[0] {
		t.Fatalf("scratch-less continuation did not grow (%d -> %d): the by-value "+
			"seen-set encoding is the only thing that makes a scratch-less resume "+
			"dedup-clean, so a flat size means it stopped carrying the set",
			sizes[0], sizes[len(sizes)-1])
	}
}

// TestDistinctHashUnknownScratchTokenFailsLoudly pins that a continuation
// naming a seen-set the execution cannot produce is an ERROR, never a resume
// against an empty set. Deduping against an empty set would re-admit every
// value already emitted — wrong rows, silently — which is precisely the failure
// mode Java's plan has (RecordQueryUnorderedPrimaryKeyDistinctPlan mints a
// fresh HashSet per execution and passes the inner continuation through).
func TestDistinctHashUnknownScratchTokenFailsLoudly(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	const n, distinct = 40, 7
	evalCtx, plan := distinctFixture(t, n, distinct)
	scratched := evalCtx.WithExecutionScratch(NewExecutionScratch())

	props := recordlayer.DefaultExecuteProperties()
	props.State = recordlayer.NewExecuteState(1 << 30)
	props = props.WithReturnedRowLimit(1)
	cursor, err := ExecutePlan(ctx, plan, nil, scratched, nil, props)
	if err != nil {
		t.Fatalf("first page: %v", err)
	}
	res, err := cursor.OnNext(ctx)
	if err != nil || !res.HasNext() {
		t.Fatalf("first row = %#v, err = %v", res, err)
	}
	cont, err := res.GetContinuation().ToBytes()
	if err != nil {
		t.Fatalf("continuation: %v", err)
	}
	_ = cursor.Close()

	// Resume the very same bytes on an execution whose scratch never held that
	// token: a fresh scratch, and no scratch at all.
	for name, resumeCtx := range map[string]*EvaluationContext{
		"fresh scratch": evalCtx.WithExecutionScratch(NewExecutionScratch()),
		"no scratch":    evalCtx,
	} {
		props := recordlayer.DefaultExecuteProperties()
		props.State = recordlayer.NewExecuteState(1 << 30)
		if _, err := ExecutePlan(ctx, plan, nil, resumeCtx, cont, props); err == nil {
			t.Fatalf("%s: resuming a token-bearing continuation succeeded; it must "+
				"fail loudly rather than dedup against an empty set", name)
		}
	}
}
