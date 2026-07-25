package executor

import (
	"context"
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
