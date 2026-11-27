// Portions derived from FoundationDB Record Layer (MessageHelpers.java,
// QueryResult.java, IndexOrphanBehavior.java, RecordCursor.java,
// and others),
// Copyright 2015-2018 Apple Inc. and the FoundationDB project authors
// Copyright 2015-2021 Apple Inc. and the FoundationDB project authors
// Copyright 2015-2022 Apple Inc. and the FoundationDB project authors
// Licensed under the Apache License, Version 2.0; translated to Go and modified.

package executor

import (
	"bytes"
	"context"
	"fmt"

	"fdb.dev/internal/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/internal/recordlayer/query/plan/cascades/values"
	"fdb.dev/internal/recordlayer/query/plan/plans"
	"fdb.dev/internal/recordlayer/vectorcodec"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer"
)

// vectorEfSearch is the efSearch a scan runs with. A VECTOR index runs Java's
// HnswVectorIndexEngine.efSearch over the scan limit, the option unchanged even
// below k. SPFresh raises the option to the limit in top-k mode, and its 0
// leaves its maintainer's own default.
func vectorEfSearch(indexType string, explicit *int, scanLimit int, selfLimiting bool) int {
	if indexType == recordlayer.IndexTypeVector {
		return recordlayer.HNSWEfSearch(explicit, scanLimit)
	}
	efSearch := 0
	if explicit != nil {
		efSearch = *explicit
	}
	if selfLimiting && efSearch != 0 && efSearch < scanLimit {
		efSearch = scanLimit
	}
	return efSearch
}

// defaultVectorEfSearch is the ordered stream's minimum scan horizon.
const defaultVectorEfSearch = 200

// executeVectorIndexScan runs a BY_DISTANCE K-NN scan over a VECTOR (HNSW)
// index: the partition-equality prefix selects the independent HNSW graph and
// the graph is traversed for the k nearest neighbors of the query vector.
// VECTOR indexes receive typed scan options; SPFresh uses ScanIndexByType.
func executeVectorIndexScan(
	_ context.Context,
	p *plans.RecordQueryVectorIndexPlan,
	store *recordlayer.FDBRecordStore,
	evalCtx *EvaluationContext,
	continuation []byte,
	props recordlayer.ExecuteProperties,
) (recordlayer.RecordCursor[QueryResult], error) {
	idx := store.GetMetaData().GetIndex(p.GetIndexName())
	if idx == nil {
		return nil, fmt.Errorf("executor: vector index %q not found in metadata", p.GetIndexName())
	}
	if err := requireReadableVectorIndex(store, idx); err != nil {
		return nil, err
	}
	if err := validateVectorPartitionPlan(idx, p); err != nil {
		return nil, err
	}

	prefixComparisons, prefixTypes, err := vectorPartitionEqualityPrefix(
		p.GetPrefixComparisons(),
		p.GetPartitionKeyComponentTypes(),
	)
	if err != nil {
		return nil, fmt.Errorf("executor: vector index %q partition prefix: %w", p.GetIndexName(), err)
	}

	queryVec, err := evalFloat64Slice(p.GetQueryVector(), evalCtx)
	if err != nil {
		return nil, fmt.Errorf("executor: vector index %q query vector: %w", p.GetIndexName(), err)
	}
	k, rankCap, empty, err := vectorScanRankCap(p, evalCtx, continuation)
	if err != nil {
		return nil, err
	}
	if empty {
		return recordlayer.Empty[QueryResult](), nil
	}

	scanType, scanLimit, efSearch, makeScanRange := vectorScanRangeConfig(idx, p, queryVec, rankCap)
	invocationRange := makeScanRange(nil)
	fingerprintSalt, err := vectorScanRangeFingerprintSalt(
		p,
		idx.Type,
		scanType,
		queryVec,
		k,
		rankCap,
		efSearch,
		invocationRange,
	)
	if err != nil {
		return nil, fmt.Errorf("executor: building vector scan execution identity for %q: %w", p.GetIndexName(), err)
	}
	partitionRangeSet, err := bindScanComparisonsToRangeSetWithTerminalWidening(
		prefixComparisons,
		prefixTypes,
		scanBindContext(evalCtx),
		false,
		fingerprintSalt,
		false,
	)
	if err != nil {
		return nil, fmt.Errorf("executor: building vector partition scan ranges for %q: %w", p.GetIndexName(), err)
	}
	scanProps := recordlayer.NewScanProperties(props)
	indexCursor, err := newScanRangeSetCursor(
		partitionRangeSet,
		continuation,
		scanProps,
		func(
			partitionRange recordlayer.TupleRange,
			innerContinuation []byte,
			childProperties recordlayer.ScanProperties,
		) (recordlayer.RecordCursor[*recordlayer.IndexEntry], error) {
			prefix, prefixErr := exactVectorPartitionPrefix(partitionRange)
			if prefixErr != nil {
				return nil, fmt.Errorf("vector index %q: %w", p.GetIndexName(), prefixErr)
			}
			if idx.Type == recordlayer.IndexTypeVector {
				return store.ScanVectorIndexWithOptions(idx, prefix, queryVec, scanLimit,
					recordlayer.VectorIndexScanOptions{EfSearch: p.GetEfSearch(), ReturnVectors: p.GetReturnVectors()},
					innerContinuation, childProperties), nil
			}
			return store.ScanIndexByType(
				idx,
				scanType,
				makeScanRange(prefix),
				innerContinuation,
				childProperties,
			), nil
		},
	)
	if err != nil {
		return nil, fmt.Errorf("executor: opening vector partition scan ranges for %q: %w", p.GetIndexName(), err)
	}
	result := newIndexFetchCursor(indexCursor, store, props.ScanState)
	return applySkipLimit(result, props.Skip, props.ReturnedRowLimit), nil
}

func vectorScanRankCap(
	p *plans.RecordQueryVectorIndexPlan,
	evalCtx *EvaluationContext,
	continuation []byte,
) (k int, rankCap int, empty bool, err error) {
	// The scan's rank cap. A top-k whose ADJUSTED cap is ≤ 0 — ROW_NUMBER() <= 0,
	// < 1, or a parameter `<= ?` / `< ?` bound to 0 or a negative — selects NO
	// rows, so return EMPTY rather than erroring. An eager positive-only eval
	// rejected k ≤ 0, which made `<= 0` / `<= ?`(=0) error out BEFORE the
	// Limit(0)/Limit(?) above could cull to empty (RFC-156 correctness-hunt bug:
	// only `< 1`, where the comparand K=1 survives the positive check, was
	// handled). Evaluate tolerantly and
	// short-circuit the non-positive adjusted cap here, once, for BOTH the
	// ordered-stream and self-limiting branches below.
	k, err = evalRankCap(p.GetK(), evalCtx)
	if err != nil {
		return 0, 0, false, fmt.Errorf("executor: vector index %q top-K: %w", p.GetIndexName(), err)
	}
	rankCap = k
	if p.GetRankType() == predicates.ComparisonDistanceRankLessThan {
		// `< K` selects the top K-1. K ≤ 1 ⇒ no rows; test BEFORE subtracting so a
		// K = math.MinInt64 (literal `< -9223372036854775808`, or a bound param)
		// cannot wrap k-1 to a huge POSITIVE and slip past the ≤0 guard into an
		// enormous horizon. K ≥ 2 here ⇒ k-1 cannot overflow.
		if k <= 1 {
			if err := rejectContinuationForEmptyVectorScan(continuation); err != nil {
				return 0, 0, false, err
			}
			return 0, 0, true, nil
		}
		rankCap = k - 1
	}
	if rankCap <= 0 { // `<= K` with K ≤ 0
		if err := rejectContinuationForEmptyVectorScan(continuation); err != nil {
			return 0, 0, false, err
		}
		return 0, 0, true, nil
	}
	return k, rankCap, false, nil
}

func vectorScanRangeConfig(
	idx *recordlayer.Index,
	p *plans.RecordQueryVectorIndexPlan,
	queryVec []float64,
	rankCap int,
) (scanType recordlayer.IndexScanType, scanLimit int, efSearch int, makeScanRange func(tuple.Tuple) recordlayer.TupleRange) {
	scanType = recordlayer.IndexScanByDistance
	scanLimit = rankCap
	selfLimiting := !p.IsOrderedStream()
	if p.IsOrderedStream() {
		// RFC-156 — VBASE distance-ordered mode: do NOT self-limit to k. Stream
		// rows in ascending distance order so the Filter ABOVE culls non-matching
		// rows and the Limit(k) ABOVE takes the true k nearest MATCHING rows.
		//
		// Phase C: dispatch through the STREAMING scan type. For SPFresh this is a
		// demand-driven cursor that widens its scanned horizon in batches as the
		// consumer pulls — admitting the next ε-pruned cells in d2 order, then
		// re-routing with a larger w up to a budget cap — so a rare residual whose
		// matches lie beyond the initial probe still returns the true k nearest
		// matching rows (or an honest ScanLimitReached if the budget is exhausted
		// first). HNSW has no posting cells to widen, so the dispatch falls back to
		// the fixed-horizon ScanByDistance (Phase B, unchanged).
		//
		// The High tuple still carries the re-rank budget c (the Phase B decoupling
		// from the probe width: efSearch passes UNCHANGED as the probe width, never
		// forced up to the horizon — coupling them was rejected in Phase B review).
		// SPFresh's streaming path ignores k/c and uses the budget cap; the HNSW
		// fallback reads (k=horizon, efSearch) as before.
		scanType = recordlayer.IndexScanByDistanceOrderedStream
		// Horizon = the scan budget for the ordered stream. It MUST be at least
		// the rank cap k: an un-partitioned HNSW index has no posting cells to
		// widen, so IndexScanByDistanceOrderedStream falls back to the
		// non-widening ScanByDistance where this horizon IS the hard k cap. A
		// fixed defaultVectorEfSearch (200) would then scan only 200 rows and
		// silently drop matches for a query whose rank cap exceeds it (e.g.
		// QUALIFY ROW_NUMBER() ... <= 300). rankCap (computed above) is the adjusted
		// cap (k for rank<=k, k-1 for rank<k), ≥ 1 after the ≤0 short-circuit.
		//
		// TRUNCATION CONTRACT (HNSW): the un-partitioned HNSW ordered stream is
		// FIXED-HORIZON — it does NOT widen on demand and never raises
		// ScanLimitReached (that demand-driven widening is SPFresh-only). This fix
		// makes the horizon ≥ k so the rank cap itself is never the truncator, but
		// a SELECTIVE residual Filter above can still exhaust the horizon before k
		// matching rows are found (a known HNSW limitation; SPFresh widens past
		// it). For SPFresh streaming this horizon is only a higher budget FLOOR —
		// the demand-driven cursor still widens beyond it as the consumer pulls.
		horizon := defaultVectorEfSearch
		if rankCap > horizon {
			horizon = rankCap
		}
		scanLimit = horizon
		makeScanRange = func(prefix tuple.Tuple) recordlayer.TupleRange {
			return recordlayer.VectorDistanceScanRangeOrdered(queryVec, horizon, efSearch, horizon, prefix)
		}
	} else {
		// Self-limiting (top-k) mode. The scan limit IS the adjusted rank cap
		// (Java's VectorIndexScanBounds.getAdjustedLimit: K for <=K, K-1 for <K) —
		// already computed as rankCap above and proven ≥ 1 by the ≤0 short-circuit
		// (a non-positive adjusted cap returned EMPTY there). No re-derive, and no
		// dead ≤0 check.
		limit := rankCap
		makeScanRange = func(prefix tuple.Tuple) recordlayer.TupleRange {
			return recordlayer.VectorDistanceScanRangeWithPrefix(queryVec, limit, efSearch, prefix)
		}
	}
	efSearch = vectorEfSearch(idx.Type, p.GetEfSearch(), scanLimit, selfLimiting)
	return scanType, scanLimit, efSearch, makeScanRange
}

// requireReadableVectorIndex is requireReadableQueryIndex admitting a
// sliding-window vector index, whose K-NN answer is its window by design.
func requireReadableVectorIndex(store *recordlayer.FDBRecordStore, idx *recordlayer.Index) error {
	if idx.GetPredicateProto().GetRowNumberWindowPredicate() == nil {
		return requireReadableQueryIndex(store, idx)
	}
	state, err := store.ReadIndexState(idx.Name)
	if err != nil {
		return err
	}
	if state != recordlayer.IndexStateReadable {
		return &recordlayer.IndexNotReadableError{IndexName: idx.Name, CurrentState: state}
	}
	return nil
}

func rejectContinuationForEmptyVectorScan(continuation []byte) error {
	if continuation == nil {
		return nil
	}
	return &recordlayer.ContinuationParseError{
		Message:  "vector scan continuation cannot resume an empty adjusted rank",
		RawBytes: bytes.Clone(continuation),
	}
}

// invalidVectorPartitionPlanError is returned before evaluating dynamic inputs
// or opening storage when a hand-built/stale vector plan disagrees with the
// metadata topology. Letting the maintainer interpret a shorter prefix would
// silently switch to multi-partition fan-out; that is especially invalid for
// an ordered-stream plan, because concatenated per-partition distance streams
// are not one globally distance-ordered stream.
type invalidVectorPartitionPlanError struct {
	indexName          string
	reason             string
	metadataPartitions int
	comparisonArity    int
	typeArity          int
}

func (e *invalidVectorPartitionPlanError) Error() string {
	return fmt.Sprintf(
		"executor: vector index %q has invalid partition plan: %s (metadata partitions=%d, comparison arity=%d, physical-type arity=%d)",
		e.indexName,
		e.reason,
		e.metadataPartitions,
		e.comparisonArity,
		e.typeArity,
	)
}

func validateVectorPartitionPlan(idx *recordlayer.Index, p *plans.RecordQueryVectorIndexPlan) error {
	comparisonArity := len(p.GetPrefixComparisons())
	typeArity := len(p.GetPartitionKeyComponentTypes())
	invalid := func(reason string, metadataPartitions int) error {
		return &invalidVectorPartitionPlanError{
			indexName:          idx.Name,
			reason:             reason,
			metadataPartitions: metadataPartitions,
			comparisonArity:    comparisonArity,
			typeArity:          typeArity,
		}
	}

	if idx.Type != recordlayer.IndexTypeVector && idx.Type != recordlayer.IndexTypeVectorSPFresh {
		return invalid(fmt.Sprintf("metadata index type is %q, want a vector index", idx.Type), 0)
	}

	partitionCount := 0
	if keyWithValue, ok := idx.RootExpression.(*recordlayer.KeyWithValueExpression); ok {
		inner := keyWithValue.InnerKey()
		if inner == nil {
			return invalid("KeyWithValue root has no inner key", 0)
		}
		partitionCount = keyWithValue.SplitPoint()
		if partitionCount < 0 || partitionCount >= inner.ColumnSize() {
			return invalid(
				fmt.Sprintf("KeyWithValue split point %d leaves no valid vector value in %d columns", partitionCount, inner.ColumnSize()),
				partitionCount,
			)
		}
	} else if idx.RootExpression == nil {
		return invalid("metadata root expression is nil", 0)
	}

	if comparisonArity != partitionCount || typeArity != partitionCount {
		return invalid("plan arity does not match the metadata KeyWithValue split point", partitionCount)
	}
	if p.IsOrderedStream() && partitionCount > 0 {
		return invalid("ordered distance streaming is unsupported for partitioned vector indexes", partitionCount)
	}
	if idx.Type == recordlayer.IndexTypeVectorSPFresh && partitionCount > 0 {
		return invalid("SPFresh does not support partitioned vector indexes", partitionCount)
	}
	return nil
}

// vectorPartitionEqualityPrefix extracts the contiguous bound equality run
// from the candidate's fixed-width partition comparison vector. Unbound
// trailing partition columns are represented by EmptyComparisonRange and are
// intentionally legal: the vector maintainer fans out over those remaining
// partitions. A constraint after a gap, or any non-equality in the physical
// prefix, is an invalid hand-built plan and must not be silently ignored.
func vectorPartitionEqualityPrefix(
	comparisons []*predicates.ComparisonRange,
	physicalTypes []values.Type,
) ([]*predicates.ComparisonRange, []values.Type, error) {
	prefixLength := 0
	seenGap := false
	for i, comparisonRange := range comparisons {
		if comparisonRange == nil || comparisonRange.IsEmpty() {
			seenGap = true
			continue
		}
		if seenGap {
			return nil, nil, fmt.Errorf("component %d is constrained after an unbound partition component", i)
		}
		if !comparisonRange.IsEquality() {
			return nil, nil, fmt.Errorf("component %d is not an equality", i)
		}
		prefixLength++
	}
	prefixComparisons := append([]*predicates.ComparisonRange(nil), comparisons[:prefixLength]...)
	prefixTypes := make([]values.Type, prefixLength)
	for i := range prefixTypes {
		prefixTypes[i] = values.UnknownType
		if i < len(physicalTypes) && physicalTypes[i] != nil {
			prefixTypes[i] = physicalTypes[i]
		}
	}
	return prefixComparisons, prefixTypes, nil
}

// exactVectorPartitionPrefix converts the binder's exact allOf range back to
// the partition tuple expected by the vector maintainer. Unlike an ordinary
// index, a vector partition cannot consume an inequality or a terminal range:
// each signed-zero choice must open one exact physical graph (or one exact
// partial-prefix fan-out managed by the maintainer).
func exactVectorPartitionPrefix(scanRange recordlayer.TupleRange) (tuple.Tuple, error) {
	if scanRange.LowEndpoint == recordlayer.EndpointTypeTreeStart &&
		scanRange.HighEndpoint == recordlayer.EndpointTypeTreeEnd &&
		len(scanRange.Low) == 0 && len(scanRange.High) == 0 {
		return nil, nil
	}
	if scanRange.LowEndpoint != recordlayer.EndpointTypeRangeInclusive ||
		scanRange.HighEndpoint != recordlayer.EndpointTypeRangeInclusive ||
		!bytes.Equal(scanRange.Low.Pack(), scanRange.High.Pack()) {
		return nil, fmt.Errorf("partition comparisons did not materialize one exact tuple prefix")
	}
	return append(tuple.Tuple(nil), scanRange.Low...), nil
}

// evalFloat64Slice evaluates a Value to a vector ([]float64). Accepts the
// runtime vector representations: []float64, []float32, and []any of numerics.
func evalFloat64Slice(v values.Value, binder values.ParameterBinder) ([]float64, error) {
	if v == nil {
		return nil, fmt.Errorf("nil query vector")
	}
	ev, err := v.Evaluate(binder)
	if err != nil {
		return nil, err
	}
	switch s := ev.(type) {
	case []byte:
		// Prepared VECTOR parameters retain their serialized precision, just as
		// stored vectors do when evaluated by scalar distance expressions.
		return vectorcodec.Deserialize(s)
	case []float64:
		return s, nil
	case []float32:
		out := make([]float64, len(s))
		for i, f := range s {
			out[i] = float64(f)
		}
		return out, nil
	case []any:
		out := make([]float64, len(s))
		for i, e := range s {
			f, ok := toFloat64Scalar(e)
			if !ok {
				return nil, fmt.Errorf("query vector element %d is not numeric (%T)", i, e)
			}
			out[i] = f
		}
		return out, nil
	default:
		return nil, fmt.Errorf("query vector is not a numeric slice (%T)", ev)
	}
}

// toLimitInt coerces an evaluated runtime LIMIT cap to int. Unlike
// evalPositiveInt it tolerates non-positive values (a 0/negative cap is a valid
// "no rows" LIMIT, not an error).
func toLimitInt(ev any) (int, bool) {
	switch n := ev.(type) {
	case int:
		return n, true
	case int32:
		return int(n), true
	case int64:
		return int(n), true
	default:
		return 0, false
	}
}

// evalRankCap evaluates a vector-scan rank cap (the ROW_NUMBER() comparand, which
// may be a bound parameter) to int, TOLERATING ≤ 0 — unlike evalPositiveInt. A
// non-positive adjusted cap means "select no rows", which the caller turns into an
// EMPTY result, not an error (`<= 0`, `< 1`, `<= ?`(=0)). Errors only on a nil
// value, an evaluation error, or a non-integer comparand.
func evalRankCap(v values.Value, binder values.ParameterBinder) (int, error) {
	if v == nil {
		return 0, fmt.Errorf("nil value")
	}
	ev, err := v.Evaluate(binder)
	if err != nil {
		return 0, err
	}
	n, ok := toLimitInt(ev)
	if !ok {
		return 0, fmt.Errorf("not an integer (%T)", ev)
	}
	return n, nil
}

func toFloat64Scalar(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case float32:
		return float64(n), true
	case int:
		return float64(n), true
	case int32:
		return float64(n), true
	case int64:
		return float64(n), true
	case uint64:
		// Tuple decoding preserves positive integers above math.MaxInt64 as
		// uint64 (its only unsigned return) — a numeric value like any other.
		return float64(n), true
	case uint:
		return float64(n), true
	default:
		return 0, false
	}
}
