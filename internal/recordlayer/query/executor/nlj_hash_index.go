// Portions derived from FoundationDB Record Layer (AggregateCursor.java,
// StreamGrouping.java, FlatMapPipelinedCursor.java,
// NumericAggregationValue.java, and others),
// Copyright 2015-2018 Apple Inc. and the FoundationDB project authors
// Copyright 2015-2021 Apple Inc. and the FoundationDB project authors
// Copyright 2015-2022 Apple Inc. and the FoundationDB project authors
// Licensed under the Apache License, Version 2.0; translated to Go and modified.

package executor

import (
	"math"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// tryBuildHashIndex attempts to build a hash index on the inner rows
// for equijoin predicates. If exactly one predicate is an equality
// comparison between an outer-leg and an inner-leg column reference —
// both PLAN-TIME BAKED (source-relative ordinal over the leg's
// QuantifiedObjectValue) — builds a hash map keyed by
// the inner operand's evaluated value. Keys are extracted by EVALUATING
// the operand against its leg row alone (evalLegKey) — the exact
// resolution the linear predicate path performs per pair — and then
// CANONICALIZED (normalizeNLJHashKey) so map-key equality agrees with
// cmpAny's promoting comparison, so the hash pre-filter can never
// disagree with the predicate re-check the way an independent name
// lookup or a raw-dynamic-type key could (a divergence there silently
// DROPS matching rows, since the hash only pre-filters candidates).
func (c *nljCursor) tryBuildHashIndex(innerAlias values.CorrelationIdentifier) {
	if len(c.preds) == 0 || len(c.innerRows) < 100 {
		return
	}
	outerVal, innerVal := extractEquijoinOperands(c.preds, c.outerAlias, innerAlias)
	if outerVal == nil || innerVal == nil {
		return
	}
	idx := make(map[any][]int, len(c.innerRows))
	for i, row := range c.innerRows {
		if row.Positional == nil {
			return
		}
		val, ok := evalLegHashKey(innerVal, c.innerCorr, c.innerLegRow(i))
		if !ok {
			// An operand that cannot resolve against the leg row — or whose
			// key has no hashable promotion-stable canonical form ([]byte,
			// message shapes, NaN) — declines the fast path
			// entirely: the linear path evaluates (and loud-errors) the same
			// predicate per pair via cmpAny, which handles all of them.
			return
		}
		// RFC-130: the hash index is additional resident memory beyond the
		// already-charged innerRows — one int per inner row plus, for a new
		// key, the key's own bytes. Charge it; a budget breach is captured in
		// buildErr and surfaced on the first OnNext (this constructor cannot
		// return an error).
		var charge int64 = nljHashEntryBytes
		if _, existing := idx[val]; !existing {
			charge += scalarValueBytes(val)
		}
		if err := c.st.ChargeMemory(charge); err != nil {
			c.buildErr = err
			c.hashIndex = nil
			return
		}
		c.chargedBytes += charge
		idx[val] = append(idx[val], i)
	}
	c.hashIndex = idx
	c.hashOuterVal = outerVal
}

// oneLegBinder binds exactly one leg correlation to its leg-local row — the
// single-sided twin of twoLegBinder for hash-key extraction, where only the
// key operand's own leg is in scope.
type oneLegBinder struct {
	id  values.CorrelationIdentifier
	leg values.OrdinalRow
}

func (b oneLegBinder) GetCorrelationBinding(id values.CorrelationIdentifier) (any, bool) {
	if id == b.id {
		return b.leg, true
	}
	return nil, false
}

// evalLegKey evaluates a baked equijoin operand against its leg row alone.
// ok=false (never an error) on any resolution failure — the caller declines
// the hash fast path and the linear predicate path surfaces the failure.
func evalLegKey(val values.Value, corr values.CorrelationIdentifier, leg values.OrdinalRow) (any, bool) {
	if leg == nil {
		return nil, false
	}
	v, err := val.Evaluate(&values.RowEvalContext{Correlations: oneLegBinder{id: corr, leg: leg}})
	if err != nil {
		return nil, false
	}
	return v, true
}

// evalLegHashKey evaluates a baked equijoin operand against its leg row and
// canonicalizes the result into the NLJ hash-key form. The ONLY entry point
// for both the index build and the probe — the two sides MUST normalize
// identically or matching rows silently vanish. ok=false on resolution
// failure or a key outside normalizeNLJHashKey's whitelist: the builder then
// declines the fast path entirely and the probe degrades to the full
// candidate list, leaving the linear cmpAny evaluation as the semantics of
// record.
func evalLegHashKey(val values.Value, corr values.CorrelationIdentifier, leg values.OrdinalRow) (any, bool) {
	v, ok := evalLegKey(val, corr, leg)
	if !ok {
		return nil, false
	}
	return normalizeNLJHashKey(v)
}

// normalizeNLJHashKey canonicalizes an equijoin key so Go map-key equality on
// the result agrees with cmpAny — the promoting comparison the predicate
// re-check applies to every surviving candidate pair. The invariant: two keys
// the linear path treats as EQUAL must normalize to the SAME map key (a
// false negative silently drops matching rows); extra bucket collisions are
// harmless — the per-pair re-check filters them.
//
//   - Every numeric promotes to float64. cmpAny compares pure-integer
//     pairs EXACTLY (values.CompareExactInts, across the signed/unsigned
//     halves) and mixed integer/float pairs as float64 — and float64(x)
//     depends only on the numeric VALUE, so any pair cmpAny calls EQUAL
//     lands in the same bucket. Distinct wide integers (int64 or uint64)
//     that collide at float64 precision are false POSITIVES only, filtered
//     by the exact per-pair re-check. Without this, int64(5) missed the
//     float64(5) bucket (BIGINT=DOUBLE dropped every match ≥100 inner
//     rows) and a covering-index float32 leg missed a stored-record
//     float64 bucket.
//   - NaN declines: cmpAny compares NaN via the Double.compare total
//     order (NaN = NaN is EQUAL, NaN vs any other float is not), while a
//     float64 NaN map key matches NOTHING — bucketing would silently
//     drop NaN=NaN matches (a false negative), so NaN keys take the
//     linear path where cmpAny answers.
//   - string and bool pass through: cmpAny's arms for them are exact
//     equality, same as map ==.
//   - [16]byte (UUID) passes through: array == is exactly cmpAny's
//     bytes.Compare(av[:], bv[:]) == 0.
//   - nil (NULL) passes through: a NULL bucket only produces candidates the
//     re-check rejects (equality on NULL is UNKNOWN), and declining instead
//     would nuke the fast path for any leg with one NULL key.
//   - Everything else declines. []byte and message/list shapes are
//     unhashable map keys (a []byte key PANICKED the build at exactly 100
//     inner rows: "hash of unhashable type: []uint8"). DATE and TIMESTAMP
//     values are text, so a probe key is never a time.Time.
func normalizeNLJHashKey(v any) (any, bool) {
	if v == nil {
		return nil, true
	}
	if f, _, numeric := values.ToFloat64(v); numeric {
		if math.IsNaN(f) {
			return nil, false
		}
		return f, true
	}
	switch v.(type) {
	case string, bool, [16]byte:
		return v, true
	}
	return nil, false
}

// nljHashEntryBytes is the per-inner-row cost charged for the NLJ hash index:
// the int slot in the bucket slice plus amortized map/slice-header overhead.
const nljHashEntryBytes int64 = 24

// extractEquijoinOperands extracts the outer- and inner-side operand VALUES
// from a single-equality equijoin predicate. Each operand must be a PLAN-TIME
// BAKED leg column reference — a single-accessor FieldValue over its leg's
// QuantifiedObjectValue (a source-relative bind) — whose
// correlation names one of the two legs. Anything else (a fused multi-accessor
// rebase, a computed operand, a reference to a buried leg of a lower join)
// declines the fast path: (nil, nil), the linear predicate path handles it.
func extractEquijoinOperands(preds []predicates.QueryPredicate, outerAlias, innerAlias values.CorrelationIdentifier) (outerVal, innerVal values.Value) {
	var o, in values.Value
	eqCount := 0
	for _, p := range preds {
		cp, ok := p.(*predicates.ComparisonPredicate)
		if !ok {
			continue
		}
		if cp.Comparison.Type != predicates.ComparisonEquals {
			continue
		}
		lIsOuter, lOK := bakedLegOperand(cp.Operand, outerAlias, innerAlias)
		rIsOuter, rOK := bakedLegOperand(cp.Comparison.Operand, outerAlias, innerAlias)
		if !lOK || !rOK {
			continue
		}
		switch {
		case lIsOuter && !rIsOuter:
			o, in = cp.Operand, cp.Comparison.Operand
			eqCount++
		case !lIsOuter && rIsOuter:
			o, in = cp.Comparison.Operand, cp.Operand
			eqCount++
		}
	}
	if eqCount != 1 {
		return nil, nil
	}
	return o, in
}

// bakedLegOperand reports whether v is a baked SINGLE-accessor column
// reference over one of the join's two legs, and which side it names
// (isOuter). Pinned (seed-rebased) and source-relative bakes both qualify —
// either resolves by its leg-local ordinal against the leg row. A FUSED
// multi-accessor path declines: its root ordinal addresses a merge-shaped
// intermediate, not the leg row. ok=false for every other shape.
func bakedLegOperand(v values.Value, outerAlias, innerAlias values.CorrelationIdentifier) (isOuter, ok bool) {
	fv, isFV := values.AsFieldValue(v)
	if !isFV || fv.Path().Len() != 1 {
		return false, false
	}
	qov, isQOV := values.AsQuantifiedObjectValue(fv.ChildValue())
	if !isQOV {
		return false, false
	}
	// The leg identity question, asked of identifiers rather than of their text.
	// SameLeg is the one answer to it: exact, because the alias namespaces here are
	// deliberately case-DISJOINT, and declining an unstated identity because an
	// unstated identity names nothing.
	switch {
	case values.SameLeg(qov.Correlation(), outerAlias):
		return true, true
	case values.SameLeg(qov.Correlation(), innerAlias):
		return false, true
	}
	return false, false
}
