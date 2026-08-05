package cascades

import (
	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// A UNIQUE index constrains its entries, but not uniformly. Its EXEMPT SET is
// the entries on which the declaration constrains nothing, or constrains
// something finer than logical row equality — NULL key components (under NULLS
// DISTINCT the uniqueness check is SKIPPED, so one NULL prefix legitimately
// holds arbitrarily many entries) and raw-NaN key components. A consumer that
// wants to trust the declaration has to establish that the exempt set is EMPTY
// on the rows it will actually see.
//
// The metadata route establishes that for every possible stream: every key
// component declared NOT NULL and none FLOAT/DOUBLE. It is correct and it is
// INERT on the SQL surface, because the SQL DDL rejects a NOT NULL scalar column
// (deliberately, for Java parity — NOT NULL is accepted only on ARRAY types), so
// every SQL-expressible secondary unique index has a nullable key.
//
// This file is the second route: a predicate strictly between the scan and the
// consumer that rejects NULL on every key column empties the exempt set's NULL
// half ON THIS STREAM, which is all a consumer needs. It is what makes both
// consumers of properties.SecondaryUniqueKeyGloballyEnforced — the DISTINCT
// elision and the strict-ordering claim — reachable from an ordinary SQL query.
//
// The FLOAT/DOUBLE half is untouched here and must stay untouched: `d IS NOT
// NULL` admits every NaN encoding, so no NULL rejection says anything about the
// raw-NaN half.

// nullRejectingComparison reports whether a comparison of this kind, applied to
// a column, guarantees that no row with NULL in that column survives.
//
// This is Java's range-enclosure encoding read back, not a new nullability
// lattice. RangeConstraints.java:650-653 compiles Comparisons.Type.NOT_NULL —
// in the same case fall-through as GREATER_THAN — to Range.greaterThan(boundary)
// over the NULL boundary, and NULL sorts below every value in the tuple
// encoding. So "rejects NULL" is "compiles to a range strictly above the NULL
// boundary", and the question is which comparison kinds do.
//
// The enumeration is CLOSED and fails closed: an unlisted kind, including every
// kind added after this was written, rejects nothing. That direction is the safe
// one — a missed rejection costs an optimization, an invented one emits
// duplicate rows.
//
// It is deliberately NOT a call into canBeUsedInScanPrefix
// (range_constraints.go:161-173), and the difference is exactly one entry.
// NotDistinctFrom sits beside Equals there because for the SCAN-PREFIX question
// the two behave alike; for the NULL-REJECTION question they are opposites.
// `x IS NOT DISTINCT FROM NULL` is NULL-safe equality and is TRUE for a NULL x,
// so it ADMITS the rows this analysis exists to exclude. Reusing the scan-prefix
// list would be wrong in that one entry, silently, on the shape most likely to
// be written by someone who knows SQL's three-valued semantics.
func nullRejectingComparison(t predicates.ComparisonType) bool {
	switch t {
	case predicates.ComparisonIsNotNull:
		// The direct form: Java's NOT_NULL → Range.greaterThan(NULL).
		return true
	case predicates.ComparisonEquals:
		// Range.singleton(v) over a non-NULL boundary; NULL = anything is
		// UNKNOWN, never TRUE.
		return true
	case predicates.ComparisonLessThan, predicates.ComparisonLessThanOrEq,
		predicates.ComparisonGreaterThan, predicates.ComparisonGreaterThanEq:
		// NULL compares UNKNOWN against every operand, in both directions.
		return true
	case predicates.ComparisonStartsWith, predicates.ComparisonLike:
		// A NULL subject makes the match UNKNOWN.
		return true
	case predicates.ComparisonIn:
		// A NULL probe is UNKNOWN against any list, including an empty one.
		return true
	case predicates.ComparisonNotEquals:
		// NULL <> x is UNKNOWN, not TRUE. Listed explicitly because the
		// intuition "a negation admits what the positive excluded" is wrong
		// here: three-valued semantics drops the row either way.
		return true
	default:
		// Refused, and three refusals are load-bearing rather than incidental:
		//
		//   ComparisonIsNull            — admits ONLY NULL. The exact inverse.
		//   ComparisonNotDistinctFrom   — NULL-safe equality; TRUE for a NULL
		//                                 subject. The trap; see the doc above.
		//   ComparisonIsDistinctFrom    — also NULL-safe: NULL IS DISTINCT FROM
		//                                 1 is TRUE, so a NULL row survives.
		//
		// The TEXT_* family, SORT and the DISTANCE_RANK family fall here too,
		// none of them having been reasoned about for this question.
		return false
	}
}

// nullRejectedOrdinals returns the ordinals, in the scan row's layout, of the
// columns on which every row reaching expr is guaranteed non-NULL.
//
// Three restrictions define what counts, and each prevents a specific unsound
// admission:
//
//   - TOP-LEVEL CONJUNCTS ONLY. A predicate under an OR proves nothing (the
//     other disjunct may admit NULL), and one under a NOT is inverted. Neither
//     is descended into; only the implicit AND of a filter's predicate list, and
//     the explicit AndPredicates within it, are flattened.
//
//   - STRICTLY BETWEEN THE SCAN AND THE CONSUMER. The walk starts below the
//     consumer and ends at the scan, so a filter sitting ABOVE the consumer is
//     never collected. That filter cannot license anything: by the time it runs,
//     a DISTINCT that was elided has already emitted its duplicates.
//
//   - RESOLVED IN THIS LAYOUT. The operand must be a BARE, top-level FieldValue
//     whose ordinal provably indexes THIS scan row (RFC-197's boundary rule).
//     A conjunct over some other source's same-named column, over a computed
//     value, or over a nested path states nothing about this key and is dropped.
//
// Anything not recognised contributes nothing, which is the fail-closed
// direction: the caller's proof is a CONJUNCTION over key columns, so a missing
// ordinal declines the proof rather than widening it.
func nullRejectedOrdinals(
	expr expressions.RelationalExpression, layout values.OrdinalDomain,
) map[int]struct{} {
	if !layout.IsKnown() {
		return nil
	}
	ords := map[int]struct{}{}
	for _, pred := range conjunctsBetweenScanAnd(expr) {
		cmp, ok := pred.(*predicates.ComparisonPredicate)
		if !ok {
			continue
		}
		if !nullRejectingComparison(cmp.Comparison.Type) {
			continue
		}
		fv, isField := cmp.Operand.(*values.FieldValue)
		if !isField {
			continue
		}
		ord, stated := fv.OrdinalIn(layout)
		if !stated {
			continue
		}
		ords[ord] = struct{}{}
	}
	return ords
}

// conjunctsBetweenScanAnd collects the top-level conjuncts of every filter on
// the walk from expr down to the FullUnorderedScanExpression — the same walk
// findScanExpression and findRecordTypes take, over the same transparent logical
// operators, kept beside them for the reason stated at findScanExpression: the
// three answer different questions of one node, and a caller wanting the
// predicates must not have to re-derive the walk.
//
// Every operator the walk descends through either removes rows or reshapes them
// without adding any, so a NULL rejection established below survives to the top
// of the walk. A node this walk does not know is not descended into, so its
// subtree contributes nothing.
func conjunctsBetweenScanAnd(expr expressions.RelationalExpression) []predicates.QueryPredicate {
	var out []predicates.QueryPredicate
	for e := expr; e != nil; {
		var inner expressions.Quantifier
		switch typed := e.(type) {
		case *expressions.FullUnorderedScanExpression:
			return out
		case *expressions.LogicalFilterExpression:
			out = append(out, flattenConjuncts(typed.GetPredicates())...)
			inner = typed.GetInner()
		case *expressions.LogicalProjectionExpression:
			inner = typed.GetInner()
		case *expressions.LogicalSortExpression:
			inner = typed.GetInner()
		case *expressions.LogicalDistinctExpression:
			inner = typed.GetInner()
		case *expressions.LogicalUniqueExpression:
			inner = typed.GetInner()
		case *expressions.LogicalTypeFilterExpression:
			inner = typed.GetInner()
		default:
			return out
		}
		ref := inner.GetRangesOver()
		if ref == nil {
			return out
		}
		e = ref.Get()
	}
	return out
}
