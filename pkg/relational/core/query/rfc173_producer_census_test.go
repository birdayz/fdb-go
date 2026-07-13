package query

// White-box regression for the RFC-173 producer census P5 (buildUnnestResultValue)
// hook. The census records an ENCLOSED bit per firing. P5's callers
// (translateUnnestJoin / translateChainedUnnestJoin) unconditionally set
// t.inInnerCluster = true on entry, so reading that field inside the producer would
// report EVERY P5 firing as enclosed by construction — blinding the census to an
// un-enclosed P5 residual. The fix threads the caller's ENTRY enclosure
// (prevEnclosure) into buildUnnestResultValue.
//
// Discriminator: a chain over a NESTED-outer-box bottom (`(A LEFT B) FULL C`)
// declines to the name-model record and fires P5 at its links (the multi-source
// INNER box bottom now ordinalizes, so it is no longer a P5 discriminator — a
// nested outer box does not gate fresh and stays name-model). The OUTERMOST
// link's enclosure is the prevEnclosure passed to translateChainedUnnestJoin.
// Translating the SAME shape fresh (prevEnclosure=false) vs enclosed (=true)
// must therefore change the un-enclosed P5 count — the outer link flips. If the
// producer read the always-true internal field instead (the bug), every firing
// would report enclosed and BOTH counts would be zero.

import (
	"testing"

	"fdb.dev/pkg/relational/core/query/logical"
)

func TestRFC173_ProducerCensusP5EnclosureBit(t *testing.T) {
	boxBase3 := func() (*logical.LogicalJoin, *logical.LogicalUnnest) {
		// A NESTED outer box bottom (`(A LEFT B) LEFT C`): the ordinal seed
		// declines (a LEFT/RIGHT box is not walk-admitted — bottomInnerBox is
		// INNER-only), so the chain declines to the name-model buildUnnestResultValue
		// at every link. NOT a FULL box, so the RFC-173 S4 cap FULL-box straddle
		// reject does not fire — this exercises the surviving P5 name-model residual
		// whose enclosure bit this test pins (a FULL outer bottom would loud-reject).
		innerLeft := logical.NewJoin(scan("T4", "A"), scan("T4", "B"), logical.JoinLeft, "")
		nested := logical.NewJoin(innerLeft, scan("T4", "C"), logical.JoinLeft, "")
		l1, _ := link(nested, "A", "SARR", "X")
		l2, _ := link(l1, "X", "SUBSTRUCT", "Y")
		return link(l2, "Y", "DEEP", "Z")
	}

	// Drive the production entry point translateUnnestJoin with inInnerCluster
	// preset to the caller's entry enclosure (rather than calling
	// translateChainedUnnestJoin directly), so EVERY link — inner and outer —
	// reports its production enclosure, not a direct-call zero-value artifact.
	unenclosedP5 := func(entryEnclosure bool) int {
		var got []ProducerCensusRecord
		SetProducerCensusObserver(func(rec ProducerCensusRecord) { got = append(got, rec) })
		defer SetProducerCensusObserver(nil)
		tr := newChainedSpineTranslator(t)
		tr.inInnerCluster = entryEnclosure
		j, u := boxBase3()
		if sel := tr.translateUnnestJoin(j, u); sel == nil {
			t.Fatalf("entryEnclosure=%v: translateUnnestJoin returned nil (translateErr: %v)", entryEnclosure, tr.translateErr)
		}
		n := 0
		for _, r := range got {
			if r.Producer == "P5" && !r.Enclosed {
				n++
			}
		}
		return n
	}

	fresh, enclosed := unenclosedP5(false), unenclosedP5(true)
	if fresh == 0 {
		t.Fatalf("prevEnclosure=false produced ZERO un-enclosed P5 firings — the census reads the always-true internal inInnerCluster, not the caller's entry enclosure (codex P1)")
	}
	if fresh <= enclosed {
		t.Fatalf("un-enclosed P5 count did not drop when the caller was enclosed (fresh=%d, enclosed=%d) — the P5 enclosed bit is not tracking the entry enclosure (codex P1)", fresh, enclosed)
	}
}
