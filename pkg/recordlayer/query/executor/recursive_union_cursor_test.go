package executor

import (
	"context"
	"errors"
	"strings"
	"testing"

	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"

	"fdb.dev/gen"
	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

// TestRecursiveRow_SiblingStructDescriptorResolves pins the recursive
// continuation's descriptor resolution for a dynamic STRUCT slot whose
// message type is a TOP-LEVEL SIBLING of the record types (demo proto's
// Flower — referenced as Order's struct field, never a record type and
// never nested inside one). The retired record-type-only walk (record
// types + their nested messages) could not find it, so resuming a valid
// continuation failed with "message type not found"; the resolver now
// indexes whole parent files — top-level siblings, nested messages, and
// transitive imports (metadataMessageResolver).
func TestRecursiveRow_SiblingStructDescriptorResolves(t *testing.T) {
	t.Parallel()
	builder := recordlayer.NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
	builder.GetRecordType("Order").SetPrimaryKey(recordlayer.Field("order_id"))
	builder.GetRecordType("Customer").SetPrimaryKey(recordlayer.Field("customer_id"))
	builder.GetRecordType("TypedRecord").SetPrimaryKey(recordlayer.Field("id"))
	md, err := builder.Build()
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	resolve := metadataMessageResolver(md)

	// The sibling type resolves directly.
	flowerDesc := (&gen.Flower{}).ProtoReflect().Descriptor()
	got, err := resolve(string(flowerDesc.FullName()))
	if err != nil {
		t.Fatalf("sibling type %s must resolve from store metadata: %v", flowerDesc.FullName(), err)
	}
	if got.FullName() != flowerDesc.FullName() {
		t.Fatalf("resolved %s, want %s", got.FullName(), flowerDesc.FullName())
	}

	// And a buffered row carrying a dynamic Flower slot round-trips through
	// the recursive codec with that resolver — the exact resume path.
	flower := dynamicpb.NewMessage(flowerDesc)
	flower.Set(flowerDesc.Fields().ByName("type"), protoreflect.ValueOfString("rose"))
	qr := dorder([]string{"N", "F"}, []any{int64(7), flower})
	b, err := encodeRecursiveRow(qr)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	back, err := decodeRecursiveRow(b, resolve)
	if err != nil {
		t.Fatalf("decode with sibling struct slot: %v", err)
	}
	if back.Positional == nil || len(back.Positional.Slots) != 2 {
		t.Fatalf("round-trip lost the row: %+v", back)
	}
	if back.Positional.Slots[0] != int64(7) {
		t.Fatalf("scalar slot: got %v", back.Positional.Slots[0])
	}
	msg, ok := back.Positional.Slots[1].(interface{ String() string })
	if !ok || !strings.Contains(msg.String(), "rose") {
		t.Fatalf("struct slot did not round-trip: %T %v", back.Positional.Slots[1], back.Positional.Slots[1])
	}
}

// TestRecursiveUnionCursor_HeldContinuationImmuneToLaterLevelTransition is the
// required regression for Bug 3 (the recursiveUnionContinuation /
// tempTableInsertContinuation live-*TempTable hazard): a row's continuation
// object, captured but NOT YET serialized (ToBytes never called), must
// describe the SAME resumable state no matter how many further rows are
// pulled — and in particular how many recursion LEVEL TRANSITIONS happen —
// before the caller eventually gets around to serializing it. The prior
// design held the live *TempTable pointer and relied on "the pager
// serializes once per page, right after pulling the row" caller discipline;
// this reproduces exactly the case that discipline doesn't cover: a resumed
// row's continuation held across a level transition.
//
// Recursion: level 0 seeds 3 rows [1,2,3] (via TempTableInsertPlan+ValuesPlan
// into the insert table). Level 1's recursive leg always inserts exactly one
// row (100) — chosen so the recursion never naturally terminates, letting
// the test stop after exactly the levels it needs rather than requiring a
// self-terminating shape. row4 (the first row of level 1) is held: its
// continuation snapshots the level-0→1 SCAN FRONTIER, [1,2,3]. The level
// 1→2 transition then swaps buffers and Clears the very TempTable object
// that frontier lived in, and level 2 inserts a new, unrelated row (100
// again) into that recycled object — before row4's continuation is ever
// serialized. A structurally-safe continuation must still decode to the
// original 3-row frontier; the pre-fix live-pointer design would instead
// serialize whatever level 2 happened to insert.
func TestRecursiveUnionCursor_HeldContinuationImmuneToLaterLevelTransition(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	scanAlias := values.NamedCorrelationIdentifier("scan")
	insertAlias := values.NamedCorrelationIdentifier("insert")

	initial := plans.NewRecordQueryTempTableInsertPlan(
		plans.NewRecordQueryExplodePlan(&values.ConstantValue{Value: []any{int64(1), int64(2), int64(3)}}),
		insertAlias, false,
	)
	recursive := plans.NewRecordQueryTempTableInsertPlan(
		plans.NewRecordQueryExplodePlan(&values.ConstantValue{Value: []any{int64(100)}}),
		insertAlias, false,
	)
	plan := plans.NewRecordQueryRecursiveLevelUnionPlan(initial, recursive, scanAlias, insertAlias)

	cur, err := ExecutePlan(ctx, plan, nil, EmptyEvaluationContext(), nil, recordlayer.DefaultExecuteProperties())
	if err != nil {
		t.Fatalf("ExecutePlan: %v", err)
	}
	defer cur.Close()

	next := func(label string) recordlayer.RecordCursorResult[QueryResult] {
		t.Helper()
		r, err := cur.OnNext(ctx)
		if err != nil {
			t.Fatalf("%s: OnNext error: %v", label, err)
		}
		if !r.HasNext() {
			t.Fatalf("%s: expected a row, got no-next (reason=%v)", label, r.GetNoNextReason())
		}
		return r
	}

	next("row1 (level 0)")
	next("row2 (level 0)")
	next("row3 (level 0)")
	// row4: first row of level 1 — the recursive leg's single insert, pulled
	// right after the level 0→1 transition swapped [1,2,3] into the scan
	// table. Hold its continuation WITHOUT serializing.
	row4 := next("row4 (level 1, HELD)")
	heldCont := row4.GetContinuation()

	// Cross the level 1→2 transition: this Clears the very TempTable object
	// that held [1,2,3] (it becomes the new insert table) and then inserts a
	// new, unrelated row (100) into it — all before heldCont is serialized.
	next("row5 (level 2, forces the level 1->2 transition + a fresh insert into the recycled table)")

	heldBytes, err := heldCont.ToBytes()
	if err != nil {
		t.Fatalf("held continuation ToBytes (serialized LATE, after a level transition): %v", err)
	}
	if heldBytes == nil {
		t.Fatal("held continuation must not be an end continuation")
	}
	var rcc gen.RecursiveCursorContinuation
	if err := rcc.UnmarshalVT(heldBytes); err != nil {
		t.Fatalf("unmarshal held continuation: %v", err)
	}
	gotRows := len(rcc.GetTempTable().GetBufferItems())
	if gotRows != 3 {
		t.Fatalf("row4's held continuation, serialized after the level 1->2 transition, snapshots %d rows, want 3 "+
			"([1,2,3], the level 0->1 scan frontier at the time row4 was produced) — "+
			"a live *TempTable reference would instead see whatever level 2 inserted into the recycled table object",
			gotRows)
	}
}

// neverTerminatingLevelUnionPlan builds a level-union recursive CTE whose
// recursive leg unconditionally inserts exactly one row every level — the
// insert table is NEVER empty, so the level_order (RecordQueryRecursiveLevel
// UnionPlan / RecursiveUnionCursor) fixpoint check never fires. Since the
// Go executor only supports UNION ALL for this plan shape (no dedup/seen-set
// arm), the ONLY thing that can stop this recursion is the streaming
// recursion-depth cap — exactly the scenario a cyclic self-referential
// recursive CTE produces against a real graph.
//
// The legs are OWNING inserts (owning=true), matching what the SQL layer
// actually generates for CTE legs (cascades_translator.go's seedInsert /
// recursiveInsert — "Java TempTableInsertExpression.ofCorrelated defaults
// isOwningTempTable=true for CTE legs"). Only the owning arm's
// TempTableInsertCursor snapshots its table in its own continuation, which
// is what lets a rebuilt cursor restore the insert table's accumulated rows
// across a real page boundary — the non-owning arm's continuation carries
// no table state at all, so it cannot be used to test page-to-page resume.
func neverTerminatingLevelUnionPlan() *plans.RecordQueryRecursiveLevelUnionPlan {
	scanAlias := values.NamedCorrelationIdentifier("scan")
	insertAlias := values.NamedCorrelationIdentifier("insert")
	initial := plans.NewRecordQueryTempTableInsertPlan(
		plans.NewRecordQueryExplodePlan(&values.ConstantValue{Value: []any{int64(1), int64(2), int64(3)}}),
		insertAlias, true,
	)
	recursive := plans.NewRecordQueryTempTableInsertPlan(
		plans.NewRecordQueryExplodePlan(&values.ConstantValue{Value: []any{int64(100)}}),
		insertAlias, true,
	)
	return plans.NewRecordQueryRecursiveLevelUnionPlan(initial, recursive, scanAlias, insertAlias)
}

// TestRecursiveUnionCursor_CyclicSinglePage_HitsDepthCap is the "non-paging"
// depth-cap pin required alongside the paging one below: nothing in the
// suite exercised RecursiveCTEDepthExceededError at all before this, for
// either recursion arm. A single, never-rebuilt cursor instance drains a
// recursion that can only stop via the depth cap (see
// neverTerminatingLevelUnionPlan) and must surface
// RecursiveCTEDepthExceededError instead of running forever. The row
// ceiling below turns "the cap is broken" into a fast, clear test failure
// instead of a hung test run.
func TestRecursiveUnionCursor_CyclicSinglePage_HitsDepthCap(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	cur, err := ExecutePlan(ctx, neverTerminatingLevelUnionPlan(), nil, EmptyEvaluationContext(), nil, recordlayer.DefaultExecuteProperties())
	if err != nil {
		t.Fatalf("ExecutePlan: %v", err)
	}
	defer cur.Close()

	const ceiling = 2000 // > maxStreamingRecursionDepth(1000) + the 3-row seed; a broken cap streams straight past this
	var depthErr *RecursiveCTEDepthExceededError
	for n := 1; ; n++ {
		if n > ceiling {
			t.Fatalf("cyclic recursion did not hit the depth cap within %d rows — RecursiveCTEDepthExceededError never fired", ceiling)
		}
		_, err := cur.OnNext(ctx)
		if err != nil {
			if !errors.As(err, &depthErr) {
				t.Fatalf("row %d: got error %v, want RecursiveCTEDepthExceededError", n, err)
			}
			return
		}
	}
}

// TestRecursiveUnionCursor_CyclicAcrossPages_HitsDepthCap is the paging
// regression: it reproduces exactly what the SQL layer's paginatingRows does
// on a real cyclic recursive CTE that scans mid-recursion (see
// TestFDB_RecursiveCTE_Continuation_ResumeAcrossPages for the SQL-level
// analogue) — a FRESH *recursiveUnionCursor is built via
// newRecursiveUnionCursor for every single row, from that row's own
// continuation, exactly like every fetchPage call building a fresh cursor
// hierarchy from the previous page's continuation. Without the statement-
// scoped fix, recursiveUnionCursor.levels lived on the (per-page-rebuilt)
// cursor and reset to zero on every rebuild, so this loop never terminates
// naturally — the depth cap could never fire because no single cursor
// instance ever saw more than one level transition. The row ceiling turns
// that into a fast, clear failure instead of a hang.
//
// The fix threads one recordlayer.ExecuteState — minted ONCE here, exactly
// as cascades_generator.go's paginatingRows.execState is minted once per
// Execute() and re-assigned into every page's ExecuteProperties.State — into
// every rebuilt cursor's props, so the level count survives the rebuild.
func TestRecursiveUnionCursor_CyclicAcrossPages_HitsDepthCap(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	// Statement-scoped: minted ONCE, threaded into every page's props below —
	// this is the mechanism under test.
	execState := recordlayer.NewExecuteState(0)
	props := recordlayer.DefaultExecuteProperties()
	props.State = execState

	plan := neverTerminatingLevelUnionPlan()
	var cont []byte
	const ceiling = 2000 // > maxStreamingRecursionDepth(1000) + the 3-row seed
	var depthErr *RecursiveCTEDepthExceededError
	for n := 1; ; n++ {
		if n > ceiling {
			t.Fatalf("cyclic recursion paged %d times without hitting the depth cap — "+
				"the statement-scoped counter is not surviving the per-page cursor rebuild", ceiling)
		}
		cur, err := ExecutePlan(ctx, plan, nil, EmptyEvaluationContext(), cont, props)
		if err != nil {
			t.Fatalf("page %d: ExecutePlan(cont=%x): %v", n, cont, err)
		}
		r, err := cur.OnNext(ctx)
		if err != nil {
			cur.Close()
			if !errors.As(err, &depthErr) {
				t.Fatalf("page %d: got error %v, want RecursiveCTEDepthExceededError", n, err)
			}
			return
		}
		if !r.HasNext() {
			cur.Close()
			t.Fatalf("page %d: cyclic recursion reported exhausted (reason=%v) — it should never naturally terminate",
				n, r.GetNoNextReason())
		}
		contBytes, err := r.GetContinuation().ToBytes()
		if err != nil {
			cur.Close()
			t.Fatalf("page %d: continuation ToBytes: %v", n, err)
		}
		cur.Close()
		cont = contBytes
	}
}

// countdownExplodeValue is a values.Value whose Evaluate returns a
// single-element collection for the first N calls and an EMPTY collection on
// the (N+1)-th, where N is set by the caller through remaining. Used as the
// recursive leg's collection in naturallyTerminatingLevelUnionPlan so the
// recursion's natural stop point (the insert table coming up empty) lands at
// an EXACT, caller-chosen recursive-leg count instead of depending on data
// content — executeExplode (executor.go) evaluates the collection exactly
// once per leg execution and materializes every element eagerly, so one
// Evaluate call corresponds to exactly one recursive leg.
type countdownExplodeValue struct {
	remaining *int
}

func (c *countdownExplodeValue) Children() []values.Value { return []values.Value{} }
func (c *countdownExplodeValue) Type() values.Type        { return values.UnknownType }
func (c *countdownExplodeValue) Name() string             { return "countdown" }
func (c *countdownExplodeValue) Evaluate(any) (any, error) {
	if *c.remaining <= 0 {
		return []any{}, nil
	}
	*c.remaining--
	return []any{int64(1)}, nil
}

var _ values.Value = (*countdownExplodeValue)(nil)

// naturallyTerminatingLevelUnionPlan builds a level-union recursive CTE whose
// recursive leg inserts exactly one row per level until the level-union has
// made targetDepth recursive-leg STARTS (checkDepth calls), at which point
// the leg inserts nothing and the recursion stops via ordinary fixpoint
// exhaustion (scanTable empty) — NOT via the depth cap.
//
// This pins checkDepth's `>` boundary (recursive_union_cursor.go) exactly:
// checkDepth is called once per recursive-leg start, immediately before that
// leg runs, so the leg that starts at depth==targetDepth is the
// targetDepth-th checkDepth call. With targetDepth ==
// maxStreamingRecursionDepth, that call must SUCCEED (1000 is not >1000) and
// the recursion must complete naturally with no error — an `>=` mutant would
// instead reject that exact call and turn a valid, naturally-terminating
// recursion into a spurious RecursiveCTEDepthExceededError.
func naturallyTerminatingLevelUnionPlan(targetDepth int) *plans.RecordQueryRecursiveLevelUnionPlan {
	scanAlias := values.NamedCorrelationIdentifier("scan")
	insertAlias := values.NamedCorrelationIdentifier("insert")
	initial := plans.NewRecordQueryTempTableInsertPlan(
		plans.NewRecordQueryExplodePlan(&values.ConstantValue{Value: []any{int64(1)}}),
		insertAlias, true,
	)
	// The first targetDepth-1 recursive-leg starts must each insert a row (so
	// each is followed by another checkDepth call); the targetDepth-th must
	// insert nothing (so the recursion stops right there, without a
	// (targetDepth+1)-th checkDepth call).
	remaining := targetDepth - 1
	recursive := plans.NewRecordQueryTempTableInsertPlan(
		plans.NewRecordQueryExplodePlan(&countdownExplodeValue{remaining: &remaining}),
		insertAlias, true,
	)
	return plans.NewRecordQueryRecursiveLevelUnionPlan(initial, recursive, scanAlias, insertAlias)
}

// drainRecursiveUnion pulls every row from cur, returning the row count and
// the first error encountered (nil if the cursor drained to SourceExhausted).
func drainRecursiveUnion(t *testing.T, ctx context.Context, cur recordlayer.RecordCursor[QueryResult]) (int, error) {
	t.Helper()
	n := 0
	for {
		r, err := cur.OnNext(ctx)
		if err != nil {
			return n, err
		}
		if !r.HasNext() {
			if !r.GetNoNextReason().IsSourceExhausted() {
				t.Fatalf("drained to a non-exhaustion stop (reason=%v) after %d rows — unexpected for this fixture", r.GetNoNextReason(), n)
			}
			return n, nil
		}
		n++
	}
}

// TestRecursiveUnionCursor_DepthCap_ExactBoundary_NaturalTerminationSucceeds
// is the exact-boundary pin the depth-cap tests above don't provide: those
// drive an unboundedly cyclic recursion into the cap from above, where ±1 in
// checkDepth's comparison is invisible (the recursion would hit the cap
// either way, just one row earlier or later). This fixture instead
// terminates NATURALLY at EXACTLY maxStreamingRecursionDepth recursive-leg
// starts — a valid recursion a real cyclic-free CTE could produce — and it
// must succeed with no RecursiveCTEDepthExceededError. See
// naturallyTerminatingLevelUnionPlan's doc comment for why targetDepth ==
// maxStreamingRecursionDepth is exactly the boundary checkDepth's `>` (not
// `>=`) must allow through.
func TestRecursiveUnionCursor_DepthCap_ExactBoundary_NaturalTerminationSucceeds(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	cur, err := ExecutePlan(ctx, naturallyTerminatingLevelUnionPlan(maxStreamingRecursionDepth), nil,
		EmptyEvaluationContext(), nil, recordlayer.DefaultExecuteProperties())
	if err != nil {
		t.Fatalf("ExecutePlan: %v", err)
	}
	defer cur.Close()

	rows, err := drainRecursiveUnion(t, ctx, cur)
	if err != nil {
		var depthErr *RecursiveCTEDepthExceededError
		if errors.As(err, &depthErr) {
			t.Fatalf("a recursion that terminates naturally at EXACTLY maxStreamingRecursionDepth (%d) "+
				"recursive-leg starts was rejected as RecursiveCTEDepthExceededError after %d rows — "+
				"checkDepth's boundary check is off by one (rejecting depth==%d instead of only depth>%d)",
				maxStreamingRecursionDepth, rows, maxStreamingRecursionDepth, maxStreamingRecursionDepth)
		}
		t.Fatalf("unexpected error after %d rows: %v", rows, err)
	}
	// 1 initial-leg row + (targetDepth-1) recursive-leg rows, one per
	// non-empty leg start (see naturallyTerminatingLevelUnionPlan).
	wantRows := maxStreamingRecursionDepth
	if rows != wantRows {
		t.Fatalf("got %d rows, want %d", rows, wantRows)
	}
}

// TestRecursiveUnionCursor_DepthCap_OneOverBoundary_Fails is the paired
// control: a recursion whose natural termination would require ONE MORE
// recursive-leg start than the cap allows must still fail with
// RecursiveCTEDepthExceededError. Without this, a test suite could pass a
// cap that always allows one extra level (an off-by-one in the OTHER
// direction) purely because the exact-boundary test above only checks the
// allowed side.
func TestRecursiveUnionCursor_DepthCap_OneOverBoundary_Fails(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	cur, err := ExecutePlan(ctx, naturallyTerminatingLevelUnionPlan(maxStreamingRecursionDepth+1), nil,
		EmptyEvaluationContext(), nil, recordlayer.DefaultExecuteProperties())
	if err != nil {
		t.Fatalf("ExecutePlan: %v", err)
	}
	defer cur.Close()

	rows, err := drainRecursiveUnion(t, ctx, cur)
	var depthErr *RecursiveCTEDepthExceededError
	if !errors.As(err, &depthErr) {
		t.Fatalf("recursion needing %d recursive-leg starts (one more than maxStreamingRecursionDepth=%d) "+
			"drained %d rows with err=%v, want RecursiveCTEDepthExceededError",
			maxStreamingRecursionDepth+1, maxStreamingRecursionDepth, rows, err)
	}
}

// TestRecursiveUnionCursor_ManyShallowInvocations_NoFalsePositive is the
// false-positive regression for keying the statement-scoped depth guard
// (recordlayer.ExecuteState.recursionLevels) by plan pointer ALONE: the SAME
// static plan node, invoked independently many times within one statement —
// e.g. a depth-1 recursive CTE nested in a correlated subquery, re-evaluated
// once per outer row via flatMapCursor.OnNext (flat_map_cursor.go:380 calls
// ExecutePlan with a nil inner continuation for every outer row except the
// one specific row being resumed mid-inner) — must NOT have those
// invocations' levels summed into one shared total. Without
// newRecursiveUnionCursor resetting the count at each fresh (continuation ==
// nil) invocation boundary, invocation 1001 of a plan whose SINGLE
// invocation only ever reaches depth 1 would alone trip
// maxStreamingRecursionDepth even though no single invocation came anywhere
// close — a valid query wrongly rejected.
func TestRecursiveUnionCursor_ManyShallowInvocations_NoFalsePositive(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	// Statement-scoped: minted ONCE and reused across every invocation below,
	// exactly as one real statement execution shares one ExecuteState across
	// every re-invocation of a correlated subquery's plan tree.
	execState := recordlayer.NewExecuteState(0)
	props := recordlayer.DefaultExecuteProperties()
	props.State = execState

	// targetDepth==1: each invocation's initial leg seeds one row, which
	// triggers exactly ONE checkDepth call (the lone recursive-leg start),
	// which immediately inserts nothing and terminates naturally. The SAME
	// plan pointer is reused for every invocation below — required, since
	// the guard keys off c.plan identity.
	plan := naturallyTerminatingLevelUnionPlan(1)

	const invocations = maxStreamingRecursionDepth + 100 // well past the cap if levels wrongly accumulated across invocations
	for i := 1; i <= invocations; i++ {
		cur, err := ExecutePlan(ctx, plan, nil, EmptyEvaluationContext(), nil, props)
		if err != nil {
			t.Fatalf("invocation %d: ExecutePlan: %v", i, err)
		}
		rows, err := drainRecursiveUnion(t, ctx, cur)
		cur.Close()
		if err != nil {
			t.Fatalf("invocation %d of %d (each independently depth-1): got error %v — "+
				"the statement-scoped counter is accumulating ACROSS independent invocations of "+
				"the same plan node instead of resetting at each fresh-invocation boundary",
				i, invocations, err)
		}
		if rows != 1 {
			t.Fatalf("invocation %d: got %d rows, want 1", i, rows)
		}
	}
}

// neverTerminatingDfsPlan builds a streaming (UNION ALL, non-DISTINCT)
// recursive DFS join whose child leg unconditionally emits exactly one row
// per node, regardless of the parent — every node has exactly one child,
// forever, so the traversal has no natural leaf and the ONLY thing that can
// stop it is the depth cap in executeRecursiveDfsJoinStreaming's childFn.
func neverTerminatingDfsPlan() *plans.RecordQueryRecursiveDfsJoinPlan {
	node := func() *plans.RecordQueryValuesPlan {
		return plans.NewRecordQueryValuesPlan([]values.Value{
			&values.ConstantValue{Value: int64(1), Typ: values.NewPrimitiveType(values.TypeCodeLong, false)},
		})
	}
	prior := values.NamedCorrelationIdentifier("prior")
	return plans.NewRecordQueryRecursiveDfsJoinPlan(node(), node(), prior, plans.DfsPreorder)
}

// TestRecursiveDfsJoinStreaming_HitsDepthCap is the DFS-arm half of the
// "either arm" non-paging depth-cap coverage: executeRecursiveDfsJoinStreaming
// (the sibling that already survives paging correctly — its depth is the
// TRUE reconstructed stack depth from the continuation's level list, not a
// side counter, see recursive_cursor.go) still needs its OWN cap check
// pinned, since nothing exercised RecursiveCTEDepthExceededError for this arm
// either. A never-terminating one-child-per-node traversal must surface the
// depth error instead of recursing forever.
func TestRecursiveDfsJoinStreaming_HitsDepthCap(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	cur, err := ExecutePlan(ctx, neverTerminatingDfsPlan(), nil, EmptyEvaluationContext(), nil, recordlayer.DefaultExecuteProperties())
	if err != nil {
		t.Fatalf("ExecutePlan: %v", err)
	}
	defer cur.Close()

	const ceiling = 2000 // > maxStreamingRecursionDepth(1000)
	var depthErr *RecursiveCTEDepthExceededError
	for n := 1; ; n++ {
		if n > ceiling {
			t.Fatalf("cyclic DFS recursion did not hit the depth cap within %d rows — RecursiveCTEDepthExceededError never fired", ceiling)
		}
		_, err := cur.OnNext(ctx)
		if err != nil {
			if !errors.As(err, &depthErr) {
				t.Fatalf("row %d: got error %v, want RecursiveCTEDepthExceededError", n, err)
			}
			return
		}
	}
}
