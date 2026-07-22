package cascades

// RFC-186's REWRITING instrument, re-specified after the instrument-first
// probe refuted the RFC's original ==1-by-pruning premise (see
// designated_final.go and DIVERGENCES.md "virtual vs physical prune"):
// child groups legitimately cross REWRITING with their full canonical
// final set (the forced prune was tried and reverted — PLANNING cannot
// re-derive the RFC-153 shapes), so the enforceable invariant is not
// finals==1 but COHERENCE — the winner OptimizeGroup stamps and the
// designation the cost properties derive through must be the SAME
// expression, because both come from the same comparator. A violation
// means the comparator and the designation diverged (a cache-staleness or
// comparator-drift bug) and REWRITING costing is again history-dependent.

// SetVerifyRewritingCoherence turns the RFC-186 REWRITING coherence check
// on for this planner. The embedded plan harness enables it permanently —
// a violation there is a red test, not a log line.
func (p *Planner) SetVerifyRewritingCoherence(on bool) { p.verifyRewritingCoherence = on }

// RewritingCoherenceViolations returns what the coherence check recorded
// across the last Plan() call, when enabled. Empty means every REWRITING
// winner stamped by OptimizeGroup was identical to the group's designated
// final at stamp time — costing and pruning agreed everywhere.
func (p *Planner) RewritingCoherenceViolations() []string { return p.rewritingCoherenceViolations }
