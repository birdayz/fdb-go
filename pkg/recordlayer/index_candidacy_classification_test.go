package recordlayer

import (
	_ "embed"
	"regexp"
	"testing"
)

// indexGoSource is the index.go source, embedded so the classification test below
// enumerates every IndexType* constant automatically — a newly-added constant is
// picked up without editing this test's regex.
//
//go:embed index.go
var indexGoSource string

// TestIndexScanCandidacy_EveryTypeClassified is F45's fail-safe for the F35 fix.
//
// GetMatchCandidates (cascades_generator.go) offers a VALUE-index scan candidate
// by fall-through and opts OUT via the IsAtomicMutationIndex deny-list. That is
// fail-OPEN: a NEW index type added without a deny-list/classification entry
// silently becomes a value-scan candidate and could reintroduce the F35
// stale-read bug class. Java is fail-safe by construction — each
// IndexMaintainerFactory opts INTO value candidacy — but Go's polarity is
// inverted, so a missing classification fails toward "is a value candidate".
//
// This test converts that into fail-safe-at-test-time: it enumerates every
// IndexType* constant from the index.go source (via go:embed) and asserts each is
// explicitly classified into exactly ONE query-candidacy bucket. Add a new index
// type without classifying it here (and wiring GetMatchCandidates) and this test
// goes red — the silent-value-candidate regression is caught before it ships.
//
// It also cross-checks IsAtomicMutationIndex: every atomic-mutation type MUST be
// diverted from value candidacy (that is the F35 invariant).
func TestIndexScanCandidacy_EveryTypeClassified(t *testing.T) {
	t.Parallel()

	// Diverted BEFORE the value-candidate append in GetMatchCandidates:
	//   - aggregate candidate (tryAggregateIndexCandidate): permuted MIN/MAX +
	//     the COUNT/SUM totals;
	//   - vector candidate (tryVectorIndexCandidate);
	//   - atomic-mutation running-extremum (_EVER) / bitmap (the IsAtomicMutationIndex
	//     guard from F35).
	// None of these reach value-index candidacy.
	divertedFromValueCandidacy := map[string]string{
		IndexTypeCount:          "aggregate/atomic",
		IndexTypeCountNotNull:   "aggregate/atomic",
		IndexTypeCountUpdates:   "aggregate/atomic",
		IndexTypeSum:            "aggregate/atomic",
		IndexTypeMaxEverLong:    "atomic (_EVER)",
		IndexTypeMinEverLong:    "atomic (_EVER)",
		IndexTypeMaxEverTuple:   "atomic (_EVER)",
		IndexTypeMinEverTuple:   "atomic (_EVER)",
		IndexTypeMaxEverVersion: "atomic (_EVER)",
		IndexTypeBitmapValue:    "atomic (bitmap)",
		IndexTypePermutedMin:    "aggregate (permuted)",
		IndexTypePermutedMax:    "aggregate (permuted)",
		IndexTypeVector:         "vector",
		IndexTypeVectorSPFresh:  "vector",
	}
	// Reaches value-index candidacy (the defs append). value/version/rank are
	// genuinely BY_VALUE-scannable (Java parity — their maintainers support a
	// BY_VALUE scan). text/multidimensional/time_window_leaderboard currently also
	// reach the append; whether they produce a USABLE value candidate (their roots
	// are not plain value expressions) is a separate open axis flagged for the
	// re-audit, but they are classified here so the completeness check is
	// exhaustive.
	reachesValueCandidacy := map[string]bool{
		IndexTypeValue:                 true,
		IndexTypeVersion:               true,
		IndexTypeRank:                  true,
		IndexTypeText:                  true,
		IndexTypeMultidimensional:      true,
		IndexTypeTimeWindowLeaderboard: true,
	}

	re := regexp.MustCompile(`IndexType\w+\s+=\s+"([^"]+)"`)
	matches := re.FindAllStringSubmatch(indexGoSource, -1)
	if len(matches) == 0 {
		t.Fatal("no IndexType* constants extracted from index.go — regex or const layout changed")
	}

	for _, m := range matches {
		typ := m[1]
		reason, diverted := divertedFromValueCandidacy[typ]
		reaches := reachesValueCandidacy[typ]

		// Exactly one bucket: diverted != reaches. Both false = a new unclassified
		// type (the F45 trip). Both true = a double-classification bug.
		if diverted == reaches {
			t.Errorf("index type %q not classified into exactly one query-candidacy bucket "+
				"(divertedFromValueCandidacy=%v reachesValueCandidacy=%v) — an unclassified type "+
				"silently becomes a VALUE-scan candidate (fail-open, F35 bug class). Classify its "+
				"candidacy in GetMatchCandidates and add it to this test (F45).", typ, diverted, reaches)
		}

		// The F35 invariant: an atomic-mutation index must never be a value candidate.
		if (&Index{Type: typ}).IsAtomicMutationIndex() && !diverted {
			t.Errorf("index type %q is IsAtomicMutationIndex==true but not diverted from value "+
				"candidacy — it would be scanned as a value source and read stale/aggregated data (F35)", typ)
		}
		if diverted {
			_ = reason // reason is documentation of WHY it's diverted; keep it referenced.
		}
	}
}
