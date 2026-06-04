// Package executor bridges RecordQueryPlan trees (Cascades planner
// output) and the FDBRecordStore scanning API to produce
// RecordCursor[QueryResult] streams. Mirrors Java's
// RecordQueryPlan.executePlan dispatching to
// FDBRecordStoreBase.scanRecords.
//
// The executor is a standalone visitor (not a method on
// RecordQueryPlan) to avoid circular dependencies between the plans
// package and the recordlayer package.
package executor

import (
	"container/heap"
	"context"
	"fmt"
	"math"
	"slices"
	"sort"
	"strings"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"

	"github.com/birdayz/fdb-record-layer-go/gen"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb/tuple"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/predicates"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/plans"
)

type innerPlanAccessor interface{ GetInner() plans.RecordQueryPlan }

type RecursiveCTEDepthExceededError struct {
	MaxDepth int
}

func (e *RecursiveCTEDepthExceededError) Error() string {
	return fmt.Sprintf("recursive CTE exceeded maximum depth of %d", e.MaxDepth)
}

// AggregateTypeMismatchError is returned when MIN or MAX is applied to
// a non-numeric column. Java's fdb-relational rejects this with
// "VerifyException: unable to encapsulate aggregate operation due to
// type mismatch(es)" — the function registry only installs numeric
// MIN/MAX overloads.
type AggregateTypeMismatchError struct {
	Message string
}

func (e *AggregateTypeMismatchError) Error() string {
	return e.Message
}

type NumericRangeOverflowError struct {
	Value    any
	Column   string
	TypeName string
}

func (e *NumericRangeOverflowError) Error() string {
	return fmt.Sprintf("value %v out of range for %s column %q", e.Value, e.TypeName, e.Column)
}

type SumOverflowError struct{}

func (*SumOverflowError) Error() string { return "long overflow" }

// ExecutePlan executes a RecordQueryPlan tree against a store,
// returning a cursor over the results. Recursive — child plans are
// executed first, then the parent operator is applied.
func ExecutePlan(
	ctx context.Context,
	plan plans.RecordQueryPlan,
	store *recordlayer.FDBRecordStore,
	evalCtx *EvaluationContext,
	continuation []byte,
	props recordlayer.ExecuteProperties,
) (recordlayer.RecordCursor[QueryResult], error) {
	switch p := plan.(type) {
	case *plans.RecordQueryScanPlan:
		return executeScan(ctx, p, store, evalCtx, continuation, props)
	case *plans.RecordQueryIndexPlan:
		return executeIndexScan(ctx, p, store, evalCtx, continuation, props)
	case *plans.RecordQueryVectorIndexPlan:
		return executeVectorIndexScan(ctx, p, store, evalCtx, continuation, props)
	case *plans.RecordQueryTypeFilterPlan:
		return executeTypeFilter(ctx, p, store, evalCtx, continuation, props)
	case *plans.RecordQueryFilterPlan:
		return executeFilter(ctx, p, store, evalCtx, continuation, props)
	case *plans.RecordQueryLimitPlan:
		return executeLimit(ctx, p, store, evalCtx, continuation, props)
	case *plans.RecordQueryDistinctPlan:
		return executeDistinct(ctx, p, store, evalCtx, continuation, props)
	case *plans.RecordQueryProjectionPlan:
		return executeProjection(ctx, p, store, evalCtx, continuation, props)
	case *plans.RecordQuerySortPlan:
		return executeSort(ctx, p, store, evalCtx, continuation, props)
	case *plans.RecordQueryUnionPlan:
		return executeUnion(ctx, p, store, evalCtx, continuation, props)
	case *plans.RecordQueryIntersectionPlan:
		return executeIntersection(ctx, p, store, evalCtx, continuation, props)
	case *plans.RecordQueryNestedLoopJoinPlan:
		return executeNestedLoopJoin(ctx, p, store, evalCtx, continuation, props)
	case *plans.RecordQueryStreamingAggregationPlan:
		return executeAggregation(ctx, p.GetInner(), p.GetGroupingKeys(), p.GetAggregates(), store, evalCtx, continuation, props)
	case *plans.RecordQueryExplodePlan:
		return executeExplode(p, evalCtx, props)
	case *plans.RecordQueryDeletePlan:
		return executeDelete(ctx, p, store, evalCtx, continuation, props)
	case *plans.RecordQueryInsertPlan:
		return executeInsert(ctx, p, store, evalCtx, continuation, props)
	case *plans.RecordQueryUpdatePlan:
		return executeUpdate(ctx, p, store, evalCtx, continuation, props)
	case *plans.RecordQueryTempTableScanPlan:
		return executeTempTableScan(p, evalCtx, props)
	case *plans.RecordQueryTempTableInsertPlan:
		return executeTempTableInsert(ctx, p, store, evalCtx, continuation, props)
	case *plans.RecordQueryTableFunctionPlan:
		return executeTableFunction(p, evalCtx, props)
	case *plans.RecordQueryValuesPlan:
		return executeValues(p, evalCtx)
	case *plans.RecordQueryRecursiveLevelUnionPlan:
		return executeRecursiveLevelUnion(ctx, p, store, evalCtx, continuation, props)
	case *plans.RecordQueryRecursiveDfsJoinPlan:
		return executeRecursiveDfsJoin(ctx, p, store, evalCtx, continuation, props)
	case *plans.RecordQueryUnorderedUnionPlan:
		return executeUnorderedUnion(ctx, p, store, evalCtx, continuation, props)
	case *plans.RecordQueryPredicatesFilterPlan:
		return executePredicatesFilter(ctx, p, store, evalCtx, continuation, props)
	case *plans.RecordQueryMapPlan:
		return executeMap(ctx, p, store, evalCtx, continuation, props)
	case *plans.RecordQueryFirstOrDefaultPlan:
		return executeFirstOrDefault(ctx, p, store, evalCtx, continuation, props)
	case *plans.RecordQueryDefaultOnEmptyPlan:
		return executeDefaultOnEmpty(ctx, p, store, evalCtx, continuation, props)
	case *plans.RecordQueryInJoinPlan:
		return executeInJoin(ctx, p, store, evalCtx, continuation, props)
	case *plans.RecordQueryMergeSortUnionPlan:
		return executeMergeSortUnion(ctx, p, store, evalCtx, continuation, props)
	case *plans.RecordQueryInUnionPlan:
		return executeInUnion(ctx, p, store, evalCtx, continuation, props)

	case *plans.RecordQueryFlatMapPlan:
		return executeFlatMap(ctx, p, store, evalCtx, continuation, props)

	case *plans.RecordQueryFetchFromPartialRecordPlan:
		return executeFetchFromPartialRecord(ctx, p, store, evalCtx, continuation, props)

	case *plans.RecordQueryAggregateIndexPlan:
		return executeAggregateIndexScan(ctx, p, store, evalCtx, continuation, props)

	case *plans.RecordQueryMultiIntersectionOnValuesPlan:
		return executeMultiIntersection(ctx, p, store, evalCtx, continuation, props)

	case *plans.RecordQueryLoadByKeysPlan:
		return executeLoadByKeys(ctx, p, store, evalCtx, props)

	// --- Go extensions (no Java equivalent) ---
	case *plans.RecordQueryInMemorySortPlan:
		return executeInMemorySort(ctx, p, store, evalCtx, continuation, props)

	default:
		return nil, fmt.Errorf("executor: unsupported plan type %T", plan)
	}
}

func executeScan(
	_ context.Context,
	p *plans.RecordQueryScanPlan,
	store *recordlayer.FDBRecordStore,
	evalCtx *EvaluationContext,
	continuation []byte,
	props recordlayer.ExecuteProperties,
) (recordlayer.RecordCursor[QueryResult], error) {
	scanProps := recordlayer.ScanProperties{
		ExecuteProperties:   props,
		Reverse:             p.IsReverse(),
		CursorStreamingMode: recordlayer.StreamingModeIterator,
	}

	// If the plan carries scan comparisons (PK predicates pushed down
	// by the Cascades planner), convert them to an FDB tuple range and
	// scan only that range. Mirrors Java's RecordQueryScanPlan.executePlan()
	// which calls comparisons.toTupleRange() → store.scanRecords(range).
	if comps := p.GetScanComparisons(); len(comps) > 0 {
		tupleRange, err := scanComparisonsToTupleRange(comps, evalCtx)
		if err != nil {
			return nil, fmt.Errorf("executor: building scan range for PK comparisons: %w", err)
		}

		// When the PK uses RecordTypeKey() as its first component, FDB
		// keys are prefixed with the record type discriminator. Prepend
		// it so the scan range matches the actual key structure.
		//
		// After prepending, constrain TreeStart/TreeEnd endpoints to
		// the record-type prefix. Without this, an inequality like
		// order_id > 0 with HighEndpoint=TreeEnd would scan past
		// this record type into other record types' key ranges —
		// the subspace contains ALL record types interleaved by their
		// RecordTypeKey prefix.
		types := p.GetRecordTypes()
		if len(types) == 1 {
			md := store.GetMetaData()
			rt := md.GetRecordType(types[0])
			if rt != nil && rt.PrimaryKey != nil && recordlayer.KeyExpressionHasRecordTypePrefix(rt.PrimaryKey) {
				rtk := rt.GetRecordTypeKey()
				tupleRange = tupleRange.Prepend(tuple.Tuple{rtk})
				// Clamp unbounded endpoints to the record-type prefix so
				// the scan stays within this type's key range.
				if tupleRange.HighEndpoint == recordlayer.EndpointTypeTreeEnd {
					tupleRange.High = tuple.Tuple{rtk}
					tupleRange.HighEndpoint = recordlayer.EndpointTypeRangeInclusive
				}
				if tupleRange.LowEndpoint == recordlayer.EndpointTypeTreeStart {
					tupleRange.Low = tuple.Tuple{rtk}
					tupleRange.LowEndpoint = recordlayer.EndpointTypeRangeInclusive
				}
			}
		}

		lowEP := tupleRange.LowEndpoint
		highEP := tupleRange.HighEndpoint
		if continuation != nil {
			if scanProps.Reverse {
				highEP = recordlayer.EndpointTypeContinuation
			} else {
				lowEP = recordlayer.EndpointTypeContinuation
			}
		}

		inner := store.ScanRecordsInRange(
			tupleRange.Low, tupleRange.High,
			lowEP, highEP,
			continuation, scanProps,
		)
		return recordlayer.MapCursor(inner, FromStoredRecord), nil
	}

	types := p.GetRecordTypes()
	var inner recordlayer.RecordCursor[*recordlayer.FDBStoredRecord[proto.Message]]
	if len(types) == 1 {
		inner = store.ScanRecordsByType(types[0], continuation, scanProps)
	} else {
		inner = store.ScanRecords(continuation, scanProps)
	}

	return recordlayer.MapCursor(inner, FromStoredRecord), nil
}

func executeIndexScan(
	ctx context.Context,
	p *plans.RecordQueryIndexPlan,
	store *recordlayer.FDBRecordStore,
	evalCtx *EvaluationContext,
	continuation []byte,
	props recordlayer.ExecuteProperties,
) (recordlayer.RecordCursor[QueryResult], error) {
	idx := store.GetMetaData().GetIndex(p.GetIndexName())
	if idx == nil {
		return nil, fmt.Errorf("executor: index %q not found in metadata", p.GetIndexName())
	}
	maintainer, err := store.GetIndexMaintainer(idx)
	if err != nil {
		return nil, fmt.Errorf("executor: getting index maintainer for %q: %w", p.GetIndexName(), err)
	}

	scanRange, err := scanComparisonsToTupleRange(p.GetScanComparisons(), evalCtx)
	if err != nil {
		return nil, fmt.Errorf("executor: building scan range for %q: %w", p.GetIndexName(), err)
	}

	scanProps := recordlayer.ScanProperties{
		ExecuteProperties:   props,
		Reverse:             p.IsReverse(),
		CursorStreamingMode: recordlayer.StreamingModeIterator,
	}

	indexCursor := maintainer.Scan(scanRange, continuation, scanProps)

	if p.IsCovering() {
		var pkCols []string
		if rts := p.GetRecordTypes(); len(rts) > 0 {
			if rt := store.GetMetaData().GetRecordType(rts[0]); rt != nil && rt.PrimaryKey != nil {
				pkCols = rt.PrimaryKey.FieldNames()
			}
		}
		return &coveringIndexCursor{
			inner:     indexCursor,
			columns:   p.GetCoveringColumns(),
			pkColumns: pkCols,
		}, nil
	}

	resultCursor := &indexFetchCursor{
		inner: indexCursor,
		store: store,
	}

	return resultCursor, nil
}

// defaultVectorEfSearch is the HNSW search-quality knob used when the query
// does not specify OPTIONS ef_search. ef_search must be >= k for a correct
// top-K result; the executor raises it to k when the configured value is lower.
const defaultVectorEfSearch = 200

// executeVectorIndexScan runs a BY_DISTANCE K-NN scan over a VECTOR (HNSW)
// index: the partition-equality prefix selects the independent HNSW graph and
// the graph is traversed for the k nearest neighbors of the query vector.
// Dispatches through ScanIndexByType(IndexScanByDistance), which the vector
// index maintainer services via ScanByDistance.
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

	// Partition prefix from the leading equality comparisons.
	var prefix tuple.Tuple
	for _, cr := range p.GetPrefixComparisons() {
		if cr == nil || !cr.IsEquality() {
			break
		}
		prefix = append(prefix, cr.GetEqualityComparison().Operand.Evaluate(evalCtx))
	}

	queryVec, err := evalFloat64Slice(p.GetQueryVector(), evalCtx)
	if err != nil {
		return nil, fmt.Errorf("executor: vector index %q query vector: %w", p.GetIndexName(), err)
	}
	k, err := evalPositiveInt(p.GetK(), evalCtx)
	if err != nil {
		return nil, fmt.Errorf("executor: vector index %q top-K: %w", p.GetIndexName(), err)
	}

	// Derive the scan limit from the rank operator, matching Java's
	// VectorIndexScanBounds.getAdjustedLimit: ROW_NUMBER() < K returns the top
	// K-1, ROW_NUMBER() <= K returns the top K. (= K is rejected upstream at the
	// DistanceRank comparison, so only < / <= reach here.) k is already ≥ 1
	// (evalPositiveInt), so limit ≥ 0; limit == 0 (ROW_NUMBER() < 1) selects no
	// rows.
	limit := k
	if p.GetRankType() == predicates.ComparisonDistanceRankLessThan {
		limit = k - 1
	}
	if limit <= 0 {
		return recordlayer.Empty[QueryResult](), nil
	}

	efSearch := defaultVectorEfSearch
	if p.GetEfSearch() != nil {
		efSearch = *p.GetEfSearch()
	}
	if efSearch < limit {
		efSearch = limit
	}

	scanRange := recordlayer.VectorDistanceScanRangeWithPrefix(queryVec, limit, efSearch, prefix)
	scanProps := recordlayer.ScanProperties{
		ExecuteProperties:   props,
		CursorStreamingMode: recordlayer.StreamingModeIterator,
	}
	indexCursor := store.ScanIndexByType(idx, recordlayer.IndexScanByDistance, scanRange, continuation, scanProps)
	return &indexFetchCursor{inner: indexCursor, store: store}, nil
}

// evalFloat64Slice evaluates a Value to a vector ([]float64). Accepts the
// runtime vector representations: []float64, []float32, and []any of numerics.
func evalFloat64Slice(v values.Value, binder values.ParameterBinder) ([]float64, error) {
	if v == nil {
		return nil, fmt.Errorf("nil query vector")
	}
	switch s := v.Evaluate(binder).(type) {
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
		return nil, fmt.Errorf("query vector is not a numeric slice (%T)", v.Evaluate(binder))
	}
}

// evalPositiveInt evaluates a Value to a positive int (the top-K comparand).
func evalPositiveInt(v values.Value, binder values.ParameterBinder) (int, error) {
	if v == nil {
		return 0, fmt.Errorf("nil value")
	}
	var k int
	switch n := v.Evaluate(binder).(type) {
	case int:
		k = n
	case int32:
		k = int(n)
	case int64:
		k = int(n)
	default:
		return 0, fmt.Errorf("not an integer (%T)", v.Evaluate(binder))
	}
	if k <= 0 {
		return 0, fmt.Errorf("must be positive, got %d", k)
	}
	return k, nil
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
	default:
		return 0, false
	}
}

func scanComparisonsToTupleRange(comparisons []*predicates.ComparisonRange, binder values.ParameterBinder) (recordlayer.TupleRange, error) {
	if len(comparisons) == 0 {
		return recordlayer.TupleRangeAllOf(nil), nil
	}

	var prefix tuple.Tuple
	for _, cr := range comparisons {
		if !cr.IsEquality() {
			break
		}
		comp := cr.GetEqualityComparison()
		val := comp.Operand.Evaluate(binder)
		prefix = append(prefix, val)
	}

	eqCount := len(prefix)
	if eqCount >= len(comparisons) {
		return recordlayer.TupleRangeAllOf(prefix), nil
	}

	nextRange := comparisons[eqCount]
	if nextRange.IsEmpty() {
		return recordlayer.TupleRangeAllOf(prefix), nil
	}

	if !nextRange.IsInequality() {
		return recordlayer.TupleRangeAllOf(prefix), nil
	}

	var lowEndpoint, highEndpoint recordlayer.EndpointType
	var lowItem, highItem any
	hasLow := false
	hasHigh := false
	lowIsNullBoundary := false // low bound is the NULL exclusion (prefix + null, exclusive)

	if len(prefix) == 0 {
		lowEndpoint = recordlayer.EndpointTypeTreeStart
		highEndpoint = recordlayer.EndpointTypeTreeEnd
	} else {
		lowEndpoint = recordlayer.EndpointTypeRangeInclusive
		highEndpoint = recordlayer.EndpointTypeRangeInclusive
	}

	// Java's InequalityRangeCombiner keeps the *tightest* of multiple low (or
	// high) comparisons via Comparisons.compare(); here a later >/>= simply
	// wins last. That is harmless because upstream ComparisonRange merging has
	// already combined comparisons on the same column into one tightest range
	// before we get here, so this loop never sees two competing low bounds.
	for _, ineq := range nextRange.GetInequalityComparisons() {
		var comparand any
		if ineq.Operand != nil {
			comparand = ineq.Operand.Evaluate(binder)
		}
		// A NULL comparand makes an ordered inequality (<, <=, >, >=) UNKNOWN
		// for every row (SQL 3VL) → unsatisfiable → empty result. We must NOT
		// fall through to the endpoint logic: a `< NULL` would otherwise install
		// the NULL low boundary with a nil high, producing an inverted FDB range
		// (begin strinc(prefix,NULL) > end prefix). Return an explicit empty
		// range (begin == end). IS NOT NULL has no operand and is the legitimate
		// null-boundary case, handled below.
		switch ineq.Type {
		case predicates.ComparisonLessThan, predicates.ComparisonLessThanOrEq,
			predicates.ComparisonGreaterThan, predicates.ComparisonGreaterThanEq:
			if comparand == nil {
				return recordlayer.TupleRange{
					Low:          prefix,
					High:         prefix,
					LowEndpoint:  recordlayer.EndpointTypeRangeInclusive,
					HighEndpoint: recordlayer.EndpointTypeRangeExclusive,
				}, nil
			}
		}
		switch ineq.Type {
		case predicates.ComparisonGreaterThan:
			lowItem = comparand
			lowEndpoint = recordlayer.EndpointTypeRangeExclusive
			hasLow = true
		case predicates.ComparisonGreaterThanEq:
			lowItem = comparand
			lowEndpoint = recordlayer.EndpointTypeRangeInclusive
			hasLow = true
		case predicates.ComparisonLessThan:
			highItem = comparand
			highEndpoint = recordlayer.EndpointTypeRangeExclusive
			hasHigh = true
			// An upper-only range must EXCLUDE NULL index entries: NULL sorts
			// first in the index, and `col < v` is UNKNOWN (not TRUE) on NULL,
			// so those rows must not appear. Mirror Java
			// ScanComparisons.InequalityRangeCombiner: when no low bound is set,
			// pin the low to the NULL boundary (lowItem stays nil) RANGE_EXCLUSIVE,
			// which strinc's past the null prefix and skips every null entry.
			if !hasLow {
				lowEndpoint = recordlayer.EndpointTypeRangeExclusive
				lowIsNullBoundary = true
				hasLow = true
			}
		case predicates.ComparisonLessThanOrEq:
			highItem = comparand
			highEndpoint = recordlayer.EndpointTypeRangeInclusive
			hasHigh = true
			if !hasLow {
				lowEndpoint = recordlayer.EndpointTypeRangeExclusive
				lowIsNullBoundary = true
				hasLow = true
			}
		case predicates.ComparisonIsNotNull:
			// IS NOT NULL is the pure NULL-boundary range: everything strictly
			// after the null entries (Java: lowItem null, RANGE_EXCLUSIVE).
			if !hasLow {
				lowEndpoint = recordlayer.EndpointTypeRangeExclusive
				lowIsNullBoundary = true
				hasLow = true
			}
		}
	}

	// Build the endpoint tuples, mirroring Java's buildEndpointTuple:
	//   hasX  -> prefix + [item]; item==nil with a null boundary appends the
	//            NULL element (a low of (…,null) RANGE_EXCLUSIVE skips nulls).
	//   !hasX -> the prefix itself (if any), else unbounded (TREE_START/END).
	var low, high tuple.Tuple
	switch {
	case hasLow && lowItem != nil:
		low = append(append(tuple.Tuple{}, prefix...), lowItem)
	case hasLow && lowIsNullBoundary:
		low = append(append(tuple.Tuple{}, prefix...), nil)
	case len(prefix) > 0:
		low = prefix
	}
	if hasHigh && highItem != nil {
		high = append(append(tuple.Tuple{}, prefix...), highItem)
	} else if len(prefix) > 0 {
		high = prefix
	}

	return recordlayer.TupleRange{
		Low:          low,
		High:         high,
		LowEndpoint:  lowEndpoint,
		HighEndpoint: highEndpoint,
	}, nil
}

type indexFetchCursor struct {
	inner  recordlayer.RecordCursor[*recordlayer.IndexEntry]
	store  *recordlayer.FDBRecordStore
	closed bool
}

func (c *indexFetchCursor) OnNext(ctx context.Context) (recordlayer.RecordCursorResult[QueryResult], error) {
	for {
		if err := ctx.Err(); err != nil {
			return recordlayer.RecordCursorResult[QueryResult]{}, err
		}
		result, err := c.inner.OnNext(ctx)
		if err != nil {
			return recordlayer.NewResultNoNext[QueryResult](recordlayer.SourceExhausted, &recordlayer.EndContinuation{}), err
		}
		if !result.HasNext() {
			return recordlayer.NewResultNoNext[QueryResult](result.GetNoNextReason(), result.GetContinuation()), nil
		}

		entry := result.GetValue()
		pk := entry.PrimaryKey()
		if pk == nil {
			continue
		}

		rec, err := c.store.LoadRecord(pk)
		if err != nil {
			return recordlayer.NewResultNoNext[QueryResult](recordlayer.SourceExhausted, &recordlayer.EndContinuation{}), fmt.Errorf("executor: loading record for index entry pk %v: %w", pk, err)
		}
		if rec == nil {
			continue
		}

		qr := FromStoredRecord(rec)
		return recordlayer.NewResultWithValue(qr, result.GetContinuation()), nil
	}
}

func (c *indexFetchCursor) Close() error {
	c.closed = true
	return c.inner.Close()
}

func (c *indexFetchCursor) IsClosed() bool { return c.closed }

type coveringIndexCursor struct {
	inner     recordlayer.RecordCursor[*recordlayer.IndexEntry]
	columns   []string
	pkColumns []string
	closed    bool
}

func (c *coveringIndexCursor) OnNext(ctx context.Context) (recordlayer.RecordCursorResult[QueryResult], error) {
	result, err := c.inner.OnNext(ctx)
	if err != nil {
		return recordlayer.NewResultNoNext[QueryResult](recordlayer.SourceExhausted, &recordlayer.EndContinuation{}), err
	}
	if !result.HasNext() {
		return recordlayer.NewResultNoNext[QueryResult](result.GetNoNextReason(), result.GetContinuation()), nil
	}

	entry := result.GetValue()
	vals := entry.IndexValues()
	pk := entry.PrimaryKey()

	datum := make(map[string]any, len(c.columns)+len(c.pkColumns))
	for i, col := range c.columns {
		if i < len(vals) {
			datum[strings.ToUpper(col)] = vals[i]
		}
	}
	// PrimaryKey() may include a record type key prefix (e.g., (recTypeKey, id)).
	// The user-level PK columns are at the tail. Skip the prefix.
	pkOffset := 0
	if len(pk) > len(c.pkColumns) {
		pkOffset = len(pk) - len(c.pkColumns)
	}
	for i, col := range c.pkColumns {
		idx := i + pkOffset
		if idx < len(pk) {
			datum[strings.ToUpper(col)] = pk[idx]
		}
	}
	return recordlayer.NewResultWithValue(QueryResult{Datum: datum}, result.GetContinuation()), nil
}

func (c *coveringIndexCursor) Close() error {
	c.closed = true
	return c.inner.Close()
}

func (c *coveringIndexCursor) IsClosed() bool { return c.closed }

var _ recordlayer.RecordCursor[QueryResult] = (*coveringIndexCursor)(nil)

func executeTypeFilter(
	ctx context.Context,
	p *plans.RecordQueryTypeFilterPlan,
	store *recordlayer.FDBRecordStore,
	evalCtx *EvaluationContext,
	continuation []byte,
	props recordlayer.ExecuteProperties,
) (recordlayer.RecordCursor[QueryResult], error) {
	innerCursor, err := ExecutePlan(ctx, p.GetInner(), store, evalCtx, continuation, props.ClearSkipAndLimit())
	if err != nil {
		return nil, err
	}

	allowed := make(map[string]bool, len(p.GetRecordTypes()))
	for _, rt := range p.GetRecordTypes() {
		allowed[rt] = true
	}

	filtered := &filterResultCursor{
		inner: innerCursor,
		pred: func(qr QueryResult) bool {
			if qr.Record == nil || qr.Record.RecordType == nil {
				return false
			}
			return allowed[qr.Record.RecordType.Name]
		},
	}
	return applySkipLimit(filtered, props.Skip, props.ReturnedRowLimit), nil
}

func executeFilter(
	ctx context.Context,
	p *plans.RecordQueryFilterPlan,
	store *recordlayer.FDBRecordStore,
	evalCtx *EvaluationContext,
	continuation []byte,
	props recordlayer.ExecuteProperties,
) (recordlayer.RecordCursor[QueryResult], error) {
	innerCursor, err := ExecutePlan(ctx, p.GetInner(), store, evalCtx, continuation, props.ClearSkipAndLimit())
	if err != nil {
		return nil, err
	}

	preds := p.GetPredicates()
	needsRowCtx := len(evalCtx.params) > 0 || len(evalCtx.scalarSubqueries) > 0 || len(evalCtx.bindings) > 0
	filtered := &filterResultCursor{
		inner: innerCursor,
		pred: func(qr QueryResult) (keep bool) {
			defer func() {
				if r := recover(); r != nil {
					switch r.(type) {
					case *predicates.TypeMismatchError, *values.ArithmeticOverflowError, *values.ArithmeticDivisionByZeroError, *values.ScalarTypeMismatchError, *values.InvalidCastError:
						panic(r)
					}
					keep = false
				}
			}()
			var rowCtx any = qr.Datum
			if m, ok := qr.Datum.(map[string]any); ok {
				switch {
				case StrictReferenceCheck && qr.Complete:
					// RFC-048 W1: a HAVING/filter reference to a name absent from
					// a complete row (aggregate output) is a bug, not a NULL.
					rowCtx = evalCtx.RowContextStrict(m)
				case needsRowCtx:
					rowCtx = evalCtx.RowContext(m)
				}
			}
			for _, pred := range preds {
				if pred.Eval(rowCtx) != predicates.TriTrue {
					return false
				}
			}
			return true
		},
	}
	return applySkipLimit(filtered, props.Skip, props.ReturnedRowLimit), nil
}

// executeLimit implements LIMIT/OFFSET. Go-only SQL extension — Java
// uses ExecuteProperties.ReturnedRowLimit set at the JDBC layer instead.
//
// Optimization: propagates the effective row limit (limit + offset)
// into the inner plan's ExecuteProperties so downstream scans stop
// reading from FDB after enough records are produced. This avoids
// reading the full table when only N rows are needed.
func executeLimit(
	ctx context.Context,
	p *plans.RecordQueryLimitPlan,
	store *recordlayer.FDBRecordStore,
	evalCtx *EvaluationContext,
	continuation []byte,
	props recordlayer.ExecuteProperties,
) (recordlayer.RecordCursor[QueryResult], error) {
	children := p.GetChildren()
	if len(children) == 0 {
		return recordlayer.Empty[QueryResult](), nil
	}

	limit := int(p.GetLimit())
	offset := int(p.GetOffset())

	// Go-only extension: propagate effective limit to inner plan so
	// scans stop early. The inner needs at most (offset + limit) rows.
	innerProps := props
	effectiveLimit := offset + limit
	if effectiveLimit > 0 {
		if innerProps.ReturnedRowLimit == 0 || effectiveLimit < innerProps.ReturnedRowLimit {
			innerProps.ReturnedRowLimit = effectiveLimit
		}
	}

	innerCursor, err := ExecutePlan(ctx, children[0], store, evalCtx, continuation, innerProps)
	if err != nil {
		return nil, err
	}

	return recordlayer.SkipThenLimit(innerCursor, offset, limit), nil
}

// executeFetchFromPartialRecord executes a FetchFromPartialRecordPlan.
// In Java, this takes index entries (partial records) and fetches full
// records by PK. In Go, the index scan executor already returns full
// records, so the fetch is a pass-through that delegates to the inner.
// This exists as a safety net for plans where the Cascades optimizer
// didn't eliminate the fetch via MergeFetchIntoCoveringIndex or
// PushMapThroughFetch.
func executeFetchFromPartialRecord(
	ctx context.Context,
	p *plans.RecordQueryFetchFromPartialRecordPlan,
	store *recordlayer.FDBRecordStore,
	evalCtx *EvaluationContext,
	continuation []byte,
	props recordlayer.ExecuteProperties,
) (recordlayer.RecordCursor[QueryResult], error) {
	inner := p.GetInner()
	if inner == nil {
		return recordlayer.Empty[QueryResult](), nil
	}
	return ExecutePlan(ctx, inner, store, evalCtx, continuation, props)
}

func executeDistinct(
	ctx context.Context,
	p *plans.RecordQueryDistinctPlan,
	store *recordlayer.FDBRecordStore,
	evalCtx *EvaluationContext,
	continuation []byte,
	props recordlayer.ExecuteProperties,
) (recordlayer.RecordCursor[QueryResult], error) {
	innerCursor, err := ExecutePlan(ctx, p.GetInner(), store, evalCtx, continuation, props.ClearSkipAndLimit())
	if err != nil {
		return nil, err
	}

	seen := make(map[string]struct{})
	filtered := &filterResultCursor{
		inner: innerCursor,
		pred: func(qr QueryResult) bool {
			key := distinctKey(qr)
			if _, exists := seen[key]; exists {
				return false
			}
			seen[key] = struct{}{}
			return true
		},
	}
	return applySkipLimit(filtered, props.Skip, props.ReturnedRowLimit), nil
}

func distinctKey(qr QueryResult) string {
	m, ok := qr.Datum.(map[string]any)
	if !ok {
		return fmt.Sprintf("%T:%v", qr.Datum, qr.Datum)
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var sb strings.Builder
	for i, k := range keys {
		if i > 0 {
			sb.WriteByte('|')
		}
		sb.WriteString(k)
		sb.WriteByte('=')
		v := m[k]
		if v == nil {
			sb.WriteString("\x00NULL\x00")
		} else {
			fmt.Fprintf(&sb, "%T:%v", v, v)
		}
	}
	return sb.String()
}

func executeProjection(
	ctx context.Context,
	p *plans.RecordQueryProjectionPlan,
	store *recordlayer.FDBRecordStore,
	evalCtx *EvaluationContext,
	continuation []byte,
	props recordlayer.ExecuteProperties,
) (recordlayer.RecordCursor[QueryResult], error) {
	innerCursor, err := ExecutePlan(ctx, p.GetInner(), store, evalCtx, continuation, props.ClearSkipAndLimit())
	if err != nil {
		return nil, err
	}

	projections := p.GetProjections()
	aliases := p.GetAliases()
	needsRowCtx := len(evalCtx.params) > 0 || len(evalCtx.scalarSubqueries) > 0
	var evalErr error
	mapped := recordlayer.MapCursor(innerCursor, func(qr QueryResult) QueryResult {
		if evalErr != nil {
			return qr
		}
		projected := make(map[string]any, len(projections))
		var rowCtx any = qr.Datum
		if m, ok := qr.Datum.(map[string]any); ok {
			switch {
			case StrictReferenceCheck && qr.Complete:
				// RFC-048 W1: a projection reading a name absent from a complete
				// row (aggregate output) is a bug, not a NULL.
				rowCtx = evalCtx.RowContextStrict(m)
			case needsRowCtx:
				rowCtx = evalCtx.RowContext(m)
			}
		}
		for i, proj := range projections {
			func() {
				defer func() {
					if r := recover(); r != nil {
						switch e := r.(type) {
						case *values.ArithmeticDivisionByZeroError:
							evalErr = e
						case *values.ArithmeticOverflowError:
							evalErr = e
						case *values.ScalarTypeMismatchError:
							evalErr = e
						case *values.InvalidCastError:
							evalErr = e
						default:
							evalErr = fmt.Errorf("projection evaluation panic: %v", r)
						}
					}
				}()
				key := projectionColumnName(proj)
				val := proj.Evaluate(rowCtx)
				projected[key] = val
				// Also store under the alias so that outer projections
				// (e.g. CTE consumers) can resolve the aliased name.
				if i < len(aliases) && aliases[i] != "" {
					aliasKey := strings.ToUpper(aliases[i])
					if aliasKey != key {
						projected[aliasKey] = val
					}
				}
				// For computed expressions, also store under the
				// positional key (_0, _1, ...) so Java-compatible
				// column name lookups work.
				if _, isField := proj.(*values.FieldValue); !isField {
					posKey := fmt.Sprintf("_%d", i)
					if posKey != key {
						projected[posKey] = val
					}
				}
			}()
			if evalErr != nil {
				return qr
			}
		}
		return QueryResult{
			Datum:      projected,
			Record:     qr.Record,
			PrimaryKey: qr.PrimaryKey,
		}
	})
	errCursor := &errCheckCursor{inner: applySkipLimit(mapped, props.Skip, props.ReturnedRowLimit), err: &evalErr}
	return errCursor, nil
}

type errCheckCursor struct {
	inner recordlayer.RecordCursor[QueryResult]
	err   *error
}

func (c *errCheckCursor) OnNext(ctx context.Context) (recordlayer.RecordCursorResult[QueryResult], error) {
	if *c.err != nil {
		return recordlayer.RecordCursorResult[QueryResult]{}, *c.err
	}
	result, err := c.inner.OnNext(ctx)
	if err != nil {
		return result, err
	}
	if *c.err != nil {
		return recordlayer.RecordCursorResult[QueryResult]{}, *c.err
	}
	return result, nil
}

func (c *errCheckCursor) Close() error   { return c.inner.Close() }
func (c *errCheckCursor) IsClosed() bool { return c.inner.IsClosed() }

// executeSort implements ORDER BY. When a row limit is set (from a
// LIMIT clause pushed down via ExecuteProperties), uses a heap-based
// top-K algorithm that keeps only the needed rows in memory — O(K)
// space instead of O(N). Go-only extension optimization.
func executeSort(
	ctx context.Context,
	p *plans.RecordQuerySortPlan,
	store *recordlayer.FDBRecordStore,
	evalCtx *EvaluationContext,
	continuation []byte,
	props recordlayer.ExecuteProperties,
) (recordlayer.RecordCursor[QueryResult], error) {
	// Deserialize the sort continuation (if resuming). Extract the
	// inner continuation for the leaf cursor and the buffered records.
	// Mirrors Java's RecordQuerySortPlan + MemorySortCursorContinuation.
	var innerContinuation []byte
	var priorBuf []QueryResult

	if continuation != nil {
		ic, buf, decErr := decodeSortContinuation(continuation)
		if decErr != nil {
			return nil, fmt.Errorf("invalid sort continuation: %w", decErr)
		}
		innerContinuation = ic
		priorBuf = buf
	}

	innerCursor, err := ExecutePlan(ctx, p.GetInner(), store, evalCtx, innerContinuation, props.ClearSkipAndLimit())
	if err != nil {
		return nil, err
	}

	keys := p.GetSortKeys()
	keyNames := make([]string, len(keys))
	directions := make([]bool, len(keys))
	for i, k := range keys {
		keyNames[i] = k.Value.Name()
		if fv, ok := k.Value.(*values.FieldValue); ok {
			keyNames[i] = fv.Field
		}
		directions[i] = k.Reverse
	}

	cursor := newMemorySortCursor(innerCursor, keyNames, directions)
	if len(priorBuf) > 0 {
		cursor.buf = priorBuf
	}
	return applySkipLimit(cursor, props.Skip, props.ReturnedRowLimit), nil
}

func executeUnion(
	ctx context.Context,
	p *plans.RecordQueryUnionPlan,
	store *recordlayer.FDBRecordStore,
	evalCtx *EvaluationContext,
	continuation []byte,
	props recordlayer.ExecuteProperties,
) (recordlayer.RecordCursor[QueryResult], error) {
	inners := p.GetInners()
	if len(inners) == 0 {
		return recordlayer.Empty[QueryResult](), nil
	}

	var md *recordlayer.RecordMetaData
	if store != nil {
		md = store.GetRecordMetaData()
	}

	firstBranchKeys := planColumnNamesWithMD(inners[0], md)

	// If plan metadata gives us column names for all branches, stream
	// directly without buffering.
	if firstBranchKeys != nil {
		allKnown := true
		for i := 1; i < len(inners); i++ {
			if planColumnNamesWithMD(inners[i], md) == nil {
				allKnown = false
				break
			}
		}
		if allKnown {
			return executeUnionStreaming(ctx, inners, store, evalCtx, props, md, firstBranchKeys)
		}
	}

	// Fallback: need to peek rows to discover column names — buffer.
	return executeUnionBuffered(ctx, inners, store, evalCtx, continuation, props, md, firstBranchKeys)
}

func executeUnionStreaming(
	ctx context.Context,
	inners []plans.RecordQueryPlan,
	store *recordlayer.FDBRecordStore,
	evalCtx *EvaluationContext,
	props recordlayer.ExecuteProperties,
	md *recordlayer.RecordMetaData,
	targetKeys []string,
) (recordlayer.RecordCursor[QueryResult], error) {
	cursors := make([]recordlayer.RecordCursor[QueryResult], 0, len(inners))
	for i, inner := range inners {
		c, err := ExecutePlan(ctx, inner, store, evalCtx, nil, props.ClearSkipAndLimit())
		if err != nil {
			for _, prev := range cursors {
				prev.Close()
			}
			return nil, err
		}
		if i > 0 {
			srcKeys := planColumnNamesWithMD(inner, md)
			if srcKeys != nil && !slices.Equal(srcKeys, targetKeys) {
				c = recordlayer.MapCursor(c, func(qr QueryResult) QueryResult {
					return remapUnionColumnsByPosition(qr, srcKeys, targetKeys)
				})
			}
		}
		cursors = append(cursors, c)
	}
	return applySkipLimit(newConcatCursor[QueryResult](cursors), props.Skip, props.ReturnedRowLimit), nil
}

func executeUnionBuffered(
	ctx context.Context,
	inners []plans.RecordQueryPlan,
	store *recordlayer.FDBRecordStore,
	evalCtx *EvaluationContext,
	continuation []byte,
	props recordlayer.ExecuteProperties,
	md *recordlayer.RecordMetaData,
	firstBranchKeys []string,
) (recordlayer.RecordCursor[QueryResult], error) {
	var all []QueryResult
	for branchIdx, inner := range inners {
		cursor, err := ExecutePlan(ctx, inner, store, evalCtx, continuation, props.ClearSkipAndLimit())
		if err != nil {
			return nil, err
		}
		items, err := CollectAllBounded(ctx, cursor, props.GetMaterializationLimit(), "buffered union branch")
		cursor.Close()
		if err != nil {
			return nil, err
		}
		branchKeys := planColumnNames(inner)
		if branchIdx == 0 {
			firstBranchKeys = branchKeys
			if len(firstBranchKeys) == 0 && len(items) > 0 {
				if m, ok := items[0].Datum.(map[string]any); ok {
					firstBranchKeys = mapKeysOrdered(m)
				}
			}
		}
		if branchIdx > 0 && len(firstBranchKeys) > 0 {
			targetKeys := firstBranchKeys
			srcKeys := branchKeys
			if len(srcKeys) == 0 && len(items) > 0 {
				if m, ok := items[0].Datum.(map[string]any); ok {
					srcKeys = mapKeysOrdered(m)
				}
			}
			for i := range items {
				items[i] = remapUnionColumnsByPosition(items[i], srcKeys, targetKeys)
			}
		}
		all = append(all, items...)
	}
	return applySkipLimit(recordlayer.FromList(all), props.Skip, props.ReturnedRowLimit), nil
}

func mapKeysOrdered(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func planColumnNames(p plans.RecordQueryPlan) []string {
	return planColumnNamesWithMD(p, nil)
}

func planColumnNamesWithMD(p plans.RecordQueryPlan, md *recordlayer.RecordMetaData) []string {
	sawMap := false
	for {
		if proj, ok := p.(*plans.RecordQueryProjectionPlan); ok {
			projs := proj.GetProjections()
			names := make([]string, len(projs))
			aliases := proj.GetAliases()
			for i, v := range projs {
				if i < len(aliases) && aliases[i] != "" {
					names[i] = strings.ToUpper(aliases[i])
				} else {
					names[i] = projectionColumnName(v)
				}
			}
			return names
		}
		if _, ok := p.(*plans.RecordQueryMapPlan); ok {
			sawMap = true
		}
		if ip, ok := p.(innerPlanAccessor); ok {
			p = ip.GetInner()
		} else {
			break
		}
	}
	if rt, ok := p.GetResultType().(*values.RecordType); ok && len(rt.Fields) > 0 {
		names := make([]string, len(rt.Fields))
		for i, f := range rt.Fields {
			names[i] = strings.ToUpper(f.Name)
		}
		return names
	}
	if md != nil && !sawMap {
		if scan, ok := p.(*plans.RecordQueryScanPlan); ok && len(scan.GetRecordTypes()) == 1 {
			rt := md.GetRecordType(scan.GetRecordTypes()[0])
			if rt != nil && rt.Descriptor != nil {
				fields := rt.Descriptor.Fields()
				names := make([]string, fields.Len())
				for i := 0; i < fields.Len(); i++ {
					names[i] = strings.ToUpper(string(fields.Get(i).Name()))
				}
				return names
			}
		}
	}
	return nil
}

func remapUnionColumnsByPosition(qr QueryResult, srcKeys, targetKeys []string) QueryResult {
	m, ok := qr.Datum.(map[string]any)
	if !ok {
		return qr
	}
	if len(srcKeys) != len(targetKeys) {
		return qr
	}
	needsRemap := false
	for i := range srcKeys {
		if srcKeys[i] != targetKeys[i] {
			needsRemap = true
			break
		}
	}
	if !needsRemap {
		return qr
	}
	remapped := make(map[string]any, len(m))
	for i, srcKey := range srcKeys {
		remapped[targetKeys[i]] = m[srcKey]
	}
	return QueryResult{Datum: remapped, Record: qr.Record, PrimaryKey: qr.PrimaryKey}
}

func executeIntersection(
	ctx context.Context,
	p *plans.RecordQueryIntersectionPlan,
	store *recordlayer.FDBRecordStore,
	evalCtx *EvaluationContext,
	continuation []byte,
	props recordlayer.ExecuteProperties,
) (recordlayer.RecordCursor[QueryResult], error) {
	inners := p.GetInners()
	if len(inners) == 0 {
		return recordlayer.Empty[QueryResult](), nil
	}

	cursors, started, err := buildIntersectionChildCursors(ctx, inners, store, evalCtx, continuation, props)
	if err != nil {
		return nil, err
	}

	keyVals := p.GetComparisonKeyValues()
	compKeyFunc := intersectionCompKeyFunc(keyVals)
	return applySkipLimit(
		recordlayer.IntersectionResume(cursors, compKeyFunc, false, started),
		props.Skip, props.ReturnedRowLimit,
	), nil
}

// buildIntersectionChildCursors decodes a parent IntersectionContinuation into
// per-child resume states (RFC-071) and creates one cursor per child:
//   - START (!Started): ExecutePlan with a nil continuation (begin fresh),
//   - MID (Started + bytes): ExecutePlan resuming from the per-child bytes,
//   - END (Started + empty): an empty cursor — the child is exhausted, which
//     ends the intersection immediately (any exhausted child ends it).
//
// The returned `started` slice seeds each child's mergeChildState so the next
// checkpoint re-encodes MID/END children correctly rather than as START. With a
// nil/empty incoming continuation every child is START (unchanged first-page
// behavior). Shared by executeIntersection and executeMultiIntersection.
func buildIntersectionChildCursors(
	ctx context.Context,
	inners []plans.RecordQueryPlan,
	store *recordlayer.FDBRecordStore,
	evalCtx *EvaluationContext,
	continuation []byte,
	props recordlayer.ExecuteProperties,
) ([]recordlayer.RecordCursor[QueryResult], []bool, error) {
	resume, err := recordlayer.DecodeIntersectionContinuation(continuation, len(inners))
	if err != nil {
		return nil, nil, err
	}
	childProps := props.ClearSkipAndLimit()
	cursors := make([]recordlayer.RecordCursor[QueryResult], len(inners))
	started := make([]bool, len(inners))
	for i, inner := range inners {
		started[i] = resume[i].Started
		if resume[i].Started && len(resume[i].Continuation) == 0 {
			cursors[i] = recordlayer.Empty[QueryResult]() // END: exhausted child
			continue
		}
		c, cerr := ExecutePlan(ctx, inner, store, evalCtx, resume[i].Continuation, childProps)
		if cerr != nil {
			for _, prev := range cursors[:i] {
				if prev != nil {
					prev.Close()
				}
			}
			return nil, nil, cerr
		}
		cursors[i] = c
	}
	return cursors, started, nil
}

// intersectionCompKeyFunc builds a ComparisonKeyFunc that extracts a
// tuple-encoded comparison key from a QueryResult. Uses the plan's
// comparison-key values when available, falls back to PrimaryKey, then
// to a string representation of the datum.
func intersectionCompKeyFunc(keyVals []values.Value) recordlayer.ComparisonKeyFunc[QueryResult] {
	return func(qr QueryResult) tuple.Tuple {
		if len(keyVals) > 0 {
			t := make(tuple.Tuple, len(keyVals))
			for i, kv := range keyVals {
				t[i] = kv.Evaluate(qr.Datum)
			}
			return t
		}
		if qr.PrimaryKey != nil {
			return qr.PrimaryKey
		}
		return tuple.Tuple{fmt.Sprintf("%v", qr.Datum)}
	}
}

func executeFlatMap(
	ctx context.Context,
	p *plans.RecordQueryFlatMapPlan,
	store *recordlayer.FDBRecordStore,
	evalCtx *EvaluationContext,
	continuation []byte,
	props recordlayer.ExecuteProperties,
) (recordlayer.RecordCursor[QueryResult], error) {
	nestedProps := props.ClearSkipAndLimit()

	// Parse FlatMapContinuation if resuming.
	var outerCont, innerCont, checkValue []byte
	if len(continuation) > 0 {
		var fmc gen.FlatMapContinuation
		if err := proto.Unmarshal(continuation, &fmc); err == nil {
			outerCont = fmc.OuterContinuation
			innerCont = fmc.InnerContinuation
			checkValue = fmc.CheckValue
		}
	}

	outerCursor, err := ExecutePlan(ctx, p.GetOuter(), store, evalCtx, outerCont, nestedProps)
	if err != nil {
		return nil, err
	}

	cursor := newFlatMapCursor(
		outerCursor, p.GetInner(), store, evalCtx,
		p.GetOuterAlias(), p.GetInnerAlias(),
		p.GetResultValue(),
		p.IsLeftOuter(), p.IsExists(), p.IsNotExists(),
		nestedProps,
	)
	cursor.initialInnerCont = innerCont
	cursor.hasPendingInner = innerCont != nil
	cursor.pendingCheckValue = checkValue
	if innerCont != nil && outerCont != nil {
		cursor.lastOuterContinuation = recordlayer.NewBytesContinuation(outerCont)
	}
	return applySkipLimit(cursor, props.Skip, props.ReturnedRowLimit), nil
}

func executeNestedLoopJoin(
	ctx context.Context,
	p *plans.RecordQueryNestedLoopJoinPlan,
	store *recordlayer.FDBRecordStore,
	evalCtx *EvaluationContext,
	continuation []byte,
	props recordlayer.ExecuteProperties,
) (recordlayer.RecordCursor[QueryResult], error) {
	// Materialize the inner side once (typically the smaller table).
	// Clear TimeLimit for inner — the inner must be fully materialized
	// within this transaction. Java's FlatMapPipelinedCursor also
	// materializes the inner fully per outer row.
	innerProps := props.ClearSkipAndLimit().ClearRowAndTimeLimits()
	innerCursor, err := ExecutePlan(ctx, p.GetInner(), store, evalCtx, nil, innerProps)
	if err != nil {
		return nil, err
	}
	innerRows, err := CollectAllBounded(ctx, innerCursor, props.GetMaterializationLimit(), "nested loop join inner side")
	innerCursor.Close()
	if err != nil {
		return nil, err
	}

	// Stream the outer side one row at a time via nljCursor.
	outerProps := props.ClearSkipAndLimit()
	if p.GetJoinType() == plans.JoinFullOuter {
		// FULL OUTER accumulates cross-outer match state (the matchedInner
		// bitmap) that drives the post-outer drain phase, and that state is
		// NOT serialized into the continuation. The driver rebuilds the
		// cursor from scratch on each transaction page, which would reset
		// the bitmap mid-scan and produce wrong drain results. Clear the
		// outer's time/row limits so the whole FULL join completes within a
		// single transaction (one cursor, one bitmap). Very large FULL joins
		// then fail loudly at FDB's 5s transaction limit rather than
		// returning silently-wrong rows — the same limitation class as the
		// materialized inner side above. INNER/LEFT/RIGHT are unaffected:
		// they carry no cross-outer state and resume correctly per outer row.
		//
		// Consequence: with limits cleared the outer always runs to
		// SourceExhausted in one transaction and never emits a partial
		// continuation, so a fresh FULL query passes continuation=nil here
		// and the driver can never hand a FULL OUTER continuation back. The
		// `continuation` arg below is thus effectively always nil for FULL;
		// it is passed through unconditionally only for code uniformity.
		outerProps = outerProps.ClearRowAndTimeLimits()
	}
	outerCursor, err := ExecutePlan(ctx, p.GetOuter(), store, evalCtx, continuation, outerProps)
	if err != nil {
		return nil, err
	}

	cursor := newNLJCursor(
		outerCursor, innerRows,
		p.GetJoinType(), p.GetOuterAlias(), p.GetInnerAlias(),
		p.GetPredicates(), evalCtx,
	)
	return applySkipLimit(cursor, props.Skip, props.ReturnedRowLimit), nil
}

func mergeRows(outer, inner QueryResult, outerAlias, innerAlias string) QueryResult {
	outerMap, ok1 := outer.Datum.(map[string]any)
	innerMap, ok2 := inner.Datum.(map[string]any)
	if !ok1 || !ok2 {
		return QueryResult{Datum: outer.Datum, Record: outer.Record, PrimaryKey: outer.PrimaryKey}
	}

	merged := make(map[string]any, len(outerMap)+len(innerMap))
	outerType := recordTypeName(outer)
	innerType := recordTypeName(inner)

	outerQual := outerAlias
	if outerQual == "" {
		outerQual = outerType
	}
	innerQual := innerAlias
	if innerQual == "" {
		innerQual = innerType
	}

	// Pass A — bare keys. The outer writes every bare key; the inner only
	// overwrites bare keys when its namespace differs from the outer's (so a
	// self-join under one alias doesn't clobber the outer's columns).
	for k, v := range outerMap {
		merged[k] = v
	}
	for k, v := range innerMap {
		if strings.Contains(k, ".") {
			merged[k] = v
			continue
		}
		if innerQual == "" || innerQual != outerQual {
			merged[k] = v
		}
	}

	// Pass B — explicit-alias-qualified keys for BOTH legs. An explicit
	// table alias is authoritative: `t.id` must resolve to the leg the user
	// named `t`, regardless of join orientation. These are written before
	// the record-type fallback (Pass C) so that when one leg's alias equals
	// the OTHER leg's record type (e.g. self-join `FROM t, (SELECT ... FROM t) x`,
	// where the inner leg's alias `T` collides with the outer leg's type `T`),
	// the inner's alias-qualified `T.ID` wins over the outer's type fallback.
	// Without this ordering the outer's `outerType + ".ID"` fallback claimed
	// the `T.` namespace first and shadowed the inner alias (wrong results).
	qualifyAlias(merged, outerMap, outerAlias)
	qualifyAlias(merged, innerMap, innerAlias)

	// Pass C — record-type fallback for unaliased references (`FROM t` →
	// `t.col` where `t` is the type name). Only fills keys not already
	// claimed by an explicit alias above.
	qualifyTypeFallback(merged, outerMap, outerAlias, outerType)
	qualifyTypeFallback(merged, innerMap, innerAlias, innerType)

	return QueryResult{Datum: merged, Record: outer.Record, PrimaryKey: outer.PrimaryKey}
}

// qualifyAlias writes explicit-alias-qualified keys ("ALIAS.COL") for each
// bare column in src into dst. An explicit table alias is authoritative, so
// these keys are never overwritten by the record-type fallback. No-op when
// alias is empty (unaliased reference — handled by qualifyTypeFallback).
// Pre-qualified keys (containing a dot) carry their own namespace from a
// prior join level and are left untouched.
func qualifyAlias(dst, src map[string]any, alias string) {
	if alias == "" {
		return
	}
	for k, v := range src {
		if strings.Contains(k, ".") {
			continue
		}
		qualKey := alias + "." + strings.ToUpper(k)
		if _, exists := src[qualKey]; exists {
			// Already qualified under this alias by a prior level — keep it.
			continue
		}
		dst[qualKey] = v
	}
}

// qualifyTypeFallback writes record-type-qualified keys ("TYPE.COL") for
// unaliased table references. It only fills keys not already claimed by an
// explicit alias (qualifyAlias runs first), so a leg whose record type
// happens to equal another leg's explicit alias cannot shadow that alias.
func qualifyTypeFallback(dst, src map[string]any, alias, recType string) {
	if recType == "" {
		return
	}
	// When the alias equals the type, the alias pass already wrote TYPE.COL.
	// When alias is non-empty and differs, TYPE is only a fallback for
	// unaliased references; fill where absent. When alias is empty, TYPE is
	// the primary namespace.
	if alias == recType {
		return
	}
	for k, v := range src {
		if strings.Contains(k, ".") {
			continue
		}
		qualKey := recType + "." + strings.ToUpper(k)
		if _, exists := dst[qualKey]; exists {
			continue
		}
		dst[qualKey] = v
	}
}

// qualifyOuterRow builds a result row from an unmatched LEFT JOIN outer
// row, adding alias-qualified keys (e.g. "CUSTOMER.NAME") so that
// downstream projections using qualified column references resolve
// correctly. This mirrors the outer-half of mergeRows without an inner.
func qualifyOuterRow(outer QueryResult, outerAlias string) QueryResult {
	outerMap, ok := outer.Datum.(map[string]any)
	if !ok {
		return outer
	}
	qualified := make(map[string]any, len(outerMap)*2)
	outerType := recordTypeName(outer)
	outerQual := outerAlias
	if outerQual == "" {
		outerQual = outerType
	}
	for k, v := range outerMap {
		qualified[k] = v
		if strings.Contains(k, ".") {
			continue
		}
		if outerQual != "" {
			qualified[outerQual+"."+strings.ToUpper(k)] = v
		}
		if outerAlias != "" && outerType != "" && outerAlias != outerType {
			qualified[outerType+"."+strings.ToUpper(k)] = v
		}
	}
	return QueryResult{Datum: qualified, Record: outer.Record, PrimaryKey: outer.PrimaryKey}
}

func recordTypeName(qr QueryResult) string {
	if qr.Record != nil && qr.Record.Record != nil {
		return strings.ToUpper(string(qr.Record.Record.ProtoReflect().Descriptor().Name()))
	}
	return ""
}

func passesJoinPredicates(combined QueryResult, preds []predicates.QueryPredicate, evalCtx *EvaluationContext) bool {
	if len(preds) == 0 {
		return true
	}
	var rowCtx any = combined.Datum
	if len(evalCtx.params) > 0 || len(evalCtx.scalarSubqueries) > 0 || len(evalCtx.bindings) > 0 {
		if m, ok := combined.Datum.(map[string]any); ok {
			rowCtx = evalCtx.RowContext(m)
		}
	}
	for _, pred := range preds {
		if pred.Eval(rowCtx) != predicates.TriTrue {
			return false
		}
	}
	return true
}

func executeAggregation(
	ctx context.Context,
	inner plans.RecordQueryPlan,
	groupingKeys []values.Value,
	aggregates []expressions.AggregateSpec,
	store *recordlayer.FDBRecordStore,
	evalCtx *EvaluationContext,
	continuation []byte,
	props recordlayer.ExecuteProperties,
) (recordlayer.RecordCursor[QueryResult], error) {
	// Deserialize the aggregate continuation (if resuming from a
	// previous transaction). Extract the inner continuation for the
	// leaf cursor and the single in-progress group's partial state.
	// Mirrors Java's RecordQueryStreamingAggregationPlan.executePlan().
	var innerContinuation []byte
	var priorGroupKey string
	var priorState *groupState

	if continuation != nil {
		ic, gk, gs, decErr := decodeAggregateContinuation(continuation, len(aggregates))
		if decErr != nil {
			return nil, fmt.Errorf("invalid aggregate continuation: %w", decErr)
		}
		innerContinuation = ic
		priorGroupKey = gk
		priorState = gs
	}

	innerCursor, err := ExecutePlan(ctx, inner, store, evalCtx, innerContinuation, props.ClearSkipAndLimit())
	if err != nil {
		return nil, err
	}

	cursor := newAggregateCursor(innerCursor, groupingKeys, aggregates)
	if priorState != nil {
		cursor.withPartialState(priorGroupKey, priorState.keyVals, priorState)
	}
	return applySkipLimit(cursor, props.Skip, props.ReturnedRowLimit), nil
}

func toFloat64(v any) float64 {
	switch n := v.(type) {
	case int64:
		return float64(n)
	case float64:
		return n
	case int:
		return float64(n)
	case int32:
		return float64(n)
	case float32:
		return float64(n)
	default:
		return math.NaN()
	}
}

func aggKeyName(k values.Value) string {
	if fv, ok := k.(*values.FieldValue); ok {
		return strings.ToUpper(fv.Field)
	}
	return strings.ToUpper(values.ExplainValue(k))
}

func isNumeric(v any) bool {
	switch v.(type) {
	case int64, int32, int, float64, float32:
		return true
	}
	return false
}

func isCountStar(agg expressions.AggregateSpec) bool {
	if agg.Function != expressions.AggCount {
		return false
	}
	if agg.Operand == nil {
		return true
	}
	if cv, ok := agg.Operand.(*values.ConstantValue); ok && cv.Value == nil {
		return true
	}
	return false
}

func aggResultName(agg expressions.AggregateSpec) string {
	opName := "?"
	if agg.OperandName != "" {
		opName = strings.ReplaceAll(agg.OperandName, " ", "")
	} else if agg.Operand != nil {
		switch v := agg.Operand.(type) {
		case *values.ConstantValue:
			if v.Value == nil {
				opName = "*"
			} else {
				opName = v.Name()
			}
		case *values.FieldValue:
			opName = v.Field
		default:
			opName = values.ExplainValue(agg.Operand)
		}
	}
	switch agg.Function {
	case expressions.AggCount:
		return strings.ToUpper(fmt.Sprintf("COUNT(%s)", opName))
	case expressions.AggSum:
		return strings.ToUpper(fmt.Sprintf("SUM(%s)", opName))
	case expressions.AggMin:
		return strings.ToUpper(fmt.Sprintf("MIN(%s)", opName))
	case expressions.AggMax:
		return strings.ToUpper(fmt.Sprintf("MAX(%s)", opName))
	case expressions.AggAvg:
		return strings.ToUpper(fmt.Sprintf("AVG(%s)", opName))
	default:
		return strings.ToUpper(fmt.Sprintf("AGG(%s)", opName))
	}
}

func executeDelete(
	ctx context.Context,
	p *plans.RecordQueryDeletePlan,
	store *recordlayer.FDBRecordStore,
	evalCtx *EvaluationContext,
	continuation []byte,
	props recordlayer.ExecuteProperties,
) (recordlayer.RecordCursor[QueryResult], error) {
	innerCursor, err := ExecutePlan(ctx, p.GetInner(), store, evalCtx, continuation, props.ClearSkipAndLimit())
	if err != nil {
		return nil, err
	}

	var results []QueryResult
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		result, err := innerCursor.OnNext(ctx)
		if err != nil {
			return nil, err
		}
		if !result.HasNext() {
			break
		}
		qr := result.GetValue()
		if qr.PrimaryKey == nil {
			continue
		}
		deleted, err := store.DeleteRecord(qr.PrimaryKey)
		if err != nil {
			return nil, fmt.Errorf("executor: deleting record pk=%v: %w", qr.PrimaryKey, err)
		}
		if deleted {
			results = append(results, qr)
		}
	}
	return recordlayer.FromList(results), nil
}

func executeInsert(
	ctx context.Context,
	p *plans.RecordQueryInsertPlan,
	store *recordlayer.FDBRecordStore,
	evalCtx *EvaluationContext,
	continuation []byte,
	props recordlayer.ExecuteProperties,
) (recordlayer.RecordCursor[QueryResult], error) {
	innerCursor, err := ExecutePlan(ctx, p.GetInner(), store, evalCtx, continuation, props.ClearSkipAndLimit())
	if err != nil {
		return nil, err
	}

	// Materialize the inner rows BEFORE writing any record so that
	// INSERT … SELECT reading the target table doesn't re-scan its own
	// freshly-inserted rows (the Halloween problem). Bounded by the
	// materialization limit, the same guard the other buffering operators
	// use. (Note: a single INSERT that paginates across transactions can
	// still re-read across page boundaries — that extreme case is a known
	// limitation, RFC-035.)
	innerRows, err := CollectAllBounded(ctx, innerCursor, props.GetMaterializationLimit(), "INSERT source")
	innerCursor.Close()
	if err != nil {
		return nil, err
	}

	// Resolved lazily on the first computed-row datum.
	var targetDesc protoreflect.MessageDescriptor

	results := make([]QueryResult, 0, len(innerRows))
	for _, qr := range innerRows {
		// INSERT always coerces the inner result to the target type (Java's
		// InsertExpression computation value), so build from the row datum
		// — INSERT … VALUES (Explode of literal RecordConstructors) and
		// INSERT … SELECT (projection aliased to the target columns) both
		// produce a datum keyed by the target column names. A datum-less
		// stored record (rare) is saved as-is.
		var msg proto.Message
		switch datum := qr.Datum.(type) {
		case map[string]any:
			if targetDesc == nil {
				rt := store.GetMetaData().GetRecordType(p.GetTargetRecordType())
				if rt == nil {
					return nil, fmt.Errorf("executor: INSERT target record type %q not found", p.GetTargetRecordType())
				}
				targetDesc = rt.Descriptor
			}
			msg, err = buildInsertRecord(targetDesc, datum)
			if err != nil {
				return nil, err
			}
		default:
			if qr.Record == nil || qr.Record.Record == nil {
				continue
			}
			msg = qr.Record.Record
		}

		stored, err := store.SaveRecordWithOptions(msg, recordlayer.RecordExistenceCheckErrorIfExists)
		if err != nil {
			return nil, fmt.Errorf("executor: inserting record: %w", err)
		}
		results = append(results, FromStoredRecord(stored))
	}
	return recordlayer.FromList(results), nil
}

// buildInsertRecord materializes a proto message of the target record
// type from a computed row datum (column name → value). Used when the
// INSERT inner produces computed rows (the literal-VALUES Explode, or a
// projected SELECT) rather than stored records.
//
// It iterates the TARGET fields and pulls each from the datum
// (case-insensitively), ignoring datum keys that don't name a target
// column. This matters for INSERT … SELECT: the projection is aliased to
// the target columns, but the datum also carries the projection's own
// output names — those extra keys must be ignored, not error.
//
// For INSERT … VALUES, arity / NOT NULL / "expected Record but got
// Primitive" are enforced at plan time (buildInsertValuesArray). For
// INSERT … SELECT, a NULL projected into a NOT NULL column is NOT caught
// here — it falls through to the record store's Required-field marshal at
// save time (a less precise error than the plan-time NOT_NULL_VIOLATION).
// This matches Java, where proto enforcement is the backstop for dynamic
// sources; it's intentional, not an oversight.
func buildInsertRecord(desc protoreflect.MessageDescriptor, datum map[string]any) (proto.Message, error) {
	msg := dynamicpb.NewMessage(desc)
	refl := msg.ProtoReflect()
	folded := make(map[string]any, len(datum))
	for k, v := range datum {
		folded[strings.ToLower(k)] = v
	}
	fields := desc.Fields()
	for i := 0; i < fields.Len(); i++ {
		fd := fields.Get(i)
		v, ok := folded[strings.ToLower(string(fd.Name()))]
		if !ok || v == nil {
			continue // absent / NULL → leave field unset (SQL NULL)
		}
		// INSERT … VALUES pre-converts each field to a protoreflect.Value
		// at plan time (the relational ConvertToProtoValue handles enums
		// and nested records that goToProtoValue cannot); set it verbatim.
		// Projected-SELECT rows carry plain Go values, converted here.
		if pv, ok := v.(protoreflect.Value); ok {
			refl.Set(fd, pv)
			continue
		}
		pv, err := goToProtoValue(fd, v)
		if err != nil {
			return nil, err
		}
		refl.Set(fd, pv)
	}
	return msg, nil
}

// fieldByNameFold resolves a proto field by name, case-insensitively.
// Computed-row datums key columns by the SQL identifier casing, which
// need not match the proto descriptor's field-name casing.
func fieldByNameFold(fields protoreflect.FieldDescriptors, name string) protoreflect.FieldDescriptor {
	for i := 0; i < fields.Len(); i++ {
		fd := fields.Get(i)
		if strings.EqualFold(string(fd.Name()), name) {
			return fd
		}
	}
	return nil
}

func executeUpdate(
	ctx context.Context,
	p *plans.RecordQueryUpdatePlan,
	store *recordlayer.FDBRecordStore,
	evalCtx *EvaluationContext,
	continuation []byte,
	props recordlayer.ExecuteProperties,
) (recordlayer.RecordCursor[QueryResult], error) {
	innerCursor, err := ExecutePlan(ctx, p.GetInner(), store, evalCtx, continuation, props.ClearSkipAndLimit())
	if err != nil {
		return nil, err
	}

	transforms := p.GetTransforms()

	var results []QueryResult
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		result, err := innerCursor.OnNext(ctx)
		if err != nil {
			return nil, err
		}
		if !result.HasNext() {
			break
		}
		qr := result.GetValue()
		if qr.Record == nil || qr.Record.Record == nil {
			continue
		}

		msg := proto.Clone(qr.Record.Record)
		refl := msg.ProtoReflect()
		desc := refl.Descriptor()

		for _, t := range transforms {
			fd := desc.Fields().ByName(protoreflect.Name(strings.ToLower(t.FieldPath)))
			if fd == nil {
				fd = fieldByNameFold(desc.Fields(), t.FieldPath)
			}
			if fd == nil {
				return nil, fmt.Errorf("executor: update field %q not found in descriptor", t.FieldPath)
			}
			var rowCtx any = qr.Datum
			if len(evalCtx.params) > 0 || len(evalCtx.scalarSubqueries) > 0 {
				if m, ok := qr.Datum.(map[string]any); ok {
					rowCtx = evalCtx.RowContext(m)
				}
			}
			newVal := t.NewValue.Evaluate(rowCtx)
			if newVal == nil {
				refl.Clear(fd)
			} else {
				pv, err := goToProtoValue(fd, newVal)
				if err != nil {
					return nil, fmt.Errorf("executor: converting update value for %q: %w", t.FieldPath, err)
				}
				refl.Set(fd, pv)
			}
		}

		stored, err := store.SaveRecordWithOptions(msg, recordlayer.RecordExistenceCheckErrorIfNotExistsOrTypeChanged)
		if err != nil {
			return nil, fmt.Errorf("executor: updating record: %w", err)
		}
		results = append(results, FromStoredRecord(stored))
	}
	return recordlayer.FromList(results), nil
}

func goToProtoValue(fd protoreflect.FieldDescriptor, v any) (protoreflect.Value, error) {
	switch fd.Kind() {
	case protoreflect.BoolKind:
		switch b := v.(type) {
		case bool:
			return protoreflect.ValueOfBool(b), nil
		}
	case protoreflect.Int32Kind, protoreflect.Sint32Kind, protoreflect.Sfixed32Kind:
		switch n := v.(type) {
		case int64:
			if n < math.MinInt32 || n > math.MaxInt32 {
				return protoreflect.Value{}, &NumericRangeOverflowError{Value: n, Column: string(fd.Name()), TypeName: fd.Kind().String()}
			}
			return protoreflect.ValueOfInt32(int32(n)), nil
		case int32:
			return protoreflect.ValueOfInt32(n), nil
		case int:
			if int64(n) < math.MinInt32 || int64(n) > math.MaxInt32 {
				return protoreflect.Value{}, &NumericRangeOverflowError{Value: int64(n), Column: string(fd.Name()), TypeName: fd.Kind().String()}
			}
			return protoreflect.ValueOfInt32(int32(n)), nil
		}
	case protoreflect.Int64Kind, protoreflect.Sint64Kind, protoreflect.Sfixed64Kind:
		switch n := v.(type) {
		case int64:
			return protoreflect.ValueOfInt64(n), nil
		case int:
			return protoreflect.ValueOfInt64(int64(n)), nil
		}
	case protoreflect.Uint32Kind, protoreflect.Fixed32Kind:
		switch n := v.(type) {
		case int64:
			if n < 0 || n > math.MaxUint32 {
				return protoreflect.Value{}, &NumericRangeOverflowError{Value: n, Column: string(fd.Name()), TypeName: fd.Kind().String()}
			}
			return protoreflect.ValueOfUint32(uint32(n)), nil
		case uint32:
			return protoreflect.ValueOfUint32(n), nil
		}
	case protoreflect.Uint64Kind, protoreflect.Fixed64Kind:
		switch n := v.(type) {
		case int64:
			if n < 0 {
				return protoreflect.Value{}, &NumericRangeOverflowError{Value: n, Column: string(fd.Name()), TypeName: fd.Kind().String()}
			}
			return protoreflect.ValueOfUint64(uint64(n)), nil
		case uint64:
			return protoreflect.ValueOfUint64(n), nil
		}
	case protoreflect.FloatKind:
		switch n := v.(type) {
		case float64:
			if n > math.MaxFloat32 || n < -math.MaxFloat32 {
				return protoreflect.Value{}, &NumericRangeOverflowError{Value: n, Column: string(fd.Name()), TypeName: fd.Kind().String()}
			}
			return protoreflect.ValueOfFloat32(float32(n)), nil
		case float32:
			return protoreflect.ValueOfFloat32(n), nil
		}
	case protoreflect.DoubleKind:
		switch n := v.(type) {
		case float64:
			return protoreflect.ValueOfFloat64(n), nil
		}
	case protoreflect.StringKind:
		switch s := v.(type) {
		case string:
			return protoreflect.ValueOfString(s), nil
		}
	case protoreflect.BytesKind:
		switch b := v.(type) {
		case []byte:
			return protoreflect.ValueOfBytes(b), nil
		}
	case protoreflect.EnumKind:
		switch n := v.(type) {
		case int64:
			return protoreflect.ValueOfEnum(protoreflect.EnumNumber(n)), nil
		}
	}
	return protoreflect.Value{}, fmt.Errorf("cannot convert %T to proto field kind %v", v, fd.Kind())
}

func executeTempTableScan(
	p *plans.RecordQueryTempTableScanPlan,
	evalCtx *EvaluationContext,
	props recordlayer.ExecuteProperties,
) (recordlayer.RecordCursor[QueryResult], error) {
	tt := evalCtx.GetOrCreateTempTable(p.GetTempTableAlias())
	items := tt.GetList()
	return applySkipLimit(recordlayer.FromList(items), props.Skip, props.ReturnedRowLimit), nil
}

func executeTempTableInsert(
	ctx context.Context,
	p *plans.RecordQueryTempTableInsertPlan,
	store *recordlayer.FDBRecordStore,
	evalCtx *EvaluationContext,
	continuation []byte,
	props recordlayer.ExecuteProperties,
) (recordlayer.RecordCursor[QueryResult], error) {
	innerCursor, err := ExecutePlan(ctx, p.GetInner(), store, evalCtx, continuation, props.ClearSkipAndLimit())
	if err != nil {
		return nil, err
	}

	tt := evalCtx.GetOrCreateTempTable(p.GetTempTableAlias())

	mapped := recordlayer.MapCursor(innerCursor, func(qr QueryResult) QueryResult {
		tt.Add(qr)
		return qr
	})
	return mapped, nil
}

func executeTableFunction(
	p *plans.RecordQueryTableFunctionPlan,
	evalCtx *EvaluationContext,
	props recordlayer.ExecuteProperties,
) (recordlayer.RecordCursor[QueryResult], error) {
	sv := p.GetStreamValue()
	if sv == nil {
		return applySkipLimit(recordlayer.Empty[QueryResult](), props.Skip, props.ReturnedRowLimit), nil
	}
	result := sv.Evaluate(evalCtx)
	if result == nil {
		return applySkipLimit(recordlayer.Empty[QueryResult](), props.Skip, props.ReturnedRowLimit), nil
	}
	list, ok := result.([]any)
	if !ok {
		return applySkipLimit(
			recordlayer.FromList([]QueryResult{{Datum: result}}),
			props.Skip, props.ReturnedRowLimit,
		), nil
	}
	items := make([]QueryResult, len(list))
	for i, elem := range list {
		items[i] = QueryResult{Datum: elem}
	}
	return applySkipLimit(recordlayer.FromList(items), props.Skip, props.ReturnedRowLimit), nil
}

func executeExplode(
	p *plans.RecordQueryExplodePlan,
	evalCtx *EvaluationContext,
	props recordlayer.ExecuteProperties,
) (recordlayer.RecordCursor[QueryResult], error) {
	cv := p.GetCollectionValue()
	if cv == nil {
		return applySkipLimit(recordlayer.Empty[QueryResult](), props.Skip, props.ReturnedRowLimit), nil
	}
	result := cv.Evaluate(evalCtx)
	if result == nil {
		return applySkipLimit(recordlayer.Empty[QueryResult](), props.Skip, props.ReturnedRowLimit), nil
	}
	list, ok := result.([]any)
	if !ok {
		return applySkipLimit(
			recordlayer.FromList([]QueryResult{{Datum: result}}),
			props.Skip, props.ReturnedRowLimit,
		), nil
	}
	items := make([]QueryResult, len(list))
	for i, elem := range list {
		items[i] = QueryResult{Datum: elem}
	}
	return applySkipLimit(recordlayer.FromList(items), props.Skip, props.ReturnedRowLimit), nil
}

func executeValues(p *plans.RecordQueryValuesPlan, evalCtx *EvaluationContext) (recordlayer.RecordCursor[QueryResult], error) {
	cols := p.GetColumns()
	row := make(map[string]any, len(cols))
	for _, col := range cols {
		row[col.Name()] = col.Evaluate(evalCtx)
	}
	return recordlayer.FromList([]QueryResult{{Datum: row}}), nil
}

// executeRecursiveLevelUnion implements level-order (BFS) recursive
// CTE execution. Two temp tables ping-pong between read and write
// roles: the initial plan seeds level 0 into the insert table, then
// buffers flip and the recursive plan reads from scan and writes to
// insert, repeating until a level produces zero rows.
// Mirrors Java's RecordQueryRecursiveLevelUnionPlan.executePlan.
func executeRecursiveLevelUnion(
	ctx context.Context,
	p *plans.RecordQueryRecursiveLevelUnionPlan,
	store *recordlayer.FDBRecordStore,
	evalCtx *EvaluationContext,
	continuation []byte,
	props recordlayer.ExecuteProperties,
) (recordlayer.RecordCursor[QueryResult], error) {
	scanAlias := p.GetTempTableScanAlias()
	insertAlias := p.GetTempTableInsertAlias()

	scanTable := NewTempTable()
	insertTable := NewTempTable()

	levelCtx := evalCtx.WithBinding(scanAlias, scanTable)
	levelCtx = levelCtx.WithBinding(insertAlias, insertTable)

	initialCursor, err := ExecutePlan(ctx, p.GetInitialState(), store, levelCtx, continuation, props.ClearSkipAndLimit())
	if err != nil {
		return nil, fmt.Errorf("executor: recursive level union initial: %w", err)
	}

	var allResults []QueryResult
	items, err := CollectAllBounded(ctx, initialCursor, props.GetMaterializationLimit(), "recursive CTE initial state")
	initialCursor.Close()
	if err != nil {
		return nil, fmt.Errorf("executor: recursive level union initial collect: %w", err)
	}

	// UNION DISTINCT: track seen rows via a string key to detect
	// and filter duplicates (cycle detection on cyclic graphs).
	// Extract canonical column names from the seed datum so the dedup
	// key only considers CTE-relevant columns (ignoring extra join
	// columns the recursive branch may carry in its datum).
	distinct := p.IsDistinct()
	var seen map[string]struct{}
	var canonicalCols []string
	if distinct {
		seen = make(map[string]struct{}, len(items))
		if len(items) > 0 {
			if m, ok := items[0].Datum.(map[string]any); ok {
				canonicalCols = make([]string, 0, len(m))
				for k := range m {
					canonicalCols = append(canonicalCols, k)
				}
				sort.Strings(canonicalCols)
			}
		}
		var deduped []QueryResult
		for _, it := range items {
			k := queryResultKeyForCols(it, canonicalCols)
			if _, dup := seen[k]; dup {
				continue
			}
			seen[k] = struct{}{}
			deduped = append(deduped, it)
		}
		items = deduped
	}
	allResults = append(allResults, items...)

	const maxRecursionDepth = 1000
	for level := 0; ; level++ {
		if len(insertTable.GetList()) == 0 {
			break
		}
		if level >= maxRecursionDepth {
			return nil, &RecursiveCTEDepthExceededError{MaxDepth: maxRecursionDepth}
		}

		scanTable, insertTable = insertTable, scanTable
		insertTable.Clear()

		levelCtx = evalCtx.WithBinding(scanAlias, scanTable)
		levelCtx = levelCtx.WithBinding(insertAlias, insertTable)

		recursiveCursor, err := ExecutePlan(ctx, p.GetRecursiveState(), store, levelCtx, nil, props.ClearSkipAndLimit())
		if err != nil {
			return nil, fmt.Errorf("executor: recursive level union recursive: %w", err)
		}
		items, err := CollectAllBounded(ctx, recursiveCursor, props.GetMaterializationLimit(), "recursive CTE recursive level")
		recursiveCursor.Close()
		if err != nil {
			return nil, fmt.Errorf("executor: recursive level union recursive collect: %w", err)
		}
		if distinct {
			var newItems []QueryResult
			for _, it := range items {
				k := queryResultKeyForCols(it, canonicalCols)
				if _, dup := seen[k]; dup {
					continue
				}
				seen[k] = struct{}{}
				newItems = append(newItems, it)
			}
			items = newItems
			// Also replace insertTable contents with only the new
			// (non-duplicate) rows so the next level's scan sees only
			// genuinely new rows.
			insertTable.Clear()
			for _, it := range items {
				insertTable.Add(it)
			}
		}
		allResults = append(allResults, items...)
	}

	return applySkipLimit(recordlayer.FromList(allResults), props.Skip, props.ReturnedRowLimit), nil
}

// executeRecursiveDfsJoin implements depth-first recursive CTE
// execution. The root plan seeds the traversal; for each row, the
// child plan is re-evaluated with the prior row bound via
// priorCorrelation. Supports PREORDER (emit parent then children)
// and POSTORDER (emit children then parent).
// Mirrors Java's RecordQueryRecursiveDfsJoinPlan.executePlan.
func executeRecursiveDfsJoin(
	ctx context.Context,
	p *plans.RecordQueryRecursiveDfsJoinPlan,
	store *recordlayer.FDBRecordStore,
	evalCtx *EvaluationContext,
	continuation []byte,
	props recordlayer.ExecuteProperties,
) (recordlayer.RecordCursor[QueryResult], error) {
	rootCursor, err := ExecutePlan(ctx, p.GetRoot(), store, evalCtx, continuation, props.ClearSkipAndLimit())
	if err != nil {
		return nil, fmt.Errorf("executor: recursive dfs join root: %w", err)
	}

	rootRows, err := CollectAllBounded(ctx, rootCursor, props.GetMaterializationLimit(), "recursive DFS join root")
	rootCursor.Close()
	if err != nil {
		return nil, fmt.Errorf("executor: recursive dfs join root collect: %w", err)
	}

	preorder := p.GetTraversalStrategy() == plans.DfsPreorder
	var results []QueryResult
	var seen map[string]struct{}
	// For UNION DISTINCT, extract the canonical column names from the
	// root datum. The dedup key must use only these columns so that
	// root rows (with 1 column) and recursive rows (which may carry
	// extra join columns in the datum) produce matching keys.
	var canonicalCols []string
	if p.IsDistinct() {
		seen = make(map[string]struct{}, len(rootRows))
		if len(rootRows) > 0 {
			if m, ok := rootRows[0].Datum.(map[string]any); ok {
				canonicalCols = make([]string, 0, len(m))
				for k := range m {
					canonicalCols = append(canonicalCols, k)
				}
				sort.Strings(canonicalCols)
			}
		}
	}

	const maxRecursionDepth = 256

	for _, root := range rootRows {
		if seen != nil {
			k := queryResultKeyForCols(root, canonicalCols)
			if _, dup := seen[k]; dup {
				continue
			}
			seen[k] = struct{}{}
		}
		if err := dfsVisit(ctx, root, p, store, evalCtx, preorder, props, &results, 0, maxRecursionDepth, seen, canonicalCols); err != nil {
			return nil, err
		}
	}

	return applySkipLimit(recordlayer.FromList(results), props.Skip, props.ReturnedRowLimit), nil
}

func dfsVisit(
	ctx context.Context,
	node QueryResult,
	p *plans.RecordQueryRecursiveDfsJoinPlan,
	store *recordlayer.FDBRecordStore,
	evalCtx *EvaluationContext,
	preorder bool,
	props recordlayer.ExecuteProperties,
	results *[]QueryResult,
	depth, maxDepth int,
	seen map[string]struct{},
	canonicalCols []string,
) error {
	if depth >= maxDepth {
		return &RecursiveCTEDepthExceededError{MaxDepth: maxDepth}
	}

	if preorder {
		*results = append(*results, node)
	}

	singleRow := NewTempTable()
	singleRow.Add(node)
	childCtx := evalCtx.WithBinding(p.GetPriorCorrelation(), singleRow)
	childCursor, err := ExecutePlan(ctx, p.GetChild(), store, childCtx, nil, props.ClearSkipAndLimit())
	if err != nil {
		return fmt.Errorf("recursive DFS child plan: %w", err)
	}

	children, err := CollectAllBounded(ctx, childCursor, props.GetMaterializationLimit(), "recursive DFS children")
	childCursor.Close()
	if err != nil {
		return fmt.Errorf("recursive DFS collect children: %w", err)
	}

	for _, child := range children {
		if seen != nil {
			k := queryResultKeyForCols(child, canonicalCols)
			if _, dup := seen[k]; dup {
				continue
			}
			seen[k] = struct{}{}
		}
		if err := dfsVisit(ctx, child, p, store, evalCtx, preorder, props, results, depth+1, maxDepth, seen, canonicalCols); err != nil {
			return err
		}
	}

	if !preorder {
		*results = append(*results, node)
	}
	return nil
}

// applySkipLimit wraps a cursor with skip/limit only when the values
// are meaningful. ReturnedRowLimit <= 0 means unlimited (matching
// DefaultExecuteProperties convention).
func applySkipLimit(cursor recordlayer.RecordCursor[QueryResult], skip, limit int) recordlayer.RecordCursor[QueryResult] {
	if skip > 0 {
		cursor = recordlayer.SkipCursor(cursor, skip)
	}
	if limit > 0 {
		cursor = recordlayer.LimitRowsCursor(cursor, limit)
	}
	return cursor
}

// filterResultCursor filters QueryResult items.
type filterResultCursor struct {
	inner  recordlayer.RecordCursor[QueryResult]
	pred   func(QueryResult) bool
	closed bool
}

func (c *filterResultCursor) OnNext(ctx context.Context) (result recordlayer.RecordCursorResult[QueryResult], err error) {
	defer func() {
		if r := recover(); r != nil {
			switch e := r.(type) {
			case *predicates.TypeMismatchError:
				err = e
			case *values.ArithmeticOverflowError:
				err = e
			case *values.ArithmeticDivisionByZeroError:
				err = e
			case *values.ScalarTypeMismatchError:
				err = e
			case *values.InvalidCastError:
				err = e
			default:
				panic(r)
			}
		}
	}()
	for {
		if err = ctx.Err(); err != nil {
			return recordlayer.RecordCursorResult[QueryResult]{}, err
		}
		result, err = c.inner.OnNext(ctx)
		if err != nil {
			return result, err
		}
		if !result.HasNext() {
			return result, nil
		}
		if c.pred(result.GetValue()) {
			return result, nil
		}
	}
}

func (c *filterResultCursor) Close() error {
	c.closed = true
	return c.inner.Close()
}

func (c *filterResultCursor) IsClosed() bool { return c.closed }

// sortResultCursor collects all inner results, sorts them, then
// yields in sorted order. Used by RecordQuerySortPlan.
type sortResultCursor struct {
	items []QueryResult
	pos   int
}

func newSortResultCursor(items []QueryResult) *sortResultCursor {
	return &sortResultCursor{items: items}
}

func (c *sortResultCursor) OnNext(_ context.Context) (recordlayer.RecordCursorResult[QueryResult], error) {
	if c.pos >= len(c.items) {
		return recordlayer.NewResultNoNext[QueryResult](recordlayer.SourceExhausted, &recordlayer.EndContinuation{}), nil
	}
	v := c.items[c.pos]
	c.pos++
	return recordlayer.NewResultWithValue(v, &recordlayer.StartContinuation{}), nil
}

func (c *sortResultCursor) Close() error   { return nil }
func (c *sortResultCursor) IsClosed() bool { return false }

// MaterializationLimitExceededError is returned when an operator tries to
// buffer more rows in memory than the configured materialization limit.
type MaterializationLimitExceededError struct {
	Limit   int
	Context string
}

func (e *MaterializationLimitExceededError) Error() string {
	return fmt.Sprintf("materialization limit exceeded (%d rows): %s; consider adding an index or increasing the materialization limit", e.Limit, e.Context)
}

// CollectAll drains a cursor into a slice.
func CollectAll(ctx context.Context, cursor recordlayer.RecordCursor[QueryResult]) ([]QueryResult, error) {
	var results []QueryResult
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		result, err := cursor.OnNext(ctx)
		if err != nil {
			return nil, err
		}
		if !result.HasNext() {
			break
		}
		results = append(results, result.GetValue())
	}
	return results, nil
}

// CollectAllBounded drains a cursor into a slice, returning a
// MaterializationLimitExceededError if the number of rows exceeds limit.
func CollectAllBounded(ctx context.Context, cursor recordlayer.RecordCursor[QueryResult], limit int, opName string) ([]QueryResult, error) {
	var results []QueryResult
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		result, err := cursor.OnNext(ctx)
		if err != nil {
			return nil, err
		}
		if !result.HasNext() {
			break
		}
		results = append(results, result.GetValue())
		if len(results) >= limit {
			return nil, &MaterializationLimitExceededError{Limit: limit, Context: opName}
		}
	}
	return results, nil
}

// sortByKeys sorts QueryResult slice by the given sort key names.
// Each key references a field in the datum map; direction is
// ascending by default.
func sortByKeys(items []QueryResult, keys []string, directions []bool) {
	// PK tiebreaker direction matches the last explicit sort key.
	pkDesc := false
	if len(directions) > 0 {
		pkDesc = directions[len(directions)-1]
	}
	sort.SliceStable(items, func(i, j int) bool {
		for k, key := range keys {
			vi := fieldFromDatum(items[i].Datum, key)
			vj := fieldFromDatum(items[j].Datum, key)
			cmp := compareAny(vi, vj)
			if cmp == 0 {
				continue
			}
			desc := k < len(directions) && directions[k]
			if desc {
				return cmp > 0
			}
			return cmp < 0
		}
		// All explicit sort keys equal — break ties by PK.
		if items[i].PrimaryKey != nil && items[j].PrimaryKey != nil {
			cmp := comparePKTuples(items[i].PrimaryKey, items[j].PrimaryKey)
			if cmp != 0 {
				if pkDesc {
					return cmp > 0
				}
				return cmp < 0
			}
		}
		return false
	})
}

// partialSortTopK is a Go-only extension optimization that rearranges
// items so that the first k elements are the top-k in sorted order,
// using a max-heap of size k. O(N log k) time, O(k) auxiliary space.
// After this call, items[:k] contains the top-k in sorted order.
func partialSortTopK(items []QueryResult, keys []string, directions []bool, k int) {
	if k <= 0 || k >= len(items) {
		sortByKeys(items, keys, directions)
		return
	}

	less := func(a, b QueryResult) bool {
		for i, key := range keys {
			va := fieldFromDatum(a.Datum, key)
			vb := fieldFromDatum(b.Datum, key)
			cmp := compareAny(va, vb)
			if cmp == 0 {
				continue
			}
			desc := i < len(directions) && directions[i]
			if desc {
				return cmp > 0
			}
			return cmp < 0
		}
		return false
	}

	// Build a max-heap of size k (we want the SMALLEST k elements, so
	// the heap root is the LARGEST among the current top-k — if a new
	// element is smaller than the root, it replaces it).
	h := &topKHeap{items: make([]QueryResult, k), less: less}
	copy(h.items, items[:k])
	heap.Init(h)

	for i := k; i < len(items); i++ {
		if less(items[i], h.items[0]) {
			h.items[0] = items[i]
			heap.Fix(h, 0)
		}
	}

	// Extract from heap in reverse order → sorted ascending.
	result := make([]QueryResult, k)
	for i := k - 1; i >= 0; i-- {
		result[i] = heap.Pop(h).(QueryResult)
	}
	copy(items[:k], result)
}

// topKHeap is a max-heap for the top-K partial sort. The "less"
// function defines the desired sort order; the heap inverts it (max-
// heap) so the root is the WORST element among the current top-K.
type topKHeap struct {
	items []QueryResult
	less  func(a, b QueryResult) bool
}

func (h *topKHeap) Len() int           { return len(h.items) }
func (h *topKHeap) Less(i, j int) bool { return h.less(h.items[j], h.items[i]) } // inverted for max-heap
func (h *topKHeap) Swap(i, j int)      { h.items[i], h.items[j] = h.items[j], h.items[i] }
func (h *topKHeap) Push(x any)         { h.items = append(h.items, x.(QueryResult)) }
func (h *topKHeap) Pop() any {
	old := h.items
	n := len(old)
	item := old[n-1]
	h.items = old[:n-1]
	return item
}

// comparePKTuples compares two primary key tuples using their packed
// byte representation, which preserves FDB tuple ordering. Returns
// -1, 0, or 1.
func comparePKTuples(a, b tuple.Tuple) int {
	ap := a.Pack()
	bp := b.Pack()
	for i := 0; i < len(ap) && i < len(bp); i++ {
		if ap[i] < bp[i] {
			return -1
		}
		if ap[i] > bp[i] {
			return 1
		}
	}
	if len(ap) < len(bp) {
		return -1
	}
	if len(ap) > len(bp) {
		return 1
	}
	return 0
}

func projectionColumnName(v values.Value) string {
	if fv, ok := v.(*values.FieldValue); ok {
		return fv.Field
	}
	return strings.ToUpper(values.ExplainValue(v))
}

func fieldFromDatum(datum any, key string) any {
	if m, ok := datum.(map[string]any); ok {
		return m[strings.ToUpper(key)]
	}
	return nil
}

func compareAny(a, b any) int {
	if a == nil && b == nil {
		return 0
	}
	if a == nil {
		return -1
	}
	if b == nil {
		return 1
	}
	if f, ok := a.(float32); ok {
		a = float64(f)
	}
	if f, ok := b.(float32); ok {
		b = float64(f)
	}
	switch av := a.(type) {
	case int64:
		switch bv := b.(type) {
		case int64:
			if av < bv {
				return -1
			}
			if av > bv {
				return 1
			}
			return 0
		case float64:
			fa := float64(av)
			if fa < bv {
				return -1
			}
			if fa > bv {
				return 1
			}
			return 0
		default:
			return 0
		}
	case float64:
		switch bv := b.(type) {
		case float64:
			if av < bv {
				return -1
			}
			if av > bv {
				return 1
			}
			return 0
		case int64:
			fb := float64(bv)
			if av < fb {
				return -1
			}
			if av > fb {
				return 1
			}
			return 0
		default:
			return 0
		}
	case string:
		bv, ok := b.(string)
		if !ok {
			return 0
		}
		if av < bv {
			return -1
		}
		if av > bv {
			return 1
		}
		return 0
	case bool:
		bv, ok := b.(bool)
		if !ok {
			return 0
		}
		if av == bv {
			return 0
		}
		if !av {
			return -1
		}
		return 1
	default:
		return 0
	}
}

// --- Go extensions (no Java equivalent) ---

// executeInMemorySort materializes the inner plan's output and sorts it.
// Go extension — Java's Cascades has no physical sort operator.
func executeInMemorySort(
	ctx context.Context,
	p *plans.RecordQueryInMemorySortPlan,
	store *recordlayer.FDBRecordStore,
	evalCtx *EvaluationContext,
	continuation []byte,
	props recordlayer.ExecuteProperties,
) (recordlayer.RecordCursor[QueryResult], error) {
	var innerContinuation []byte
	var priorBuf []QueryResult
	if continuation != nil {
		ic, buf, decErr := decodeSortContinuation(continuation)
		if decErr != nil {
			return nil, fmt.Errorf("invalid sort continuation: %w", decErr)
		}
		innerContinuation = ic
		priorBuf = buf
	}

	innerCursor, err := ExecutePlan(ctx, p.GetInner(), store, evalCtx, innerContinuation, props.ClearSkipAndLimit())
	if err != nil {
		return nil, err
	}

	keys := p.GetSortKeys()
	sortFn := func(results []QueryResult) {
		pkDesc := false
		if len(keys) > 0 {
			pkDesc = keys[len(keys)-1].Desc
		}
		sort.SliceStable(results, func(i, j int) bool {
			for _, k := range keys {
				var ci, cj any
				if k.ValueExpr != nil {
					ci = k.ValueExpr.Evaluate(results[i].Datum)
					cj = k.ValueExpr.Evaluate(results[j].Datum)
				} else {
					ci = compareByField(results[i], k.Field)
					cj = compareByField(results[j], k.Field)
				}
				iNil := ci == nil
				jNil := cj == nil
				if iNil && jNil {
					continue
				}
				if iNil || jNil {
					if k.NullsFirst {
						return iNil
					}
					return jNil
				}
				cmp := compareValues(ci, cj)
				if cmp == 0 {
					continue
				}
				if k.Desc {
					return cmp > 0
				}
				return cmp < 0
			}
			if results[i].PrimaryKey != nil && results[j].PrimaryKey != nil {
				cmp := comparePKTuples(results[i].PrimaryKey, results[j].PrimaryKey)
				if cmp != 0 {
					if pkDesc {
						return cmp > 0
					}
					return cmp < 0
				}
			}
			return false
		})
	}

	cursor := newCustomSortCursor(innerCursor, sortFn)
	if len(priorBuf) > 0 {
		cursor.buf = priorBuf
	}
	return applySkipLimit(cursor, props.Skip, props.ReturnedRowLimit), nil
}

func compareByField(qr QueryResult, field string) any {
	m, ok := qr.Datum.(map[string]any)
	if !ok {
		return nil
	}
	if v, found := m[field]; found {
		return v
	}
	if v, found := m[strings.ToUpper(field)]; found {
		return v
	}
	for k, v := range m {
		if strings.EqualFold(k, field) {
			return v
		}
	}
	return nil
}

// queryResultKeyForCols produces a dedup key using only the specified
// canonical columns. This ensures root rows (which have only seed
// columns) and recursive rows (which may carry extra join columns)
// produce matching keys when their CTE-relevant values are equal.
func queryResultKeyForCols(qr QueryResult, cols []string) string {
	if len(cols) == 0 {
		return queryResultKey(qr)
	}
	m, ok := qr.Datum.(map[string]any)
	if !ok {
		return fmt.Sprintf("%v", qr.Datum)
	}
	var sb strings.Builder
	for i, col := range cols {
		if i > 0 {
			sb.WriteByte('|')
		}
		v := m[col]
		if v == nil {
			sb.WriteString("\x00NULL\x00")
		} else {
			sb.WriteString(fmt.Sprintf("%v", v))
		}
	}
	return sb.String()
}

// queryResultKey produces a stable string key from a QueryResult's datum
// for UNION DISTINCT deduplication in recursive CTEs. The key is built
// from VALUES ONLY (sorted by column name for determinism) so rows with
// different column names but identical values (e.g. seed {SRC:1} and
// recursive {DST:1}) are correctly identified as duplicates.
func queryResultKey(qr QueryResult) string {
	m, ok := qr.Datum.(map[string]any)
	if !ok {
		return fmt.Sprintf("%v", qr.Datum)
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var sb strings.Builder
	for i, k := range keys {
		if i > 0 {
			sb.WriteByte('|')
		}
		v := m[k]
		if v == nil {
			sb.WriteString("\x00NULL\x00")
		} else {
			sb.WriteString(fmt.Sprintf("%v", v))
		}
	}
	return sb.String()
}
