package query

// White-box regression for the RFC-173 producer census P5 (buildUnnestResultValue)
// hook, and the S4-B retirement of the CHAINED-UNNEST-OVER-NESTED-OUTER-BOX P5
// caller.
//
// Two facts are pinned:
//
//  1. RETIREMENT (RED-on-revert of the chainedSpineWalk LEFT/RIGHT-box admission
//     + the SelectMergeRule dissolved-box barrier): a chained lateral unnest whose
//     spine bottoms in a NESTED LEFT outer box (`(A LEFT B) LEFT C, A.SARR AS X,
//     X.SUBSTRUCT AS Y, Y.DEEP AS Z`) now ORDINALIZES un-enclosed — it fires ZERO
//     P5 producers. Before S4-B it was the surviving name-model P5 residual. If the
//     admission is reverted, the shape drops back to the name model and this trips.
//
//  2. CENSUS OBSERVER NON-VACUITY: the observer STILL fires for a name-model P5
//     shape, so fact (1)'s 0-count is meaningful (not a silently-broken observer).
//     The shape here is an ENCLOSED single unnest over a LEFT box (an unnest that is
//     itself a leg of a larger name-model cluster); it fires P5 and reports the
//     ENTRY enclosure. This assertion is a non-vacuity/sanity check, NOT a
//     threading-bit regression pin — passing entryEnclosure=true, the producer
//     reports Enclosed=true whether it reads the threaded prevEnclosure or the
//     always-true inInnerCluster, so it does not by itself discriminate the
//     threading fix. (UN-enclosed P5 firings DO still exist for OTHER name-model
//     residuals — filtered chained unnests, scalar-subquery-multirow projections,
//     grouped bare-key-over-join — which are the REMAINING producer callers toward
//     full P5/P4 retirement (RFC-173 item B, TODO.md); those could re-arm a
//     discriminating enclosure-bit pin, but they are not this test's subject.)

import (
	"testing"

	"fdb.dev/pkg/relational/core/query/logical"
)

func TestRFC173_ProducerCensusP5EnclosureBit(t *testing.T) {
	countP5 := func(build func() (*logical.LogicalJoin, *logical.LogicalUnnest), entryEnclosure bool) (total, unenclosed int) {
		var got []ProducerCensusRecord
		SetProducerCensusObserver(func(rec ProducerCensusRecord) { got = append(got, rec) })
		defer SetProducerCensusObserver(nil)
		tr := newChainedSpineTranslator(t)
		tr.inInnerCluster = entryEnclosure
		j, u := build()
		if sel := tr.translateUnnestJoin(j, u); sel == nil && tr.translateErr != nil {
			t.Fatalf("entryEnclosure=%v: translateUnnestJoin errored: %v", entryEnclosure, tr.translateErr)
		}
		for _, r := range got {
			if r.Producer == "P5" {
				total++
				if !r.Enclosed {
					unenclosed++
				}
			}
		}
		return total, unenclosed
	}

	// (1) The RETIREMENT: a NESTED LEFT outer box bottom (`(A LEFT B) LEFT C`)
	// under a 3-link chain ordinalizes and fires ZERO P5, un-enclosed. Before
	// S4-B the chained ordinal declined (LEFT/RIGHT boxes were not walk-admitted)
	// and every link name-modeled buildUnnestResultValue.
	nestedChain := func() (*logical.LogicalJoin, *logical.LogicalUnnest) {
		innerLeft := logical.NewJoin(scan("T4", "A"), scan("T4", "B"), logical.JoinLeft, "")
		nested := logical.NewJoin(innerLeft, scan("T4", "C"), logical.JoinLeft, "")
		l1, _ := link(nested, "A", "SARR", "X")
		l2, _ := link(l1, "X", "SUBSTRUCT", "Y")
		return link(l2, "Y", "DEEP", "Z")
	}
	if total, _ := countP5(nestedChain, false); total != 0 {
		t.Fatalf("nested LEFT box chain (un-enclosed) fired %d P5 producer(s), want 0 — the S4-B "+
			"chained LEFT/RIGHT-box admission regressed to the name model", total)
	}

	// (2) The surviving observable P5 firing + enclosure bit: an ENCLOSED single
	// unnest over a LEFT box fires P5, and the firing must report Enclosed=true
	// (the entry enclosure), never a hardcoded false.
	enclosedSingle := func() (*logical.LogicalJoin, *logical.LogicalUnnest) {
		box := logical.NewJoin(scan("T4", "A"), scan("T4", "B"), logical.JoinLeft, "")
		return link(box, "A", "SARR", "X")
	}
	total, unenclosed := countP5(enclosedSingle, true)
	if total == 0 {
		t.Fatalf("enclosed single unnest over a LEFT box fired ZERO P5 — the census observer no longer " +
			"sees the name-model producer (observer-mechanism vacuity)")
	}
	if unenclosed != 0 {
		t.Fatalf("an ENCLOSED P5 firing reported Enclosed=false (%d of %d) — the producer reads the "+
			"wrong enclosure (not the caller's entry prevEnclosure)", unenclosed, total)
	}
}
