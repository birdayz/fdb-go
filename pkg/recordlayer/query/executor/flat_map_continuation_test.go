package executor

import (
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
