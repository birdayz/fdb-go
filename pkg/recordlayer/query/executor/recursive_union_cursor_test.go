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
