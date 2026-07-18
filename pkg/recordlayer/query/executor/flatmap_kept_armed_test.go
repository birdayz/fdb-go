package executor

import (
	"context"
	"testing"

	"google.golang.org/protobuf/proto"

	"fdb.dev/gen"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

// flatMapArmedFixture builds a flatMapCursor over the given outer rows with a
// trivial single-row values inner, armed with a pending inner continuation
// bound to savedPK's check value. armedBytes is the pending inner
// continuation the matching row will hand to its inner plan.
func flatMapArmedFixture(t *testing.T, outer recordlayer.RecordCursor[QueryResult], savedPK tuple.Tuple, armedBytes []byte) *flatMapCursor {
	t.Helper()
	inner := plans.NewRecordQueryValuesPlan([]values.Value{&values.ConstantValue{Value: int64(7)}})
	c, err := newFlatMapCursor(
		outer, plans.NewRecordQueryValuesPlan(nil), inner, nil, EmptyEvaluationContext(),
		values.NamedCorrelationIdentifier("O"), values.NamedCorrelationIdentifier("I"),
		values.NewQuantifiedObjectValue(values.NamedCorrelationIdentifier("I")), recordlayer.ExecuteProperties{},
	)
	if err != nil {
		t.Fatalf("newFlatMapCursor: %v", err)
	}
	c.initialInnerCont = armedBytes
	c.hasPendingInner = true
	c.pendingCheckValue = savedPK.Pack()
	return c
}

// TestFlatMapKeptArmed_SurvivesMismatchingRow pins Java's kept-armed contract
// on the flatMap cursor itself (FlatMapPipelinedCursor nulls
// initialInnerContinuation ONLY in the match branch): a mismatching outer row
// runs its inner fresh while the armed state waits; the matching row consumes
// it. The prior first-row discard re-ran the saved row's inner from scratch —
// duplicate rows — while claiming Java fidelity.
//
// Proof of consumption at the RIGHT row: the armed bytes are a list
// continuation POSITIONED PAST the single VALUES row, so the matching
// outer's inner resumes exhausted (0 rows) while every fresh inner emits 1 —
// a first-row consume would drop the FIRST row's emission, a first-row
// discard would emit both.
func TestFlatMapKeptArmed_SurvivesMismatchingRow(t *testing.T) {
	t.Parallel()
	rows := nljTestRows("K", 2) // PKs {K,0}, {K,1}
	pastTheOneRow := []byte{0, 0, 0, 1}
	c := flatMapArmedFixture(t, recordlayer.FromList(rows), tuple.Tuple{"K", int64(1)}, pastTheOneRow)

	// First outer row {K,0} mismatches: its inner runs FRESH, armed state SURVIVES.
	res, err := c.OnNext(context.Background())
	if err != nil || !res.HasNext() {
		t.Fatalf("first row: %v", err)
	}
	if c.initialInnerCont == nil {
		t.Fatal("armed inner continuation must SURVIVE a mismatching outer row (Java keeps it until consumed)")
	}
	// Drain: the matching row {K,1} consumes the armed bytes — its inner
	// resumes at position 1 of a 1-row list (0 rows), so NO further row is
	// emitted and the cursor exhausts.
	extra := 0
	for {
		res, err = c.OnNext(context.Background())
		if err != nil {
			t.Fatalf("drain: %v", err)
		}
		if !res.HasNext() {
			break
		}
		extra++
	}
	if extra != 0 {
		t.Fatalf("matching row emitted %d rows, want 0 (its inner must RESUME from the armed position, not run fresh)", extra)
	}
	if c.initialInnerCont != nil || len(c.pendingCheckValue) != 0 {
		t.Fatal("consumption must clear the armed state and its check value")
	}
}

// TestFlatMapKeptArmed_WrapCarriesCheckValue pins the re-resume corner: an
// outer stop while the state is still armed must wrap BOTH the pending inner
// bytes AND the check value — without the check, the next resume would apply
// the armed inner to the first outer row unverified (the wrong row when the
// saved one is no longer first).
func TestFlatMapKeptArmed_WrapCarriesCheckValue(t *testing.T) {
	t.Parallel()
	c := flatMapArmedFixture(t, recordlayer.Empty[QueryResult](), tuple.Tuple{"K", int64(5)}, []byte("armed-inner"))
	cont := c.wrapOuterContinuation(recordlayer.NewBytesContinuation([]byte("outer-pos")))
	b, err := cont.ToBytes()
	if err != nil {
		t.Fatalf("ToBytes: %v", err)
	}
	fmc := &gen.FlatMapContinuation{}
	if err := proto.Unmarshal(b, fmc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if string(fmc.GetInnerContinuation()) != "armed-inner" {
		t.Fatalf("pending inner must ride the wrap, got %q", fmc.GetInnerContinuation())
	}
	if string(fmc.GetCheckValue()) != string((tuple.Tuple{"K", int64(5)}).Pack()) {
		t.Fatalf("the pending CHECK VALUE must ride the wrap, got %v", fmc.GetCheckValue())
	}
}

// TestFlatMapTerminalReplayOnRecall pins Java's cached-terminal contract
// (FlatMapPipelinedCursor:123-125) on the flatMap cursor: a re-call after an
// out-of-band outer stop replays the identical result — never re-pulls the
// outer, whose next pull here would surface MORE rows.
func TestFlatMapTerminalReplayOnRecall(t *testing.T) {
	t.Parallel()
	rows := nljTestRows("K", 2)
	outer := &flakyOuterCursor{first: rows[:1], rest: rows[1:]}
	c := flatMapArmedFixture(t, outer, tuple.Tuple{"K", int64(0)}, []byte("armed-inner"))
	// Disarm the fixture's pending state — this pin is about the terminal
	// cache, not the resume path.
	c.initialInnerCont = nil
	c.hasPendingInner = false
	c.pendingCheckValue = nil
	ctx := context.Background()
	var first recordlayer.RecordCursorResult[QueryResult]
	for {
		res, err := c.OnNext(ctx)
		if err != nil {
			t.Fatalf("drain: %v", err)
		}
		if !res.HasNext() {
			first = res
			break
		}
	}
	if first.GetNoNextReason() != recordlayer.TimeLimitReached {
		t.Fatalf("fixture must stop out-of-band, got %v", first.GetNoNextReason())
	}
	again, err := c.OnNext(ctx)
	if err != nil {
		t.Fatalf("re-call: %v", err)
	}
	if again.HasNext() {
		t.Fatal("re-call after the out-of-band stop must replay the terminal result, not surface more outer rows")
	}
	fb, _ := first.GetContinuation().ToBytes()
	ab, _ := again.GetContinuation().ToBytes()
	if again.GetNoNextReason() != first.GetNoNextReason() || string(fb) != string(ab) {
		t.Fatalf("re-call (%v, %v) must equal the cached terminal (%v, %v)",
			again.GetNoNextReason(), ab, first.GetNoNextReason(), fb)
	}
}

// TestFlatMapTerminalReplayOnRecall_InnerStop pins the OTHER terminal-cache
// site: the INNER cursor pauses out-of-band mid-row. A re-call must replay
// the cached terminal — without the cache the nil'd inner falls through to
// the outer advance and the next outer row's inner emits a VALUE (silently
// skipping the paused row's remaining inner rows).
func TestFlatMapTerminalReplayOnRecall_InnerStop(t *testing.T) {
	t.Parallel()
	rows := nljTestRows("K", 2)
	c := flatMapArmedFixture(t, recordlayer.FromList(rows[1:]), tuple.Tuple{"K", int64(0)}, []byte("armed-inner"))
	c.initialInnerCont = nil
	c.hasPendingInner = false
	c.pendingCheckValue = nil
	// Mid-row state: the current outer is bound and its inner pauses
	// out-of-band on the next pull.
	c.currentOuter = &rows[0]
	c.innerCursor = &pausingCursor{rows: nil, cont: []byte("inner-pos")}

	ctx := context.Background()
	first, err := c.OnNext(ctx)
	if err != nil || first.HasNext() {
		t.Fatalf("inner pause must surface as a no-next: %v", err)
	}
	if first.GetNoNextReason() != recordlayer.TimeLimitReached {
		t.Fatalf("reason = %v, want the inner's TimeLimitReached", first.GetNoNextReason())
	}
	again, err := c.OnNext(ctx)
	if err != nil {
		t.Fatalf("re-call: %v", err)
	}
	if again.HasNext() {
		t.Fatal("re-call after the inner's out-of-band stop must replay the terminal, not advance to the next outer row")
	}
	fb, _ := first.GetContinuation().ToBytes()
	ab, _ := again.GetContinuation().ToBytes()
	if again.GetNoNextReason() != first.GetNoNextReason() || string(fb) != string(ab) {
		t.Fatalf("re-call (%v, %v) must equal the cached terminal (%v, %v)",
			again.GetNoNextReason(), ab, first.GetNoNextReason(), fb)
	}
}
