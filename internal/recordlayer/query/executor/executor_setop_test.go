package executor

import (
	"context"
	"errors"
	"strings"
	"testing"

	cursorpb "fdb.dev/internal/pb/cursor"
	"fdb.dev/internal/recordlayer/query/plan/cascades/values"
	"fdb.dev/internal/recordlayer/query/plan/plans"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer"
	"google.golang.org/protobuf/proto"
)

func TestExecuteUnion_ConcatenatesInners(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	a := mustExecutorConstruct(plans.NewRecordQueryExplodePlan(&values.ConstantValue{
		Value: []any{int64(1)},
		Typ:   values.NewArrayType(false, values.NullableLong),
	}))
	b := mustExecutorConstruct(plans.NewRecordQueryExplodePlan(&values.ConstantValue{
		Value: []any{int64(2)},
		Typ:   values.NewArrayType(false, values.NullableLong),
	}))
	union := mustExecutorConstruct(plans.NewRecordQueryUnionPlan([]plans.RecordQueryPlan{a, b}))

	cursor, err := ExecutePlan(ctx, union, nil, EmptyEvaluationContext(), nil, recordlayer.DefaultExecuteProperties())
	if err != nil {
		t.Fatalf("ExecutePlan: %v", err)
	}
	defer cursor.Close()

	results, err := CollectAll(ctx, cursor)
	if err != nil {
		t.Fatalf("CollectAll: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("got %d results, want 2 (one from each inner)", len(results))
	}
}

func TestExecuteUnion_EmptyRejectedAtConstruction(t *testing.T) {
	t.Parallel()

	union, err := plans.NewRecordQueryUnionPlan(nil)
	if err == nil {
		t.Fatal("empty union construction unexpectedly succeeded")
	}
	if union != nil {
		t.Fatalf("empty union returned partial plan %#v alongside error %v", union, err)
	}
	if got := err.Error(); !strings.Contains(got, "at least one input quantifier is required") {
		t.Fatalf("empty union error = %q, want missing-input diagnostic", got)
	}
}

func TestUnionContinuationEmptyInputAdapter(t *testing.T) {
	t.Parallel()
	states, err := decodeUnionContinuation([]byte{}, 3)
	if err != nil || len(states) != 3 {
		t.Fatalf("executor empty input must start three children: %v", err)
	}
	for i, state := range states {
		if state.exhausted || state.continuation != nil {
			t.Fatalf("child %d is not START: %+v", i, state)
		}
	}
	if _, err := decodeUnionContinuation([]byte{0xff}, 3); err == nil {
		t.Fatal("accepted malformed union continuation")
	} else {
		var parse *recordlayer.ContinuationParseError
		if !errors.As(err, &parse) {
			t.Fatalf("lost executor parse-error policy: %T %v", err, err)
		}
	}
}

func TestUnorderedUnionCursor_MultipleChildren(t *testing.T) {
	t.Parallel()

	c1 := recordlayer.FromList([]QueryResult{
		qr("id", int64(1)),
		qr("id", int64(2)),
	})
	c2 := recordlayer.FromList([]QueryResult{
		qr("id", int64(3)),
	})
	c3 := recordlayer.FromList([]QueryResult{
		qr("id", int64(4)),
		qr("id", int64(5)),
	})

	cc := newUnorderedUnionForTest(c1, c2, c3)
	defer cc.Close()

	results := collectCursor(t, cc)
	if len(results) != 5 {
		t.Fatalf("got %d results, want 5", len(results))
	}
	for i, want := range []int64{1, 2, 3, 4, 5} {
		got := fieldVal(t, results[i], "id")
		if got != want {
			t.Errorf("results[%d].id = %d, want %d", i, got, want)
		}
	}
}

func TestUnorderedUnionCursor_EmptyFirst(t *testing.T) {
	t.Parallel()

	empty := recordlayer.FromList([]QueryResult{})
	nonempty := recordlayer.FromList([]QueryResult{
		qr("id", int64(7)),
		qr("id", int64(8)),
	})

	cc := newUnorderedUnionForTest(empty, nonempty)
	defer cc.Close()

	results := collectCursor(t, cc)
	if len(results) != 2 {
		t.Fatalf("got %d results, want 2", len(results))
	}
	if fieldVal(t, results[0], "id") != 7 {
		t.Errorf("results[0].id = %d, want 7", fieldVal(t, results[0], "id"))
	}
}

func TestUnorderedUnionCursor_AllEmpty(t *testing.T) {
	t.Parallel()

	cc := newUnorderedUnionForTest(
		recordlayer.FromList([]QueryResult{}),
		recordlayer.FromList([]QueryResult{}),
	)
	defer cc.Close()

	if results := collectCursor(t, cc); len(results) != 0 {
		t.Fatalf("got %d results, want 0", len(results))
	}
}

func TestUnorderedUnionCursor_CloseIdempotent(t *testing.T) {
	t.Parallel()

	cc := newUnorderedUnionForTest(
		recordlayer.FromList([]QueryResult{qr("id", int64(1))}),
		recordlayer.FromList([]QueryResult{qr("id", int64(2))}),
	)

	if cc.IsClosed() {
		t.Fatal("should not be closed initially")
	}
	if err := cc.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if !cc.IsClosed() {
		t.Fatal("should be closed after Close()")
	}
	if err := cc.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

// TestExecuteUnorderedUnion_SingleChildKeepsSkipLimit pins the review-caught
// hole: the single-child degenerate path cleared skip/limit for the child
// (correct — pagination composes above) but never reapplied them, silently
// returning every row of a LIMITed single-branch union.
func TestExecuteUnorderedUnion_SingleChildKeepsSkipLimit(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	plan := mustExecutorConstruct(plans.NewRecordQueryUnorderedUnionPlan([]plans.RecordQueryPlan{
		mustExecutorConstruct(plans.NewRecordQueryValuesPlan([]values.Value{
			&values.ConstantValue{Value: int64(7), Typ: values.NewPrimitiveType(values.TypeCodeInt, false)},
		})),
	}))
	props := recordlayer.DefaultExecuteProperties().WithSkip(1)
	cursor, err := ExecutePlan(ctx, plan, nil, EmptyEvaluationContext(), nil, props)
	if err != nil {
		t.Fatalf("ExecutePlan: %v", err)
	}
	defer cursor.Close()
	rows, err := CollectAll(ctx, cursor)
	if err != nil {
		t.Fatalf("CollectAll: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("skip=1 over a 1-row single-child union must yield 0 rows, got %d", len(rows))
	}
}

// TestUnorderedUnionCursor_MidStreamSnapshotWithUnpulledChild pins the
// review-caught panic: the FIRST row's continuation snapshots child 1 before
// it was ever pulled — the slot must encode START (Java's START_PROTO), never
// a nil interface the encoder's IsEnd probe dereferences. Reachable via
// LIMIT/maxRows paging that ends inside child 0.
func TestUnorderedUnionCursor_MidStreamSnapshotWithUnpulledChild(t *testing.T) {
	t.Parallel()
	cc := newUnorderedUnionForTest(
		recordlayer.FromList([]QueryResult{qr("id", int64(1)), qr("id", int64(2))}),
		recordlayer.FromList([]QueryResult{qr("id", int64(9))}),
	)
	defer cc.Close()
	res, err := cc.OnNext(context.Background())
	if err != nil || !res.HasNext() {
		t.Fatalf("first row: %v", err)
	}
	b, berr := res.GetContinuation().ToBytes()
	if berr != nil {
		t.Fatalf("first-row continuation must encode (unpulled child = START): %v", berr)
	}
	slots, derr := decodeUnionContinuation(b, 2)
	if derr != nil {
		t.Fatalf("decode: %v", derr)
	}
	if slots[0].exhausted || len(slots[0].continuation) == 0 {
		t.Fatalf("emitting child slot = %+v, want its position", slots[0])
	}
	if slots[1].exhausted || len(slots[1].continuation) != 0 {
		t.Fatalf("unpulled child slot = %+v, want START", slots[1])
	}
}

// TestUnorderedUnionCursor_StopSlotsAndResume pins the Java
// UnorderedUnionCursor contract the old eager concat declined (RFC-180 A2 →
// E3/E4): a limit-stopped child parks while the others keep emitting; the
// terminal carries the STRONGEST child reason and per-child UnionContinuation
// slots (exhausted children marked, stopped children at their positions);
// and a re-call replays the cached terminal (the flaky child would surface
// MORE rows without the guard).
func TestUnorderedUnionCursor_StopSlotsAndResume(t *testing.T) {
	t.Parallel()
	rows := nljTestRows("K", 2)
	flaky := &flakyOuterCursor{first: rows[:1], rest: rows[1:]} // 1 row, pause, then MORE
	done := recordlayer.FromList([]QueryResult{qr("id", int64(9))})
	cc := newUnorderedUnionForTest(flaky, done)
	defer cc.Close()

	var emitted int
	var terminal recordlayer.RecordCursorResult[QueryResult]
	ctx := context.Background()
	for {
		res, err := cc.OnNext(ctx)
		if err != nil {
			t.Fatalf("OnNext: %v", err)
		}
		if !res.HasNext() {
			terminal = res
			break
		}
		emitted++
	}
	// Child 0's row + child 1's row: the pause parks child 0, child 1 keeps
	// emitting (Java: a limited child does not stop the union).
	if emitted != 2 {
		t.Fatalf("emitted %d rows, want 2 (the stopped child must not stop the union)", emitted)
	}
	if terminal.GetNoNextReason() != recordlayer.TimeLimitReached {
		t.Fatalf("terminal reason = %v, want the stopped child's TimeLimitReached (strongest)", terminal.GetNoNextReason())
	}
	b, berr := terminal.GetContinuation().ToBytes()
	if berr != nil {
		t.Fatalf("ToBytes: %v", berr)
	}
	slots, derr := decodeUnionContinuation(b, 2)
	if derr != nil {
		t.Fatalf("decode: %v", derr)
	}
	if slots[0].exhausted || len(slots[0].continuation) == 0 {
		t.Fatalf("stopped child slot = %+v, want its resume position", slots[0])
	}
	if !slots[1].exhausted {
		t.Fatalf("exhausted child slot = %+v, want the exhausted marker", slots[1])
	}

	// Terminal replay: without the cache, the flaky child's next pull would
	// surface its post-pause rows.
	again, err := cc.OnNext(ctx)
	if err != nil {
		t.Fatalf("re-call: %v", err)
	}
	ab, _ := again.GetContinuation().ToBytes()
	if again.HasNext() || again.GetNoNextReason() != terminal.GetNoNextReason() || string(ab) != string(b) {
		t.Fatal("re-call must replay the cached terminal verbatim")
	}
}

// TestExecuteUnorderedUnion_ResumeContract pins the resumable unordered
// union (Java UnorderedUnionCursor parity — this replaced the RFC-180 A2 loud
// decline, which itself replaced feeding the parent token verbatim to every
// child): a VALID per-child UnionContinuation resumes (exhausted children
// skipped), while unrecognized bytes still decline loudly as a parse error —
// never reach a child raw.
func TestExecuteUnorderedUnion_ResumeContract(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	mkValues := func(v int64) plans.RecordQueryPlan {
		literal := &values.ConstantValue{
			Value: v,
			Typ:   values.NewPrimitiveType(values.TypeCodeInt, false),
		}
		input := mustExecutorConstruct(plans.NewRecordQueryValuesPlan([]values.Value{literal}))
		return mustExecutorConstruct(newProjectionMapForTest(
			[]values.Value{literal}, []string{"V"}, input,
		))
	}
	plan := mustExecutorConstruct(plans.NewRecordQueryUnorderedUnionPlan([]plans.RecordQueryPlan{mkValues(1), mkValues(2)}))

	// Fresh start (nil continuation) executes and yields both branches' rows.
	cursor, err := ExecutePlan(ctx, plan, nil, EmptyEvaluationContext(), nil, recordlayer.DefaultExecuteProperties())
	if err != nil {
		t.Fatalf("fresh ExecutePlan: %v", err)
	}
	rows, err := CollectAll(ctx, cursor)
	cursor.Close()
	if err != nil {
		t.Fatalf("CollectAll: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("fresh unordered union yielded %d rows, want 2", len(rows))
	}

	// A valid token marking child 0 exhausted resumes: only child 1 emits.
	tok, terr := proto.Marshal(&cursorpb.UnionContinuation{FirstExhausted: proto.Bool(true)})
	if terr != nil {
		t.Fatalf("marshal: %v", terr)
	}
	cursor, err = ExecutePlan(ctx, plan, nil, EmptyEvaluationContext(), tok, recordlayer.DefaultExecuteProperties())
	if err != nil {
		t.Fatalf("valid resume: %v", err)
	}
	rows, err = CollectAll(ctx, cursor)
	cursor.Close()
	if err != nil {
		t.Fatalf("resumed CollectAll: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("resume with child 0 exhausted yielded %d rows, want 1 (child 1 only)", len(rows))
	}

	// Unrecognized bytes: loud parse error, never fed to a child raw.
	_, err = ExecutePlan(ctx, plan, nil, EmptyEvaluationContext(), []byte("parent-token"), recordlayer.DefaultExecuteProperties())
	var pErr *recordlayer.ContinuationParseError
	if !errors.As(err, &pErr) {
		t.Fatalf("garbage resume: err = %v, want *ContinuationParseError (a nil error means the token was misrouted to the children)", err)
	}
}

func TestExecuteIntersection_CommonRows(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	a := mustExecutorConstruct(plans.NewRecordQueryValuesPlan([]values.Value{
		&values.ConstantValue{Value: int64(42), Typ: values.NewPrimitiveType(values.TypeCodeInt, false)},
	}))
	b := mustExecutorConstruct(plans.NewRecordQueryValuesPlan([]values.Value{
		&values.ConstantValue{Value: int64(42), Typ: values.NewPrimitiveType(values.TypeCodeInt, false)},
	}))
	intersection := mustExecutorConstruct(plans.NewRecordQueryIntersectionPlan([]plans.RecordQueryPlan{a, b}, nil))

	cursor, err := ExecutePlan(ctx, intersection, nil, EmptyEvaluationContext(), nil, recordlayer.DefaultExecuteProperties())
	if err != nil {
		t.Fatalf("ExecutePlan: %v", err)
	}
	defer cursor.Close()

	results, err := CollectAll(ctx, cursor)
	if err != nil {
		t.Fatalf("CollectAll: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("got %d results, want 1 (common row)", len(results))
	}
}

func TestExecuteIntersection_NoCommonRows(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	mkBranch := func(v int64) plans.RecordQueryPlan {
		literal := &values.ConstantValue{
			Value: v,
			Typ:   values.NewPrimitiveType(values.TypeCodeInt, false),
		}
		input := mustExecutorConstruct(plans.NewRecordQueryValuesPlan([]values.Value{literal}))
		return mustExecutorConstruct(newProjectionMapForTest(
			[]values.Value{literal}, []string{"V"}, input,
		))
	}
	a := mkBranch(1)
	b := mkBranch(2)
	intersection := mustExecutorConstruct(plans.NewRecordQueryIntersectionPlan([]plans.RecordQueryPlan{a, b}, nil))

	cursor, err := ExecutePlan(ctx, intersection, nil, EmptyEvaluationContext(), nil, recordlayer.DefaultExecuteProperties())
	if err != nil {
		t.Fatalf("ExecutePlan: %v", err)
	}
	defer cursor.Close()

	results, err := CollectAll(ctx, cursor)
	if err != nil {
		t.Fatalf("CollectAll: %v", err)
	}
	if len(results) != 0 {
		t.Fatalf("got %d results, want 0 (no common rows)", len(results))
	}
}

// --- intersectionCompKeyFunc unit tests ---

func TestIntersectionCompKeyFunc_NoKeyVals_WithPK(t *testing.T) {
	t.Parallel()
	pk := tuple.Tuple{int64(7)}
	qr := dmapPK(pk, map[string]any{"X": 1})
	fn := intersectionCompKeyFunc(nil)
	got, err := fn(qr)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 1 || got[0] != int64(7) {
		t.Fatalf("expected PK tuple {7}, got %v", got)
	}
}

func TestIntersectionCompKeyFunc_NoKeyVals_NoPK(t *testing.T) {
	t.Parallel()
	qr := dmap(map[string]any{"X": 1})
	fn := intersectionCompKeyFunc(nil)
	got, err := fn(qr)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected single-element tuple, got %v", got)
	}
	// RFC-180 C4: the keyless/PK-less fallback matches rows by their FULL
	// positional content via the lossless continuation codec ([]byte), not
	// a rendered string (which collapsed distinct composite rows).
	if _, ok := got[0].([]byte); !ok {
		t.Fatalf("expected lossless []byte element, got %T", got[0])
	}
	other, err := fn(dmap(map[string]any{"X": 2}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(got[0].([]byte)) == string(other[0].([]byte)) {
		t.Fatal("distinct rows must produce distinct fallback comparison keys")
	}
}

func TestIntersectionCompKeyFunc_WithKeyVals(t *testing.T) {
	t.Parallel()
	qr := dmap(map[string]any{"NAME": "alice", "AGE": int64(30)})
	rowType := exactTestRowType(
		values.Field{Name: "AGE", FieldType: values.NullableLong},
		values.Field{Name: "NAME", FieldType: values.TypeString},
	)
	root := mustTestQOV(t, values.UniqueCorrelationIdentifier(), rowType)
	keyVals := []values.Value{
		mustTestFieldOrdinal(t, root, 1),
		mustTestFieldOrdinal(t, root, 0),
	}
	fn := intersectionCompKeyFunc(keyVals)
	got, err := fn(qr)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 2 || got[0] != "alice" || got[1] != int64(30) {
		t.Fatalf("expected {alice, 30}, got %v", got)
	}
}
