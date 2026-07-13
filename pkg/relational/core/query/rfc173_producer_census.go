package query

// RFC-173 producer census — the instrument that lets the atomic name-model cap
// prove "zero producers" and lets the remaining producer-zeroing be sequenced
// against a live inventory rather than guessed at.
//
// The name-model ("AnchoredJoin") result-value producers are buildJoinResultValue
// (P4) and buildUnnestResultValue (P5); the re-enumeration/legColumns producers
// (P1/P2/P3) are DERIVED — they only fire under an already-name-model parent, so
// they retire for free once P4/P5 reach zero. A single non-nil-returning call to
// P4/P5 IS a real name-model record in a plan (the ordinal paths use separate seed
// builders), so observing them is a complete census of the name-model surface.
//
// The residual name-model surface has TWO kinds of firing, and the census records
// the enclosure bit (was the producer under an enclosing cluster) to tell them
// apart:
//   - ENCLOSED firings (caller's entry enclosure true): a producer that is a leg of
//     a larger cluster. Many are zeroed by ordinalizing the enclosure ROOTS (the
//     name-model unnest lowering, correlated-scalar-in-projection, recursive-CTE).
//   - UN-ENCLOSED firings (caller's entry enclosure false): a FRESH shape that still
//     name-models on its own. The census CORRECTED the Outcome-B design consult
//     here — it claimed NO un-enclosed residual exists, but two classes were found,
//     so the residual set is NOT "exactly the inInnerCluster shapes" and enclosure
//     starvation alone cannot zero it; each un-enclosed class needs an explicit flip:
//       (a) P4 existential-over-multi-way-join — RETIRED by B1: a multi-way (>=3)
//           inner join under a WHERE EXISTS used to decline its whole join subtree
//           to name-model (the TOP join firing un-enclosed) because the flat-EXISTS
//           path lacked the N-way ordinal gather plain joins have. B1
//           (translateExistsOverGatheredCluster) translates the join as its own
//           gathered ordinal cluster and wraps the existential over it, so the
//           shape now fires ZERO producers — the census gate pins that zero as the
//           regression flip-sentinel.
//       (b) P5 top-level box-base (and other declining) chained-unnest lowering —
//           the LIVE un-enclosed class: a fresh chained unnest over a box base
//           declines to name-model and its OUTERMOST link fires un-enclosed — a
//           box-substrate shape observed at the top level (the SAME class is
//           enclosed when nested). So the box-substrate residuals are NOT
//           enclosed-only; they need explicit ordinalization (the box slices),
//           not enclosure starvation.
//
// The census gate (TestFDB_RFC173_ProducerCensus, dualwindow package) pins:
// SeedRunCorpus fires zero producers (fully ordinal), the probe shapes that
// ordinalize (incl. the B1-retired existential-over-multi-join) fire zero, and a
// flip on any is a regression to the name model.
//
// The observer is a nil-in-production process global (like
// SetNameModelOracle): each producer guards its record construction — including the
// %T shape formatting — behind the nil check, so production pays only a single
// pointer compare and no allocation. Set only by the census harness at a phase
// barrier with no translation in flight.

// ProducerCensusRecord is one name-model result-value producer firing.
type ProducerCensusRecord struct {
	Producer string // "P4" (buildJoinResultValue) or "P5" (buildUnnestResultValue)
	Enclosed bool   // was the producer under an enclosing cluster (its caller's ENTRY enclosure)
	Shape    string // coarse operator-shape descriptor (diagnostics / inventory only)
}

// producerCensusObserver, when non-nil, receives every P4/P5 name-model producer
// firing. Nil in production. Read directly (not through a wrapper) at each firing
// site so the record — and its %T formatting — is constructed only when observing.
var producerCensusObserver func(ProducerCensusRecord)

// SetProducerCensusObserver installs (or, with nil, clears) the census hook.
// Test-only; the caller must guarantee no translation is in flight — the observer
// is a process global, exactly like SetNameModelOracle.
func SetProducerCensusObserver(f func(ProducerCensusRecord)) { producerCensusObserver = f }
