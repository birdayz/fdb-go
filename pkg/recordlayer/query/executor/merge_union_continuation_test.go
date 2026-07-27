package executor

// RFC-180 A5 — unit pins for the mergeSortCursor's per-child continuation
// states (Java MergeCursor / UnionCursor architecture):
//
//   - Every EMITTED row's continuation is a real, lazily-encoded
//     RecordCursorProto.UnionContinuation snapshotting ALL children's cached
//     positions (Java MergeCursorContinuation via MergeCursor.onNext →
//     getContinuationObject). Before A5 emitted rows carried
//     &StartContinuation{} — nil bytes, no resume.
//   - A child's cached continuation advances ONLY when its row is CONSUMED
//     into an emitted merge row (Java MergeCursorState.consume, :80) or when
//     the child stops (handleNextCursorResult, :61). A peeked-but-unconsumed
//     row does NOT advance its child's state — it must be re-read on resume.
//   - Dedup is Java UnionCursor's per-step advance-all-equal
//     (UnionCursor.java:112-143): every child whose comparison key equals the
//     chosen minimum is consumed for ONE emitted row. No cross-page lastKey.
//   - A child stopping for ANY limit (in-band ReturnLimitReached included)
//     stops the whole merge with the snapshot continuation
//     (UnionCursor.computeNextResultStates :81-84 + MergeCursor
//     getStrongestNoNextReason). Before A5 an in-band child stop was treated
//     as EXHAUSTION — silently dropping the child's remaining rows.

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"fdb.dev/gen"
	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// idKey is the single comparison key (ordinal 0, "id") the merge pins use.
func idKey() []values.Value {
	return []values.Value{values.NewFieldValueWithResolvedOrdinal("id", 0, values.NullableLong)}
}

// mustBytes renders a continuation to bytes, failing the test on error.
func mustBytes(t *testing.T, c recordlayer.RecordCursorContinuation) []byte {
	t.Helper()
	if c == nil {
		t.Fatal("nil continuation")
	}
	b, err := c.ToBytes()
	if err != nil {
		t.Fatalf("continuation ToBytes: %v", err)
	}
	return b
}

// decodeUnionProto unmarshals emitted merge-continuation bytes as the
// wire-format UnionContinuation proto.
func decodeUnionProto(t *testing.T, b []byte) *gen.UnionContinuation {
	t.Helper()
	if len(b) == 0 {
		t.Fatal("emitted merge row carries EMPTY continuation bytes (the pre-A5 StartContinuation fake) — no resume possible")
	}
	msg := &gen.UnionContinuation{}
	if err := msg.UnmarshalVT(b); err != nil {
		t.Fatalf("emitted merge continuation does not decode as UnionContinuation: %v", err)
	}
	return msg
}

// listFactory builds a child factory over FromListWithContinuation — the
// resumable list cursor (4-byte big-endian position continuation).
func listFactory(rows []QueryResult) recordlayer.CursorFactory[QueryResult] {
	return func(cont []byte) recordlayer.RecordCursor[QueryResult] {
		return recordlayer.FromListWithContinuation(rows, cont)
	}
}

// drainMergeIDs drains a merge cursor, returning emitted ids and the final
// no-next result.
func drainMergeIDs(t *testing.T, c *mergeSortCursor) ([]int64, recordlayer.RecordCursorResult[QueryResult]) {
	t.Helper()
	ctx := context.Background()
	var out []int64
	for {
		r, err := c.OnNext(ctx)
		if err != nil {
			t.Fatalf("OnNext: %v", err)
		}
		if !r.HasNext() {
			return out, r
		}
		out = append(out, fieldVal(t, r.GetValue(), "id"))
	}
}

// TestMergeSortCursor_EmittedRowContinuation_PeekedChildDoesNotAdvance pins
// the emitted-row snapshot: after the first emit (left's id=1 consumed, right's
// id=2 PEEKED but unconsumed), the continuation encodes
//   - first child MID at the consumed row's position (list pos 1);
//   - second child START (absent continuation, exhausted=false) — Java
//     MergeCursorState.java:61/:80: a peeked-but-unconsumed row does NOT
//     advance its child's cached continuation.
func TestMergeSortCursor_EmittedRowContinuation_PeekedChildDoesNotAdvance(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	left := recordlayer.FromList([]QueryResult{qr("id", int64(1)), qr("id", int64(3))})
	right := recordlayer.FromList([]QueryResult{qr("id", int64(2)), qr("id", int64(4))})
	c := newMergeSortCursor([]recordlayer.RecordCursor[QueryResult]{left, right}, idKey(), false, true)
	defer c.Close()

	r, err := c.OnNext(ctx)
	if err != nil {
		t.Fatalf("OnNext: %v", err)
	}
	if !r.HasNext() || fieldVal(t, r.GetValue(), "id") != 1 {
		t.Fatalf("first merge row = %+v, want id=1", r)
	}

	msg := decodeUnionProto(t, mustBytes(t, r.GetContinuation()))
	if want := listPosContinuation(1); string(msg.GetFirstContinuation()) != string(want) {
		t.Fatalf("first (consumed) child continuation = %v, want %v (position after its consumed row)",
			msg.GetFirstContinuation(), want)
	}
	if msg.GetFirstExhausted() {
		t.Fatal("first child wrongly marked exhausted")
	}
	// The PEEKED child: START — no continuation bytes, not exhausted. Encoding
	// its post-peek position here would SKIP the peeked row (id=2) on resume.
	if msg.SecondContinuation != nil {
		t.Fatalf("second (peeked, unconsumed) child carries continuation %v — its state must NOT advance past the peeked row",
			msg.SecondContinuation)
	}
	if msg.GetSecondExhausted() {
		t.Fatal("second child wrongly marked exhausted")
	}
}

// TestMergeSortCursor_ExhaustedChildFlags pins the exhausted encoding: once a
// child reports SourceExhausted, every later continuation marks it
// exhausted=true (Java UnionCursorContinuation.setFirstChild/addOtherChild END
// arm), and a resume from such a continuation NEVER re-opens that child
// (Java MergeCursorState.from: END → RecordCursor.empty()).
func TestMergeSortCursor_ExhaustedChildFlags(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	left := recordlayer.FromList([]QueryResult{qr("id", int64(1))})
	right := recordlayer.FromList([]QueryResult{qr("id", int64(2)), qr("id", int64(3))})
	c := newMergeSortCursor([]recordlayer.RecordCursor[QueryResult]{left, right}, idKey(), false, true)
	defer c.Close()

	// Emit 1 (left consumed), then 2: pulling left again exhausts it, so row
	// 2's continuation must carry first_exhausted=true.
	var second recordlayer.RecordCursorResult[QueryResult]
	for i := 0; i < 2; i++ {
		r, err := c.OnNext(ctx)
		if err != nil {
			t.Fatalf("OnNext %d: %v", i, err)
		}
		if !r.HasNext() {
			t.Fatalf("OnNext %d: no row", i)
		}
		second = r
	}
	if got := fieldVal(t, second.GetValue(), "id"); got != 2 {
		t.Fatalf("second row id = %d, want 2", got)
	}
	msg := decodeUnionProto(t, mustBytes(t, second.GetContinuation()))
	if !msg.GetFirstExhausted() {
		t.Fatal("first child exhausted mid-merge must encode first_exhausted=true")
	}
	if msg.GetSecondContinuation() == nil {
		t.Fatal("second (consumed) child must carry its resume position")
	}

	// Resume: the exhausted child's factory must NOT be re-invoked.
	poisoned := func(cont []byte) recordlayer.RecordCursor[QueryResult] {
		return &errResultCursor{err: errors.New("factory for an EXHAUSTED child was invoked on resume (must be an empty cursor, Java MergeCursorState.from)")}
	}
	resumed, err := newMergeSortCursorFromFactories(
		[]recordlayer.CursorFactory[QueryResult]{
			poisoned,
			listFactory([]QueryResult{qr("id", int64(2)), qr("id", int64(3))}),
		},
		idKey(), false, true, mustBytes(t, second.GetContinuation()))
	if err != nil {
		t.Fatalf("resume construction: %v", err)
	}
	defer resumed.Close()
	ids, final := drainMergeIDs(t, resumed)
	if fmt.Sprint(ids) != "[3]" {
		t.Fatalf("resume after id=2 = %v, want [3]", ids)
	}
	if !final.GetNoNextReason().IsSourceExhausted() || !final.GetContinuation().IsEnd() {
		t.Fatalf("final = (%v, end=%v), want (SourceExhausted, END)", final.GetNoNextReason(), final.GetContinuation().IsEnd())
	}
}

// TestMergeSortCursor_DedupAdvanceAllEqual_ResumeMidTie pins Java's
// advance-all-equal dedup with NO cross-page state: left=[1,2], right=[2,3],
// dedup → [1,2,3]. Resuming from EVERY emitted row's continuation yields
// exactly the remaining rows — the resume that lands mid-tie (both children
// positioned AT their id=2 rows) must emit 2 exactly once (both consumed in
// one step), never duplicate it and never drop it.
func TestMergeSortCursor_DedupAdvanceAllEqual_ResumeMidTie(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	leftRows := []QueryResult{qr("id", int64(1)), qr("id", int64(2))}
	rightRows := []QueryResult{qr("id", int64(2)), qr("id", int64(3))}
	factories := []recordlayer.CursorFactory[QueryResult]{listFactory(leftRows), listFactory(rightRows)}

	fresh, err := newMergeSortCursorFromFactories(factories, idKey(), false, true, nil)
	if err != nil {
		t.Fatalf("construction: %v", err)
	}
	defer fresh.Close()

	want := []int64{1, 2, 3}
	var rowConts [][]byte
	for i, w := range want {
		r, oErr := fresh.OnNext(ctx)
		if oErr != nil {
			t.Fatalf("OnNext %d: %v", i, oErr)
		}
		if !r.HasNext() || fieldVal(t, r.GetValue(), "id") != w {
			t.Fatalf("row %d = %+v, want id=%d", i, r, w)
		}
		rowConts = append(rowConts, mustBytes(t, r.GetContinuation()))
	}

	// Resume from each row's continuation: suffix equality, no dup, no drop.
	for i, cont := range rowConts {
		resumed, rErr := newMergeSortCursorFromFactories(factories, idKey(), false, true, cont)
		if rErr != nil {
			t.Fatalf("resume %d construction: %v", i, rErr)
		}
		ids, final := drainMergeIDs(t, resumed)
		resumed.Close()
		if fmt.Sprint(ids) != fmt.Sprint(want[i+1:]) {
			t.Fatalf("resume from row %d (id=%d) = %v, want %v (mid-tie resume must not duplicate or drop)",
				i, want[i], ids, want[i+1:])
		}
		if !final.GetNoNextReason().IsSourceExhausted() {
			t.Fatalf("resume %d final reason = %v, want SourceExhausted", i, final.GetNoNextReason())
		}
	}
}

// TestMergeSortCursor_NoDedup_TiedRowsBothEmitted pins the dedup=false (UNION
// ALL merge) consumption rule: only the FIRST minimum child is consumed per
// step, so a cross-child tie emits BOTH rows — and a resume from the first
// tied row's continuation re-emits exactly the second one (the peeked tied
// child never advanced).
func TestMergeSortCursor_NoDedup_TiedRowsBothEmitted(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	leftRows := []QueryResult{qr("id", int64(2))}
	rightRows := []QueryResult{qr("id", int64(2)), qr("id", int64(5))}
	factories := []recordlayer.CursorFactory[QueryResult]{listFactory(leftRows), listFactory(rightRows)}

	c, err := newMergeSortCursorFromFactories(factories, idKey(), false, false, nil)
	if err != nil {
		t.Fatalf("construction: %v", err)
	}
	defer c.Close()

	r1, err := c.OnNext(ctx)
	if err != nil {
		t.Fatalf("OnNext: %v", err)
	}
	if !r1.HasNext() || fieldVal(t, r1.GetValue(), "id") != 2 {
		t.Fatalf("first row = %+v, want id=2", r1)
	}
	rest, _ := drainMergeIDs(t, c)
	if fmt.Sprint(rest) != "[2 5]" {
		t.Fatalf("UNION ALL merge after first tied row = %v, want [2 5] (both tied rows emitted)", rest)
	}

	resumed, err := newMergeSortCursorFromFactories(factories, idKey(), false, false, mustBytes(t, r1.GetContinuation()))
	if err != nil {
		t.Fatalf("resume construction: %v", err)
	}
	defer resumed.Close()
	ids, _ := drainMergeIDs(t, resumed)
	if fmt.Sprint(ids) != "[2 5]" {
		t.Fatalf("resume from first tied row = %v, want [2 5] (the unconsumed tied row must be re-read)", ids)
	}
}

// TestMergeSortCursor_InBandChildStop_PropagatesWithSnapshot pins the limit
// handling (UnionCursor.computeNextResultStates :81-84): a child stopping
// IN-BAND (ReturnLimitReached) stops the WHOLE merge with that reason and the
// snapshot continuation — never "child exhausted, keep merging the others",
// which silently drops the limited child's remaining rows. The resumed merge
// (fresh, unlimited children from the snapshot) yields exactly the rest.
func TestMergeSortCursor_InBandChildStop_PropagatesWithSnapshot(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	leftRows := []QueryResult{qr("id", int64(1)), qr("id", int64(3))}
	rightRows := []QueryResult{qr("id", int64(2))}

	// Left is row-limited to 1: it emits id=1 then stops IN-BAND.
	limited := recordlayer.LimitRowsCursor(recordlayer.FromList(leftRows), 1)
	c := newMergeSortCursor(
		[]recordlayer.RecordCursor[QueryResult]{limited, recordlayer.FromList(rightRows)},
		idKey(), false, true)
	defer c.Close()

	r1, err := c.OnNext(ctx)
	if err != nil {
		t.Fatalf("OnNext: %v", err)
	}
	if !r1.HasNext() || fieldVal(t, r1.GetValue(), "id") != 1 {
		t.Fatalf("first row = %+v, want id=1", r1)
	}

	r2, err := c.OnNext(ctx)
	if err != nil {
		t.Fatalf("OnNext 2: %v", err)
	}
	if r2.HasNext() {
		t.Fatalf("after the left child's IN-BAND limit stop the merge emitted id=%d — the limited child was treated as EXHAUSTED (its remaining rows silently dropped)",
			fieldVal(t, r2.GetValue(), "id"))
	}
	if got := r2.GetNoNextReason(); got != recordlayer.ReturnLimitReached {
		t.Fatalf("merge stop reason = %v, want ReturnLimitReached (the strongest child reason)", got)
	}

	// A repeated OnNext returns the same cached stop (Java MergeCursor.onNext:291).
	r3, err := c.OnNext(ctx)
	if err != nil {
		t.Fatalf("OnNext 3: %v", err)
	}
	if r3.HasNext() || r3.GetNoNextReason() != recordlayer.ReturnLimitReached {
		t.Fatalf("repeated OnNext after stop = %+v, want the cached ReturnLimitReached stop", r3)
	}

	// Resume from the stop continuation with UNLIMITED children: left resumes
	// after its consumed id=1 → [3]; right was PEEKED only → re-reads id=2.
	resumed, err := newMergeSortCursorFromFactories(
		[]recordlayer.CursorFactory[QueryResult]{listFactory(leftRows), listFactory(rightRows)},
		idKey(), false, true, mustBytes(t, r2.GetContinuation()))
	if err != nil {
		t.Fatalf("resume construction: %v", err)
	}
	defer resumed.Close()
	ids, _ := drainMergeIDs(t, resumed)
	if fmt.Sprint(ids) != "[2 3]" {
		t.Fatalf("resume after in-band stop = %v, want [2 3] (paged 1+%v must equal unpaged [1 2 3])", ids, ids)
	}
}

// TestMergeSortCursor_OutOfBandChildStop_StrongestReason pins
// getStrongestNoNextReason: with one child stopped out-of-band
// (TimeLimitReached) the merge's stop reason is out-of-band even when another
// child stopped in-band, and the snapshot continuation still resumes.
func TestMergeSortCursor_OutOfBandChildStop_StrongestReason(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	stopped := &stubStopCursor{reason: recordlayer.TimeLimitReached, cont: recordlayer.NewBytesContinuation([]byte("LPOS"))}
	right := recordlayer.FromList([]QueryResult{qr("id", int64(2))})
	c := newMergeSortCursor([]recordlayer.RecordCursor[QueryResult]{stopped, right}, idKey(), false, true)
	defer c.Close()

	r, err := c.OnNext(ctx)
	if err != nil {
		t.Fatalf("OnNext: %v", err)
	}
	if r.HasNext() {
		t.Fatalf("merge emitted %+v past an out-of-band child stop — wrong merge order risk", r.GetValue())
	}
	if got := r.GetNoNextReason(); got != recordlayer.TimeLimitReached {
		t.Fatalf("stop reason = %v, want TimeLimitReached (out-of-band is strongest)", got)
	}
	msg := decodeUnionProto(t, mustBytes(t, r.GetContinuation()))
	if string(msg.GetFirstContinuation()) != "LPOS" {
		t.Fatalf("stopped child's snapshot = %v, want its own continuation bytes", msg.GetFirstContinuation())
	}
	if msg.SecondContinuation != nil || msg.GetSecondExhausted() {
		t.Fatalf("peeked child must stay START in the stop snapshot, got %+v", msg)
	}
}

// TestMergeSortCursor_ContinuationDecodeErrors pins the decode guards: corrupt
// bytes fail as ContinuationParseError (Java UnionCursorContinuation.from's
// RecordCoreException) and a child-count mismatch fails loudly (Java's
// RecordCoreArgumentException) — never a silent fresh restart.
func TestMergeSortCursor_ContinuationDecodeErrors(t *testing.T) {
	t.Parallel()

	factories := []recordlayer.CursorFactory[QueryResult]{
		listFactory([]QueryResult{qr("id", int64(1))}),
		listFactory([]QueryResult{qr("id", int64(2))}),
	}

	if _, err := newMergeSortCursorFromFactories(factories, idKey(), false, true, []byte{0xFF, 0xFF, 0xFF}); err == nil {
		t.Fatal("corrupt continuation must fail construction, not silently restart")
	} else {
		var parseErr *recordlayer.ContinuationParseError
		if !errors.As(err, &parseErr) {
			t.Fatalf("corrupt continuation error = %T (%v), want *recordlayer.ContinuationParseError", err, err)
		}
	}

	// A 3-child token fed to a 2-child merge: count mismatch.
	threeChild := &gen.UnionContinuation{
		OtherChildState: []*gen.UnionContinuation_CursorState{{Continuation: []byte("X")}},
	}
	b, err := threeChild.MarshalVT()
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if _, err := newMergeSortCursorFromFactories(factories, idKey(), false, true, b); err == nil {
		t.Fatal("child-count mismatch must fail construction (a 3-child token cannot resume a 2-child merge)")
	}
}

// stubStopCursor immediately stops with a fixed no-next reason/continuation.
type stubStopCursor struct {
	reason recordlayer.NoNextReason
	cont   recordlayer.RecordCursorContinuation
	closed bool
}

func (s *stubStopCursor) OnNext(context.Context) (recordlayer.RecordCursorResult[QueryResult], error) {
	return recordlayer.NewResultNoNext[QueryResult](s.reason, s.cont), nil
}
func (s *stubStopCursor) Close() error   { s.closed = true; return nil }
func (s *stubStopCursor) IsClosed() bool { return s.closed }
