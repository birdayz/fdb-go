package values

// reanchorUnowned drives the producer bridge with NO ownership information, so
// every root reaching it is eligible for the accessor-name fallback.
//
// This mode is TEST-ONLY and has no exported door. Every production caller
// passes an ownership set — a foreign root there comes back unchanged before
// the name match is ever consulted — so the arms below exercise machinery that
// production reaches only for roots the producer genuinely owns. They are kept
// because that machinery is the same code: the correlation-wins rule, the
// unique-slot requirement, the exact-type check and the nested-suffix carry all
// run inside the owned path too, and this is where each is pinned in isolation.
//
// If a future caller wants the unfiltered behaviour, it does not get it by
// calling something shorter — it has to state an ownership set, and the answer
// to "which roots does this producer own" is the review question.
func reanchorUnowned(
	value Value,
	producer Value,
	target QuantifiedObjectValue,
) (Value, error) {
	return reanchorValueThroughProducer(value, producer, target, nil)
}
