package executor

import (
	"errors"
	"testing"

	"google.golang.org/protobuf/proto"

	"fdb.dev/gen"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer"
)

// TestFlatMapBuildContinuation_PairsPriorOuterWithInner pins the FlatMap
// continuation encoding against Java FlatMapPipelinedCursor.Continuation
// (cursors/FlatMapPipelinedCursor.java:373), which ALWAYS pairs
// priorOuterContinuation (the position AT the current outer row) with the inner
// continuation:
//   - inner has a resumable position (not END) → encode (priorOuter, inner) so a
//     mid-inner stop resumes THIS outer's inner. Encoding the ADVANCED outer
//     (lastOuterContinuation) here would skip the current outer's remaining inner
//     rows on resume — a silent row drop / check_value mismatch.
//   - inner exhausted (END) → advance to the next outer (lastOuter, no inner).
func TestFlatMapBuildContinuation_PairsPriorOuterWithInner(t *testing.T) {
	t.Parallel()

	newCursor := func() *flatMapCursor {
		return &flatMapCursor{
			priorOuterContinuation: recordlayer.NewBytesContinuation([]byte("PRIOR")),
			lastOuterContinuation:  recordlayer.NewBytesContinuation([]byte("LAST")),
			currentOuter:           &QueryResult{PrimaryKey: tuple.Tuple{int64(3)}},
		}
	}
	decode := func(t *testing.T, cont recordlayer.RecordCursorContinuation) *gen.FlatMapContinuation {
		t.Helper()
		b, err := cont.ToBytes()
		if err != nil {
			t.Fatalf("ToBytes: %v", err)
		}
		var fmc gen.FlatMapContinuation
		if err := proto.Unmarshal(b, &fmc); err != nil {
			t.Fatalf("Unmarshal: %v", err)
		}
		return &fmc
	}

	t.Run("inner not exhausted pairs priorOuter+inner", func(t *testing.T) {
		t.Parallel()
		fmc := decode(t, newCursor().buildContinuation(recordlayer.NewBytesContinuation([]byte("INNER"))))
		if string(fmc.OuterContinuation) != "PRIOR" {
			t.Errorf("value-emit must encode priorOuterContinuation, got %q (a lastOuter leak is the row-drop bug)", fmc.OuterContinuation)
		}
		if string(fmc.InnerContinuation) != "INNER" {
			t.Errorf("value-emit must encode the inner continuation, got %q", fmc.InnerContinuation)
		}
		if string(fmc.CheckValue) != string(tuple.Tuple{int64(3)}.Pack()) {
			t.Errorf("check_value must be the current outer PK, got %x", fmc.CheckValue)
		}
	})

	t.Run("inner exhausted advances to lastOuter with no inner", func(t *testing.T) {
		t.Parallel()
		fmc := decode(t, newCursor().buildContinuation(&recordlayer.EndContinuation{}))
		if string(fmc.OuterContinuation) != "LAST" {
			t.Errorf("inner-exhausted must advance to lastOuterContinuation, got %q", fmc.OuterContinuation)
		}
		if len(fmc.InnerContinuation) != 0 {
			t.Errorf("inner-exhausted must not encode an inner continuation, got %q", fmc.InnerContinuation)
		}
	})
}

// encodeFailContinuation is a non-end continuation whose ToBytes always
// errors. Stands in for any child continuation with unencodable state (e.g. a
// sort continuation whose typed codec rejects a slot value).
type encodeFailContinuation struct{}

func (encodeFailContinuation) ToBytes() ([]byte, error) {
	return nil, errors.New("continuation encode failed")
}
func (encodeFailContinuation) IsEnd() bool { return false }

// TestFlatMapContinuation_EncodeErrorsPropagate pins the error-propagation
// plumbing of the FlatMap continuation: a child continuation whose encode
// fails must surface that error from ToBytes() (the RecordCursorContinuation
// contract), never serialize a FlatMapContinuation with the failed component
// silently MISSING. A swallowed inner-encode error used to produce a
// continuation without inner position — resuming it silently restarts the
// current outer's inner from scratch (duplicate rows); a swallowed outer-encode
// error restarts the whole outer scan. Mirrors flatMapContinuationWrapper in
// pkg/recordlayer/cursor_combinators.go, which has always propagated.
func TestFlatMapContinuation_EncodeErrorsPropagate(t *testing.T) {
	t.Parallel()

	wantErr := func(t *testing.T, cont recordlayer.RecordCursorContinuation) {
		t.Helper()
		if cont.IsEnd() {
			t.Fatal("continuation with a live position must not be end")
		}
		if _, err := cont.ToBytes(); err == nil {
			t.Fatal("ToBytes: want encode error to propagate, got nil (a swallowed encode drops the position and silently re-emits or skips rows on resume)")
		}
	}

	t.Run("inner encode failure (mid-inner branch)", func(t *testing.T) {
		t.Parallel()
		c := &flatMapCursor{
			priorOuterContinuation: recordlayer.NewBytesContinuation([]byte("PRIOR")),
			lastOuterContinuation:  recordlayer.NewBytesContinuation([]byte("LAST")),
			currentOuter:           &QueryResult{PrimaryKey: tuple.Tuple{int64(3)}},
		}
		wantErr(t, c.buildContinuation(encodeFailContinuation{}))
	})

	t.Run("prior outer encode failure (mid-inner branch)", func(t *testing.T) {
		t.Parallel()
		c := &flatMapCursor{
			priorOuterContinuation: encodeFailContinuation{},
			lastOuterContinuation:  recordlayer.NewBytesContinuation([]byte("LAST")),
			currentOuter:           &QueryResult{PrimaryKey: tuple.Tuple{int64(3)}},
		}
		wantErr(t, c.buildContinuation(recordlayer.NewBytesContinuation([]byte("INNER"))))
	})

	t.Run("advanced outer encode failure (inner exhausted)", func(t *testing.T) {
		t.Parallel()
		c := &flatMapCursor{
			priorOuterContinuation: recordlayer.NewBytesContinuation([]byte("PRIOR")),
			lastOuterContinuation:  encodeFailContinuation{},
		}
		wantErr(t, c.buildContinuation(&recordlayer.EndContinuation{}))
	})

	t.Run("outer stop encode failure (wrapOuterContinuation)", func(t *testing.T) {
		t.Parallel()
		c := &flatMapCursor{}
		wantErr(t, c.wrapOuterContinuation(encodeFailContinuation{}))
	})
}

// TestFlatMapWrapOuterContinuation_PreservesPendingInner pins the outer-stop
// serialization: the outer position is encoded, and a pending inner
// continuation from the original resume (outer stopped before re-reaching the
// resumed row) is carried forward verbatim.
func TestFlatMapWrapOuterContinuation_PreservesPendingInner(t *testing.T) {
	t.Parallel()

	c := &flatMapCursor{
		initialInnerCont: []byte("PENDING"),
		hasPendingInner:  true,
	}
	cont := c.wrapOuterContinuation(recordlayer.NewBytesContinuation([]byte("OUTER")))
	// Mutating the cursor's pending state after the continuation is built must
	// not corrupt an already-issued continuation (continuations are immutable
	// snapshots; the cursor nils initialInnerCont when it consumes it).
	c.initialInnerCont = nil
	c.hasPendingInner = false
	b, err := cont.ToBytes()
	if err != nil {
		t.Fatalf("ToBytes: %v", err)
	}
	var fmc gen.FlatMapContinuation
	if err := proto.Unmarshal(b, &fmc); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if string(fmc.OuterContinuation) != "OUTER" {
		t.Errorf("outer-stop must encode the outer position, got %q", fmc.OuterContinuation)
	}
	if string(fmc.InnerContinuation) != "PENDING" {
		t.Errorf("outer-stop must carry the pending inner continuation forward, got %q", fmc.InnerContinuation)
	}
}
