package executor

import (
	"context"
	"strings"
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
// bound to savedPK's check value.
func flatMapArmedFixture(t *testing.T, outer recordlayer.RecordCursor[QueryResult], savedPK tuple.Tuple) *flatMapCursor {
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
	c.initialInnerCont = []byte("armed-inner")
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
func TestFlatMapKeptArmed_SurvivesMismatchingRow(t *testing.T) {
	t.Parallel()
	rows := nljTestRows("K", 2) // PKs {K,0}, {K,1}
	c := flatMapArmedFixture(t, recordlayer.FromList(rows), tuple.Tuple{"K", int64(1)})

	// First outer row {K,0} mismatches: its inner runs, armed state SURVIVES.
	res, err := c.OnNext(context.Background())
	if err != nil || !res.HasNext() {
		t.Fatalf("first row: %v", err)
	}
	if c.initialInnerCont == nil {
		t.Fatal("armed inner continuation must SURVIVE a mismatching outer row (Java keeps it until consumed)")
	}
	// Drain to the second (matching) row: the armed bytes are consumed and
	// handed to the inner plan — the VALUES inner rejects foreign bytes
	// loudly, and THAT error is the proof of consumption at the right row
	// (a first-row discard would never present the bytes to any inner; a
	// first-row consume would have errored on the FIRST pull above).
	for {
		res, err = c.OnNext(context.Background())
		if err != nil {
			if !strings.Contains(err.Error(), "cannot resume from a continuation") {
				t.Fatalf("drain: %v", err)
			}
			break
		}
		if !res.HasNext() {
			t.Fatal("cursor exhausted with the armed state never consumed — the matching row did not apply it")
		}
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
	c := flatMapArmedFixture(t, recordlayer.Empty[QueryResult](), tuple.Tuple{"K", int64(5)})
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
	c := flatMapArmedFixture(t, outer, tuple.Tuple{"K", int64(0)})
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
	c := flatMapArmedFixture(t, recordlayer.FromList(rows[1:]), tuple.Tuple{"K", int64(0)})
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
