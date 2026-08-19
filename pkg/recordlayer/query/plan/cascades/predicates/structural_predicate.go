package predicates

// FindStructuralPredicate returns the first predicate in p's tree that is
// STRUCTURAL — an instruction to the planner's matching machinery rather than a
// test a row can answer — and reports whether one was found.
//
// The three types are exactly the ones ToResidualPredicate converts, and they
// share a tell: each has an unconditional non-answer as its Eval, so none of
// them can ever contribute a TRUE.
//
//   - *ExistentialValuePredicate names the existential quantifier's alias. Once
//     its subplan has been lowered to a FirstOrDefault, no physical row carries
//     that alias.
//   - *PredicateWithValueAndRanges is an index search argument; its Eval is an
//     unconditional TriUnknown.
//   - *Placeholder is a matching-time marker; its Eval is an unconditional
//     TriUnknown. Java makes this a subtype of the sargable
//     (Placeholder.java:48); Go's is a standalone struct, which is precisely why
//     it has to be named here rather than caught by the sargable case.
//
// This exists as an INVARIANT rather than as a fix in each rule because the
// failure it guards is invisible: a structural predicate in a
// RecordQueryPredicatesFilterPlan makes the filter reject every row and report
// success, which is indistinguishable from an empty table. Three separate rules
// build physical filters, and requiring each to remember the conversion is how
// two of them came to forget it.
func FindStructuralPredicate(p QueryPredicate) (QueryPredicate, bool) {
	if p == nil {
		return nil, false
	}
	switch p.(type) {
	case *ExistentialValuePredicate, *PredicateWithValueAndRanges, *Placeholder:
		return p, true
	}
	for _, child := range p.Children() {
		if found, ok := FindStructuralPredicate(child); ok {
			return found, true
		}
	}
	return nil, false
}
