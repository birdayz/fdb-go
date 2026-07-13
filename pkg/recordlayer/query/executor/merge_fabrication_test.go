package executor

import (
	"testing"

	"fdb.dev/gen"
	"google.golang.org/protobuf/proto"
)

// TestMergeRows_LegWindowedQualifiedReads pins the RFC-173 merge: mergeRows
// builds a leg-windowed positional row (concatLegPositionals), so a qualified
// read "C.AK" / "CC2.CV" resolves LEG-LOCALLY through the alias window — the
// ordinal-model successor to the retired name-model "ALIAS.COL" key
// fabrication. The merged output stays Complete=false (its bare slots are
// last-leg-wins leftovers, not a schema).
func TestMergeRows_LegWindowedQualifiedReads(t *testing.T) {
	t.Parallel()
	outer := dmap(map[string]any{"AK": int64(100)})
	inner := dmap(map[string]any{"CV": int64(900)})
	merged := mergeRows(outer, inner, "C", "CC2")
	if merged.Complete {
		t.Fatal("mergeRows output must stay Complete=false even over Complete legs")
	}
	if v := rowVal(merged, "C.AK"); v != int64(100) {
		t.Fatalf("C.AK = %v, want 100 (leg window)", v)
	}
	if v := rowVal(merged, "CC2.CV"); v != int64(900) {
		t.Fatalf("CC2.CV = %v, want 900 (leg window)", v)
	}
}

// TestSortContinuation_PreservesComplete pins the continuation round-trip of
// QueryResult.Complete (it gates downstream behavior, so dropping it across a
// sort-buffer continuation would make resumed and post-resume rows disagree) and
// the positional payload round-trip. Also pins the LEGACY payload path: a
// pre-migration continuation (bare JSON object — the deleted name-keyed datum)
// still DECODES without error, with Complete=false and no positional (its data is
// unrepresentable in the ordinal model, but it must not crash the resume).
func TestSortContinuation_PreservesComplete(t *testing.T) {
	t.Parallel()

	r0 := dmap(map[string]any{"AK": int64(100)})
	r0.Complete = true
	r1 := dmap(map[string]any{"AK": int64(110)})
	r1.Complete = false
	buf := []QueryResult{r0, r1}
	enc, err := encodeSortContinuation(nil, buf)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	_, decoded, err := decodeSortContinuation(enc)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(decoded) != 2 || !decoded[0].Complete || decoded[1].Complete {
		t.Fatalf("Complete lost in round-trip: %+v", decoded)
	}
	if rowVal(decoded[0], "AK") != int64(100) {
		t.Fatalf("positional lost in round-trip: %+v", decoded[0].Positional)
	}

	t.Run("legacy_object_payload_decodes", func(t *testing.T) {
		t.Parallel()
		sr, _ := proto.Marshal(&gen.SortedRecord{Message: []byte(`{"AK":100}`)})
		legacy, _ := proto.Marshal(&gen.MemorySortContinuation{Records: [][]byte{sr}})
		_, decoded, err := decodeSortContinuation(legacy)
		if err != nil {
			t.Fatalf("legacy decode: %v", err)
		}
		if len(decoded) != 1 || decoded[0].Complete || decoded[0].Positional != nil {
			t.Fatalf("legacy payload must decode with Complete=false, no positional: %+v", decoded)
		}
	})
}
