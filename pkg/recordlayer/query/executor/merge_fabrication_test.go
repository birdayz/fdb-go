package executor

import (
	"testing"

	"fdb.dev/gen"
	"google.golang.org/protobuf/proto"
)

// TestQualifyAlias_CompletenessRouting pins the name-model merge's
// fabrication authority on BOTH sides of the completeness gate — the
// dimension that was unprobed when the dotted-key refusal over-reached and
// silently NULLed every qualified read of an enclosed CTE/derived leg:
//
//   - an INCOMPLETE dotted-key src (a JOIN-MERGE output) must REFUSE
//     fabrication: its bare keys are last-leg-wins leftovers, and fabricating
//     "ALIAS.COL" from them reads the wrong source (the mixed-nesting pins'
//     hazard class);
//   - a COMPLETE dotted-key src (a projection output / unnest FlatMap RC row)
//     must FABRICATE-IF-ABSENT "ALIAS.key" for every key — its alias
//     genuinely names the whole row, and refusing left the enclosing merge
//     with no "C.col" keys (all-NULL rows with correct multiplicity).
func TestQualifyAlias_CompletenessRouting(t *testing.T) {
	t.Parallel()

	dottedSrc := map[string]any{
		"AK":   int64(100),
		"LA.K": int64(100),
	}

	t.Run("incomplete_dotted_src_refuses", func(t *testing.T) {
		t.Parallel()
		dst := map[string]any{}
		qualifyAlias(dst, dottedSrc, "C", false)
		if len(dst) != 0 {
			t.Fatalf("incomplete dotted-key src must not fabricate, got %v", dst)
		}
	})

	t.Run("complete_bare_key_overwrites_stale", func(t *testing.T) {
		t.Parallel()
		// The review-proven stale-key scenario: the outer row already carries
		// "D.ID" from a DIFFERENT nesting level; the current leg aliased D is
		// authoritative for D.* — its bare schema key must WIN. A
		// fill-if-absent here preserved the stale value (wrong-source reads).
		dst := map[string]any{"D.ID": int64(1)}
		qualifyAlias(dst, map[string]any{"ID": int64(2), "LA.K": int64(2)}, "D", true)
		if got := dst["D.ID"]; got != int64(2) {
			t.Fatalf("current leg's bare key must overwrite the stale alias key: D.ID = %v, want 2", got)
		}
	})

	t.Run("complete_dotted_key_fills_if_absent", func(t *testing.T) {
		t.Parallel()
		// Dotted convenience keys ("LA.K" → "C.LA.K") must never clobber an
		// authoritative key an earlier merge level wrote.
		dst := map[string]any{"C.LA.K": int64(999)}
		qualifyAlias(dst, dottedSrc, "C", true)
		if got := dst["C.LA.K"]; got != int64(999) {
			t.Fatalf("dotted convenience key clobbered an existing key: C.LA.K = %v, want 999", got)
		}
		if got := dst["C.AK"]; got != int64(100) {
			t.Fatalf("complete src must fabricate its bare keys: C.AK = %v, want 100", got)
		}
	})

	t.Run("incomplete_bare_src_keeps_legacy_behavior", func(t *testing.T) {
		t.Parallel()
		dst := map[string]any{}
		qualifyAlias(dst, map[string]any{"ID": int64(1)}, "T", false)
		if got := dst["T.ID"]; got != int64(1) {
			t.Fatalf("single-source bare src must qualify: T.ID = %v, want 1", got)
		}
	})
}

// TestMergeRows_OutputStaysIncomplete pins the design requirement that keeps
// the mixed-nesting refusal alive at the NEXT merge level: a merged row's
// bare keys are last-leg-wins leftovers even when both LEGS were Complete,
// so the OUTPUT must never carry Complete=true (a later qualifyAlias over it
// would fabricate wrong-source keys).
func TestMergeRows_OutputStaysIncomplete(t *testing.T) {
	t.Parallel()
	outer := QueryResult{Datum: map[string]any{"AK": int64(100)}, Complete: true}
	inner := QueryResult{Datum: map[string]any{"CV": int64(900)}, Complete: true}
	merged := mergeRows(outer, inner, "C", "CC2")
	if merged.Complete {
		t.Fatal("mergeRows output must stay Complete=false even over Complete legs")
	}
	m := merged.Datum.(map[string]any)
	if m["C.AK"] != int64(100) || m["CC2.CV"] != int64(900) {
		t.Fatalf("complete legs must still alias-qualify: %v", m)
	}
}

// TestSortContinuation_PreservesComplete pins the continuation round-trip of
// QueryResult.Complete: the flag gates the merge's alias fabrication
// (qualifyAlias), so dropping it across a sort-buffer continuation made
// resumed buffered rows refuse fabrication while post-resume rows fabricated —
// qualified columns differing across a page boundary. Also pins the LEGACY
// payload path: a pre-v2 continuation (bare JSON object) still decodes, with
// Complete=false (its pre-v2 behavior).
func TestSortContinuation_PreservesComplete(t *testing.T) {
	t.Parallel()

	buf := []QueryResult{
		{Datum: map[string]any{"AK": int64(100)}, Complete: true},
		{Datum: map[string]any{"AK": int64(110)}, Complete: false},
	}
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
	if decoded[0].Datum.(map[string]any)["AK"] != int64(100) {
		t.Fatalf("datum lost in round-trip: %+v", decoded[0].Datum)
	}

	t.Run("legacy_object_payload_decodes", func(t *testing.T) {
		t.Parallel()
		sr, _ := proto.Marshal(&gen.SortedRecord{Message: []byte(`{"AK":100}`)})
		legacy, _ := proto.Marshal(&gen.MemorySortContinuation{Records: [][]byte{sr}})
		_, decoded, err := decodeSortContinuation(legacy)
		if err != nil {
			t.Fatalf("legacy decode: %v", err)
		}
		if len(decoded) != 1 || decoded[0].Complete {
			t.Fatalf("legacy payload must decode with Complete=false: %+v", decoded)
		}
		if decoded[0].Datum.(map[string]any)["AK"] != int64(100) {
			t.Fatalf("legacy datum lost: %+v", decoded[0].Datum)
		}
	})
}

// TestQualifyOuterRow_CompletenessRouting pins the pad-row path's twin gate:
// an INCOMPLETE merged pad row keeps the fabrication refusal (the null-padded
// box row hazard — the authoritative key is ABSENT, fabricating reads the
// wrong source); a COMPLETE dotted-key pad row (a CTE/derived leg as the
// preserved side) fabricates-if-absent under its alias.
func TestQualifyOuterRow_CompletenessRouting(t *testing.T) {
	t.Parallel()

	t.Run("incomplete_merged_pad_refuses", func(t *testing.T) {
		t.Parallel()
		qr := qualifyOuterRow(QueryResult{Datum: map[string]any{"ID": int64(1), "D.ID": int64(1)}}, "E")
		m := qr.Datum.(map[string]any)
		if _, fabricated := m["E.ID"]; fabricated {
			t.Fatalf("incomplete merged pad row must not fabricate E.*: %v", m)
		}
	})

	t.Run("complete_pad_fabricates", func(t *testing.T) {
		t.Parallel()
		qr := qualifyOuterRow(QueryResult{
			Datum:    map[string]any{"AK": int64(100), "LA.K": int64(100)},
			Complete: true,
		}, "C")
		m := qr.Datum.(map[string]any)
		if m["C.AK"] != int64(100) {
			t.Fatalf("complete pad row must fabricate C.AK: %v", m)
		}
	})
}
