package query

// RFC-173 producer census — the instrument that lets the atomic name-model cap
// prove "zero producers" and lets the remaining producer-zeroing be sequenced
// against a live inventory rather than guessed at.
//
// The name-model ("AnchoredJoin") result-value producers are buildJoinResultValue
// (P4) and buildUnnestResultValue (P5); the re-enumeration/legColumns producers
// (P1/P2/P3) are DERIVED — they only fire under an already-name-model parent, so
// they retire for free once P4/P5 reach zero. The design consult established
// (empirically, by gate census + runtime instrumentation) that every RESIDUAL P4/P5
// firing happens ONLY under enclosure (t.inInnerCluster == true): a FRESH
// (un-enclosed) box/join/unnest shape already ordinalizes. So the residual
// name-model set is exactly the inInnerCluster shapes, and the way to zero it is to
// starve inInnerCluster by ordinalizing the enclosure ROOTS (the name-model unnest
// lowering, correlated-scalar-in-projection, recursive-CTE) — NOT by flipping the
// enclosure declines directly.
//
// This census makes that invariant a testable gate: a permanent corpus run asserts
// every P4/P5 firing is Enclosed, and dumps the shape histogram as the sequencing
// inventory. An un-enclosed firing is a fresh-shape producer — a real bug or a
// falsification of the reframe — and is loud, never carved out.
//
// The observer is a nil-in-production process global (like forceOrdinalSpike /
// SetNameModelOracle): zero production cost, no behavior change. Set only by the
// census harness at a phase barrier with no translation in flight.

// ProducerCensusRecord is one name-model result-value producer firing.
type ProducerCensusRecord struct {
	Producer string // "P4" (buildJoinResultValue) or "P5" (buildUnnestResultValue)
	Enclosed bool   // t.inInnerCluster at the firing — the reframe invariant asserts this is always true
	Shape    string // coarse operator-shape descriptor (diagnostics / inventory only)
}

// producerCensusObserver, when non-nil, receives every P4/P5 name-model producer
// firing. Nil in production.
var producerCensusObserver func(ProducerCensusRecord)

// SetProducerCensusObserver installs (or, with nil, clears) the census hook.
// Test-only; the caller must guarantee no translation is in flight — the observer
// is a process global, exactly like SetForceOrdinalSpike / SetNameModelOracle.
func SetProducerCensusObserver(f func(ProducerCensusRecord)) { producerCensusObserver = f }

// recordProducerCensus reports a producer firing to the observer if one is
// installed. A single non-nil-returning call to buildJoinResultValue /
// buildUnnestResultValue IS a name-model firing (the ordinal paths use separate
// seed builders), so this is called at the successful-production point of each.
func recordProducerCensus(rec ProducerCensusRecord) {
	if producerCensusObserver != nil {
		producerCensusObserver(rec)
	}
}
