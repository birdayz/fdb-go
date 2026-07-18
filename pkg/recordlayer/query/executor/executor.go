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
	"context"
	"encoding/binary"
	"fmt"
	"math"
	"slices"
	"sort"
	"strings"

	"github.com/google/uuid"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"

	"fdb.dev/gen"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
	"fdb.dev/pkg/relational/api"
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

// rejectUnsupportedResume converts a non-empty incoming continuation on a
// cursor shape with NO continuation handling into a typed decline. These
// cursors previously DROPPED the bytes and restarted from row 0 — inside a
// resumed branch-tagged concat that is an infinite repeat (a VALUES branch
// re-emits its rows on every page) or silent duplicates, never an error
// Correct-or-loud until each shape gains real resume.
func rejectUnsupportedResume(continuation []byte, shape string) error {
	if len(continuation) == 0 {
		return nil
	}
	return &UnsupportedContinuationError{Shape: shape}
}

// UnsupportedContinuationError reports a resume attempt on a cursor shape
// that has no continuation support yet (RFC-180 WS-A follow-ups). The driver
// maps it to SQLSTATE 0A000 — a typed decline, never a silent wrong start
// (before RFC-180 the buffered union fed the PARENT's continuation to every
// child; a raw-key scan child consumed it as a scan position).
type UnsupportedContinuationError struct {
	Shape string
}

func (e *UnsupportedContinuationError) Error() string {
	return "unsupported continuation: " + e.Shape + " cannot resume from a continuation"
}

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
	case *plans.RecordQueryUnionPlan:
		return executeUnion(ctx, p, store, evalCtx, continuation, props)
	case *plans.RecordQueryIntersectionPlan:
		return executeIntersection(ctx, p, store, evalCtx, continuation, props)
	case *plans.RecordQueryNestedLoopJoinPlan:
		return executeNestedLoopJoin(ctx, p, store, evalCtx, continuation, props)
	case *plans.RecordQueryStreamingAggregationPlan:
		return executeAggregation(ctx, p.GetInner(), p.GetGroupingKeys(), p.GetAggregates(), store, evalCtx, continuation, props)
	case *plans.RecordQueryExplodePlan:
		if err := rejectUnsupportedResume(continuation, "explode"); err != nil {
			return nil, err
		}
		return executeExplode(p, evalCtx, props)
	case *plans.RecordQueryDeletePlan:
		return executeDelete(ctx, p, store, evalCtx, continuation, props)
	case *plans.RecordQueryInsertPlan:
		return executeInsert(ctx, p, store, evalCtx, continuation, props)
	case *plans.RecordQueryUpdatePlan:
		return executeUpdate(ctx, p, store, evalCtx, continuation, props)
	case *plans.RecordQueryTempTableScanPlan:
		if err := rejectUnsupportedResume(continuation, "temp-table scan"); err != nil {
			return nil, err
		}
		return executeTempTableScan(p, evalCtx, props)
	case *plans.RecordQueryTempTableInsertPlan:
		return executeTempTableInsert(ctx, p, store, evalCtx, continuation, props)
	case *plans.RecordQueryTableFunctionPlan:
		if err := rejectUnsupportedResume(continuation, "table function"); err != nil {
			return nil, err
		}
		return executeTableFunction(p, evalCtx, props)
	case *plans.RecordQueryValuesPlan:
		if err := rejectUnsupportedResume(continuation, "VALUES"); err != nil {
			return nil, err
		}
		return executeValues(p, evalCtx)
	case *plans.RecordQueryRecursiveLevelUnionPlan:
		// STOPGAP (RFC-181 C1): recursion is materialized eagerly and its
		// mid-stream tokens are bare list indices; the executor used to feed
		// an incoming continuation RAW to the SEED plan, where a scan seed
		// silently accepted the bytes as a key suffix — wrong seed set and
		// re-emission, no error. Decline loudly until the Java-shape
		// RecursiveUnionCursor continuation (phase + PTempTable frontier +
		// child position) is ported.
		if err := rejectUnsupportedResume(continuation, "recursive CTE (level union)"); err != nil {
			return nil, err
		}
		return executeRecursiveLevelUnion(ctx, p, store, evalCtx, nil, props)
	case *plans.RecordQueryRecursiveDfsJoinPlan:
		// Same stopgap as the level union above (RFC-181 C1).
		if err := rejectUnsupportedResume(continuation, "recursive CTE (DFS join)"); err != nil {
			return nil, err
		}
		return executeRecursiveDfsJoin(ctx, p, store, evalCtx, nil, props)
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
		return executeLoadByKeys(ctx, p, store, evalCtx, continuation, props)

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
		tupleRange, err := scanComparisonsToTupleRange(comps, scanBindContext(evalCtx))
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

	scanRange, err := scanComparisonsToTupleRange(p.GetScanComparisons(), scanBindContext(evalCtx))
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
		var logicalType *values.RecordType
		if rts := p.GetRecordTypes(); len(rts) > 0 {
			if rt := store.GetMetaData().GetRecordType(rts[0]); rt != nil {
				if rt.PrimaryKey != nil {
					pkCols = rt.PrimaryKey.FieldNames()
				}
				// The record's LOGICAL row shape — only authoritative when the
				// scan serves a single record type (a multi-type covering scan
				// has no single logical shape and keeps the index layout).
				if len(rts) == 1 && rt.Descriptor != nil {
					logicalType = PositionalTypeForDescriptor(rt.Descriptor)
				}
			}
		}
		cov := p.GetCoveringColumns()
		posNames := make([]string, 0, len(cov)+len(pkCols))
		for _, col := range cov {
			posNames = append(posNames, strings.ToUpper(col))
		}
		for _, col := range pkCols {
			posNames = append(posNames, strings.ToUpper(col))
		}
		// A covering-index row must conform to the record's LOGICAL slot
		// order — Java's IndexKeyValueToPartialRecord builds a
		// descriptor-shaped partial record, so a FieldValue ordinal baked
		// against the record type reads the same slot on the base-scan and
		// covering paths. Non-covered fields stay nil (Java: unset partial
		// fields — the planner's covering gate guarantees they are never
		// referenced). When any column cannot be mapped (a
		// nested/expression index column has no top-level logical slot),
		// this falls THROUGH to the full-record fetch below — never an
		// index-layout row, which a baked logical ordinal would misread.
		logicalOrds := coveringLogicalOrdinals(posNames, logicalType)
		if logicalOrds != nil {
			return &coveringIndexCursor{
				inner:       indexCursor,
				columns:     cov,
				pkColumns:   pkCols,
				logicalType: logicalType,
				logicalOrds: logicalOrds,
			}, nil
		}
		// This covering scan cannot present a row in the record's LOGICAL slot
		// order: a nested/expression index column (e.g. `ADDR.CITY`) has no
		// top-level logical slot, and a multi-type scan has no single logical
		// shape. Flat column references are BAKED to their logical ordinal, so an
		// INDEX-layout covering row would read a baked ordinal at the wrong slot
		// whenever index-order != logical-order (descriptor [ID,A,ADDR] + index
		// row [A,ADDR.CITY,ID]: `A#1` would read ADDR.CITY — a wrong slot). Rather
		// than serve a misread-able covering row (or fail the query loud), fall
		// back to fetching the full record by PK — the base record layout, which a
		// baked ordinal reads correctly. Slower than covering but correct; the
		// covering optimization simply doesn't apply to these shapes. (Java serves
		// them from the covering index via IndexKeyValueToPartialRecord's
		// descriptor-shaped partial record — porting that would restore covering
		// here, but the fetch fallback is correct in the meantime.)
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
		op, err := cr.GetEqualityComparison().Operand.Evaluate(evalCtx)
		if err != nil {
			return nil, err
		}
		prefix = append(prefix, op)
	}

	queryVec, err := evalFloat64Slice(p.GetQueryVector(), evalCtx)
	if err != nil {
		return nil, fmt.Errorf("executor: vector index %q query vector: %w", p.GetIndexName(), err)
	}
	// The scan's rank cap. A top-k whose ADJUSTED cap is ≤ 0 — ROW_NUMBER() <= 0,
	// < 1, or a parameter `<= ?` / `< ?` bound to 0 or a negative — selects NO
	// rows, so return EMPTY rather than erroring. An eager positive-only eval
	// rejected k ≤ 0, which made `<= 0` / `<= ?`(=0) error out BEFORE the
	// Limit(0)/Limit(?) above could cull to empty (RFC-156 correctness-hunt bug:
	// only `< 1`, where the comparand K=1 survives the positive check, was
	// handled). Evaluate tolerantly and
	// short-circuit the non-positive adjusted cap here, once, for BOTH the
	// ordered-stream and self-limiting branches below.
	k, err := evalRankCap(p.GetK(), evalCtx)
	if err != nil {
		return nil, fmt.Errorf("executor: vector index %q top-K: %w", p.GetIndexName(), err)
	}
	rankCap := k
	if p.GetRankType() == predicates.ComparisonDistanceRankLessThan {
		// `< K` selects the top K-1. K ≤ 1 ⇒ no rows; test BEFORE subtracting so a
		// K = math.MinInt64 (literal `< -9223372036854775808`, or a bound param)
		// cannot wrap k-1 to a huge POSITIVE and slip past the ≤0 guard into an
		// enormous horizon. K ≥ 2 here ⇒ k-1 cannot overflow.
		if k <= 1 {
			return recordlayer.Empty[QueryResult](), nil
		}
		rankCap = k - 1
	}
	if rankCap <= 0 { // `<= K` with K ≤ 0
		return recordlayer.Empty[QueryResult](), nil
	}

	// The default is the INDEX METHOD's own (HNSW efSearch=200; SPFresh's
	// tuned kc=64 — passing 200 here silently overrode it for every SQL
	// query). 0 = "use the maintainer's default"; only
	// an explicit per-query efSearch overrides it.
	efSearch := 0
	if idx.Type == recordlayer.IndexTypeVector {
		efSearch = defaultVectorEfSearch
	}
	if p.GetEfSearch() != nil {
		efSearch = *p.GetEfSearch()
	}

	var scanRange recordlayer.TupleRange
	scanType := recordlayer.IndexScanByDistance
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
		scanRange = recordlayer.VectorDistanceScanRangeOrdered(queryVec, horizon, efSearch, horizon, prefix)
	} else {
		// Self-limiting (top-k) mode. The scan limit IS the adjusted rank cap
		// (Java's VectorIndexScanBounds.getAdjustedLimit: K for <=K, K-1 for <K) —
		// already computed as rankCap above and proven ≥ 1 by the ≤0 short-circuit
		// (a non-positive adjusted cap returned EMPTY there). No re-derive, and no
		// dead ≤0 check.
		limit := rankCap
		if efSearch != 0 && efSearch < limit {
			efSearch = limit
		}
		scanRange = recordlayer.VectorDistanceScanRangeWithPrefix(queryVec, limit, efSearch, prefix)
	}
	scanProps := recordlayer.ScanProperties{
		ExecuteProperties:   props,
		CursorStreamingMode: recordlayer.StreamingModeIterator,
	}
	indexCursor := store.ScanIndexByType(idx, scanType, scanRange, continuation, scanProps)
	return &indexFetchCursor{inner: indexCursor, store: store}, nil
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

// scanBindContext returns the binder for evaluating scan-range comparands. It
// must be a *RowEvalContext (via RowContext), NOT the bare *EvaluationContext:
// an uncorrelated scalar subquery pushed as a scan bound is a ScalarSubqueryValue,
// and ScalarSubqueryValue.Evaluate only reads its pre-computed result from a
// *RowEvalContext's ScalarSubqueries map. Passing the bare *EvaluationContext made
// it resolve to nil → an `id = NULL` bound → an empty scan (e.g.
// `WHERE id = (SELECT MIN(id) FROM t)` returned 0 rows). RowContext still binds
// parameters (BindParameter) and correlations, so this is a strict superset.
// nil-safe (a nil evalCtx keeps the prior nil binder for the param-free unit path).
func scanBindContext(evalCtx *EvaluationContext) values.ParameterBinder {
	if evalCtx == nil {
		return nil
	}
	return evalCtx.RowContext()
}

// uuidToTupleElement converts a neutral 16-byte UUID comparand ([16]byte —
// the wire-agnostic representation a UUID carries through the value layer, see
// values.PromoteValue.Evaluate and predicates.cmpAny) into a tuple.UUID at the
// FDB wire boundary. Only a named tuple.UUID packs as the 0x30 UUID tuple
// element; a bare [16]byte would panic the tuple packer ("unencodable
// element"). Everything else (int64, string, float64, …) passes through
// unchanged. This is the sole place the value-layer [16]byte crosses into wire
// encoding on the scan-range path — symmetric with the index-entry write in
// recordlayer.scalarToInterface, so the equality probe seeks the exact 0x30
// bytes Java (and the maintainer) wrote.
func uuidToTupleElement(v any) any {
	if b, ok := v.([16]byte); ok {
		return tuple.UUID(b)
	}
	return v
}

// tupleElementToRowValue normalizes a decoded tuple element read off an index
// entry / primary key into the value layer's row domain:
//   - tuple.UUID → the neutral [16]byte the value layer works with (cmpAny,
//     PromoteValue, materialization); the inverse of uuidToTupleElement.
//   - float32 → float64: a FLOAT column is stored in index keys as a 32-bit
//     tuple float (code 0x20) and decodes as float32, but the base-record path
//     widens FLOAT to float64 (values.ProtoScalarKindToRowValue). Java is
//     type-consistent across access paths by construction — the covering
//     partial record sets the tuple element back on the proto builder
//     (IndexKeyValueToPartialRecord.FieldCopier) and every read goes through
//     the message, surfacing the same boxed Float either way — so the Go
//     covering row must live in the SAME domain as the base row, or
//     comparators (compareValues), dedup keys (distinctKey, packedDedupKey)
//     and hash-join keys split float32-vs-float64 across access paths.
//
// Applied at every index-entry → row boundary so a column flows downstream
// identically regardless of whether it was sourced from a stored record
// (protoFieldToGo) or an index entry — the two must be interchangeable for
// residual filters, sorts, DISTINCT and join keys. Other elements pass through.
func tupleElementToRowValue(v any) any {
	switch tv := v.(type) {
	case tuple.UUID:
		return [16]byte(tv)
	case float32:
		return float64(tv)
	}
	return v
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
		// IS NULL is an equality range on the NULL value (Java's
		// getComparisonType(IS_NULL)==EQUALITY): it has no RHS Operand, and the
		// sought key element is NULL itself. Append nil to seek the single
		// [null] index entry, rather than Evaluate'ing a nil Operand.
		if comp.Type == predicates.ComparisonIsNull {
			prefix = append(prefix, nil)
			continue
		}
		val, err := comp.Operand.Evaluate(binder)
		if err != nil {
			return recordlayer.TupleRange{}, err
		}
		val = uuidToTupleElement(val)
		// `col = <NULL>` (a regular equality whose comparand evaluates to NULL —
		// NOT `IS NULL`, handled above): SQL `NULL = x` is UNKNOWN for every row,
		// so the probe matches NOTHING. Appending nil here would instead seek the
		// [.., null] index entries and WRONGLY match NULL-keyed rows — e.g. a
		// correlated index-nested-loop probe `A.K = B.K` where the outer B.K is
		// NULL would match A's NULL-keyed rows (NULL=NULL must not match). Return
		// an explicit empty range (begin == end), mirroring the inequality
		// NULL-comparand handling below. IS NULL and IS NOT DISTINCT FROM
		// (null-safe equality) intentionally still seek the null entry.
		//
		// (Java's ScanComparisons.toTupleRange does NOT special-case this — it
		// packs null as a tuple element; Java avoids the wrong rows because its
		// planner never feeds a null equality comparand into a bare index probe.
		// Go's correlated index-nested-loop does, so the SQL invariant must be
		// enforced here.)
		if val == nil && comp.Type == predicates.ComparisonEquals {
			return recordlayer.TupleRange{
				Low:          prefix,
				High:         prefix,
				LowEndpoint:  recordlayer.EndpointTypeRangeInclusive,
				HighEndpoint: recordlayer.EndpointTypeRangeExclusive,
			}, nil
		}
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

	// STARTS_WITH is a single-comparison PREFIX_STRING range, handled BEFORE the
	// general low/high combiner — mirrors Java ScanComparisons.toTupleRange
	// (ScanComparisons.java ~302-308): when the sole inequality is STARTS_WITH,
	// build startTuple = equality prefix + comparand and return
	// new TupleRange(startTuple, startTuple, PREFIX_STRING, PREFIX_STRING). The
	// PREFIX_STRING endpoints strip the tuple-packed trailing null on the low and
	// strinc past it on the high, bounding the scan to keys with that string
	// prefix. Without this arm STARTS_WITH matches neither the low nor the high
	// switch below, sets no bound, and the scan degenerates to the full
	// equality-prefix range — silently returning every row under the prefix.
	if ineqs := nextRange.GetInequalityComparisons(); len(ineqs) == 1 && ineqs[0].Type == predicates.ComparisonStartsWith {
		startsWith := ineqs[0]
		if startsWith.Operand == nil {
			// No prefix operand to bound against: fall back to the equality prefix
			// rather than fabricate a bound (defensive — the planner always binds a
			// prefix operand for STARTS_WITH).
			return recordlayer.TupleRangeAllOf(prefix), nil
		}
		val, err := startsWith.Operand.Evaluate(binder)
		if err != nil {
			return recordlayer.TupleRange{}, err
		}
		// A NULL prefix operand makes `col STARTS_WITH NULL` UNKNOWN for every row
		// (SQL 3VL) → unsatisfiable → empty result, consistent with the ordered-
		// inequality NULL-comparand handling below. Return an explicit empty range
		// (begin == end) rather than PREFIX_STRING over a null element.
		if val == nil {
			return recordlayer.TupleRange{
				Low:          prefix,
				High:         prefix,
				LowEndpoint:  recordlayer.EndpointTypeRangeInclusive,
				HighEndpoint: recordlayer.EndpointTypeRangeExclusive,
			}, nil
		}
		val = uuidToTupleElement(val)
		startTuple := append(append(tuple.Tuple{}, prefix...), val)
		return recordlayer.TupleRange{
			Low:          startTuple,
			High:         startTuple,
			LowEndpoint:  recordlayer.EndpointTypePrefixString,
			HighEndpoint: recordlayer.EndpointTypePrefixString,
		}, nil
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
			var err error
			comparand, err = ineq.Operand.Evaluate(binder)
			if err != nil {
				return recordlayer.TupleRange{}, err
			}
			comparand = uuidToTupleElement(comparand)
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
		default:
			// Mirror Java ScanComparisons.InequalityRangeCombiner.addComparison
			// (ScanComparisons.java:648) `default: throw`. The only INEQUALITY-typed
			// comparison the endpoint combiner cannot turn into a low/high bound is
			// STARTS_WITH: it is a PREFIX_STRING range handled ABOVE as the sole
			// inequality (len(ineqs)==1). If STARTS_WITH arrives here it was merged
			// with a second inequality on the same column — that intersection is not
			// a representable single scan range, so fail LOUD rather than drop the
			// bound and silently return a superset. (NOT_EQUALS is ComparisonType.NONE
			// in Java — residual, never sargable into a scan range — so it does not
			// reach this combiner; any other type arriving here is likewise an
			// unexpected-invariant bug to surface, not to paper over.)
			return recordlayer.TupleRange{}, fmt.Errorf(
				"scanComparisonsToTupleRange: unexpected inequality comparison %v combined with another inequality on the same column (not a representable single scan range)",
				ineq.Type)
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
	// lastNoNext replays the terminal result on a contract-violating re-call
	// (Java's cached no-next result) — never re-pulls the inner entry scan.
	lastNoNext *recordlayer.RecordCursorResult[QueryResult]
}

func (c *indexFetchCursor) OnNext(ctx context.Context) (recordlayer.RecordCursorResult[QueryResult], error) {
	if c.lastNoNext != nil {
		return *c.lastNoNext, nil
	}
	// One index entry per call: the fetch either yields its record, exhausts, or
	// raises on an orphan/malformed entry. No loop — orphans no longer
	// skip-and-continue (that silently dropped rows); they abort with a typed
	// error like Java's IndexOrphanBehavior.ERROR default.
	if err := ctx.Err(); err != nil {
		return recordlayer.RecordCursorResult[QueryResult]{}, err
	}
	result, err := c.inner.OnNext(ctx)
	if err != nil {
		return recordlayer.NewResultNoNext[QueryResult](recordlayer.SourceExhausted, &recordlayer.EndContinuation{}), err
	}
	if !result.HasNext() {
		res := recordlayer.NewResultNoNext[QueryResult](result.GetNoNextReason(), result.GetContinuation())
		c.lastNoNext = &res
		return res, nil
	}

	entry := result.GetValue()
	indexName := ""
	if entry.Index != nil {
		indexName = entry.Index.Name
	}
	pk := entry.PrimaryKey()
	if pk == nil {
		// A malformed index entry that resolves to no primary key is detectable
		// corruption, not a row to drop. Java resolves the PK before fetching
		// (IndexEntry.getPrimaryKey) and the ERROR orphan path below is loud for
		// any missing base record; a nil PK is the same class of fault, so raise
		// rather than silently continue.
		return recordlayer.NewResultNoNext[QueryResult](recordlayer.SourceExhausted, &recordlayer.EndContinuation{}),
			&recordlayer.RecordCoreStorageError{
				Message:   "record not found from index entry",
				IndexName: indexName,
				IndexKey:  entry.Key,
			}
	}

	rec, err := c.store.LoadRecord(pk)
	if err != nil {
		return recordlayer.NewResultNoNext[QueryResult](recordlayer.SourceExhausted, &recordlayer.EndContinuation{}), fmt.Errorf("executor: loading record for index entry pk %v: %w", pk, err)
	}
	if rec == nil {
		// Port Java's IndexOrphanBehavior.ERROR (RecordQueryIndexPlan →
		// FetchIndexRecords.PRIMARY_KEY → FDBRecordStoreBase.loadIndexEntryRecord):
		// an index entry whose base record is missing means the index and the
		// records disagree — index corruption, an out-of-band delete, or a
		// maintainer bug. Query execution never uses SKIP here; silently
		// continuing would return fewer rows and hide the corruption. Raise the
		// same typed error Java throws, carrying its INDEX_NAME / PRIMARY_KEY /
		// INDEX_KEY log info.
		return recordlayer.NewResultNoNext[QueryResult](recordlayer.SourceExhausted, &recordlayer.EndContinuation{}),
			&recordlayer.RecordCoreStorageError{
				Message:    "record not found from index entry",
				IndexName:  indexName,
				PrimaryKey: pk,
				IndexKey:   entry.Key,
			}
	}

	qr := FromStoredRecord(rec)
	return recordlayer.NewResultWithValue(qr, result.GetContinuation()), nil
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
	// logicalType/logicalOrds shape the cursor's LOGICAL-ordinal rows:
	// logicalType is the record's descriptor-shaped RecordType and
	// logicalOrds[i] the logical slot of the i-th [columns..., pkColumns...]
	// value. A covering scan whose columns cannot ALL map to a logical slot
	// (nested/expression index, or a multi-type scan) is refused LOUD at
	// construction, so logicalOrds is always non-nil here.
	logicalType *values.RecordType
	logicalOrds []int
	closed      bool
	// lastNoNext replays the terminal result on a contract-violating re-call
	// (Java's cached no-next result) — never re-pulls the inner entry scan.
	lastNoNext *recordlayer.RecordCursorResult[QueryResult]
}

func (c *coveringIndexCursor) OnNext(ctx context.Context) (recordlayer.RecordCursorResult[QueryResult], error) {
	if c.lastNoNext != nil {
		return *c.lastNoNext, nil
	}
	result, err := c.inner.OnNext(ctx)
	if err != nil {
		return recordlayer.NewResultNoNext[QueryResult](recordlayer.SourceExhausted, &recordlayer.EndContinuation{}), err
	}
	if !result.HasNext() {
		res := recordlayer.NewResultNoNext[QueryResult](result.GetNoNextReason(), result.GetContinuation())
		c.lastNoNext = &res
		return res, nil
	}

	entry := result.GetValue()
	// logicalOrds is guaranteed non-nil (executeIndexScan refuses a non-conforming
	// covering scan LOUD at construction), so the row is always LOGICAL-shaped.
	pos := buildCoveringLogicalRow(c.columns, c.pkColumns, entry.IndexValues(), entry.PrimaryKey(), c.logicalType, c.logicalOrds)
	return recordlayer.NewResultWithValue(QueryResult{Positional: pos}, result.GetContinuation()), nil
}

// coveringLogicalOrdinals maps each covering-row column name to its slot in
// the record's LOGICAL row type. Returns nil (keep the index layout) when the
// logical type is unknown or ANY column has no logical slot — never a partial
// mapping, so a row is either fully logical-shaped or fully index-shaped.
func coveringLogicalOrdinals(posNames []string, logicalType *values.RecordType) []int {
	if logicalType == nil {
		return nil
	}
	ords := make([]int, len(posNames))
	for i, name := range posNames {
		ord, ok := logicalType.FieldIndex(name)
		if !ok {
			return nil
		}
		ords[i] = ord
	}
	return ords
}

// buildCoveringLogicalRow constructs a covering-index row in the record's
// LOGICAL shape: one slot per record field in descriptor order, covered
// columns placed at their logical ordinals, non-covered fields nil (Java's
// unset partial-record fields; the planner's covering gate guarantees they
// are never referenced). A value-column/PK-column name collision lands on the
// SAME logical slot with the same value — the logical row has one column.
func buildCoveringLogicalRow(columns, pkColumns []string, vals, pk tuple.Tuple, logicalType *values.RecordType, logicalOrds []int) *PositionalRow {
	slots := make([]any, len(logicalType.Fields))
	for i := range columns {
		if i < len(vals) {
			slots[logicalOrds[i]] = tupleElementToRowValue(vals[i])
		}
	}
	// PrimaryKey() may include a record type key prefix (e.g., (recTypeKey, id));
	// the user-level PK columns are at the tail. Skip the prefix.
	pkOffset := 0
	if len(pk) > len(pkColumns) {
		pkOffset = len(pk) - len(pkColumns)
	}
	for i := range pkColumns {
		idx := i + pkOffset
		if idx < len(pk) {
			slots[logicalOrds[len(columns)+i]] = tupleElementToRowValue(pk[idx])
		}
	}
	return &PositionalRow{Type: logicalType, Slots: slots}
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
		pred: func(qr QueryResult) (bool, error) {
			if qr.Record == nil || qr.Record.RecordType == nil {
				return false, nil
			}
			return allowed[qr.Record.RecordType.Name], nil
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
	needsRowCtx := hasBindingContext(evalCtx)
	// When the input flows a 2-way ordinal join's merged
	// positional row, filter predicates evaluate under the LEG WINDOWS —
	// computed once, from the input plan's result value.
	legSpans, windowsOK := downstreamLegWindows(p.GetInner())
	filtered := &filterResultCursor{
		inner: innerCursor,
		pred: func(qr QueryResult) (bool, error) {
			var rowCtx any
			if qr.Positional != nil && windowsOK {
				// The merged positional row of a gated 2-way
				// ordinal join — a leg reference QOV(leg).col needs
				// its leg window (unconditional: even with no binding context,
				// the leg bindings are required; the bare merged row misreads
				// leg-relative ordinals — a wrong-slot hazard).
				rowCtx = legWindowRowContext(qr.Positional, evalCtx, legSpans)
			} else if qr.Positional != nil {
				// The non-join frontier flows an authoritative
				// ordinal row — resolve filter predicates by ordinal (loud on a
				// miss, no name-map fallback).
				rowCtx = frontierRowContext(qr.Positional, evalCtx, needsRowCtx)
			}
			for _, pred := range preds {
				res, err := pred.Eval(rowCtx)
				if err != nil {
					return false, err
				}
				if res != predicates.TriTrue {
					return false, nil
				}
			}
			return true, nil
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

	// Runtime row cap (RFC-156 parameterized vector rank limit `... <= ?`):
	// evaluate the Value against the bound parameters. The Value already carries
	// the rank adjustment (K for rank<=K, K-1 for rank<K), so its result is the
	// final cap. A non-positive cap (e.g. K-1 with K=1, i.e. ROW_NUMBER() < 1)
	// yields 0 rows — the same EMPTY result a literal Limit(0) gives.
	if lv := p.GetLimitValue(); lv != nil {
		ev, err := lv.Evaluate(evalCtx)
		if err != nil {
			return nil, fmt.Errorf("limit value: %w", err)
		}
		n, ok := toLimitInt(ev)
		if !ok {
			return nil, fmt.Errorf("limit value is not an integer (%T)", ev)
		}
		if n < 0 {
			n = 0
		}
		limit = n
	}

	// RFC-128 §3.3: envelope the LIMIT continuation so the skip/limit state
	// survives the per-page transaction rollover that paginatingRows does.
	// The shared skipCursor/limitRowsCursor (cursor_combinators.go, driven by
	// applySkipLimit from 23 sites) forward the inner continuation with no
	// skip/limit bookkeeping — exactly like Java's SkipCursor/RowLimitedCursor
	// — so resuming them re-skips `offset` and resets `limit`. We therefore
	// keep those byte-identical and confine the envelope to THIS operator: a
	// LIMIT-specific continuation that records {inner continuation, remaining
	// offset, remaining limit}. On resume we decode it, drive the child from
	// the inner continuation, and continue skipping/limiting from where the
	// previous page stopped — never re-skipping, never resetting the cap.
	innerCont, remOffset, remLimit, decErr := decodeLimitContinuation(continuation, offset, limit)
	if decErr != nil {
		return nil, fmt.Errorf("invalid limit continuation: %w", decErr)
	}

	// Go-only extension: propagate the effective row limit to the inner plan
	// so downstream scans stop early. The child must produce remOffset rows to
	// skip PLUS however many this LIMIT may emit. Under an existing parent
	// returned-row cap (e.g. MAX_ROWS) the LIMIT emits at most that many
	// post-offset, so the child budget is remOffset + min(remLimit, parentCap)
	// — NOT min(remOffset+remLimit, parentCap), which would stop the child
	// before it skips the offset (`SELECT COUNT(*) FROM t LIMIT 1 OFFSET 1`
	// under MAX_ROWS=1 erroring on resume instead of returning 0 rows).
	innerProps := props
	emit := remLimit // <0 == unbounded (OFFSET-only)
	if pc := props.ReturnedRowLimit; pc > 0 && (emit < 0 || pc < emit) {
		emit = pc
	}
	if emit >= 0 {
		innerProps.ReturnedRowLimit = remOffset + emit
	}

	innerCursor, err := ExecutePlan(ctx, children[0], store, evalCtx, innerCont, innerProps)
	if err != nil {
		return nil, err
	}

	return newLimitEnvelopeCursor(innerCursor, remOffset, remLimit), nil
}

// limitEnvelopeCursor performs RFC-128's LIMIT/OFFSET (skip `remOffset`, then
// emit at most `remLimit`) over its inner cursor, AND wraps each result's
// continuation in a LimitContinuation so a cross-page resume continues from the
// exact skip/limit position instead of re-skipping/re-limiting. It deliberately
// re-implements skip-then-limit inline (rather than reusing the shared
// SkipCursor/RowLimitedCursor) so it can observe each skip and emit and record
// the remaining counts — the shared combinators are kept byte-identical to Java
// because they are driven generically from 23 operator sites via applySkipLimit.
type limitEnvelopeCursor struct {
	inner     recordlayer.RecordCursor[QueryResult]
	remOffset int
	remLimit  int
	// unbounded means "no row cap, OFFSET only" — the LIMIT was negative
	// (LogicalLimit.Limit < 0). SQL `OFFSET n` without a LIMIT is not valid in
	// this grammar, so this is currently unreachable from SQL; the guard keeps
	// a negative limit from collapsing the result to empty if a future caller
	// ever produces one.
	unbounded bool
	// terminal caches a sticky no-next result (matching RowLimitedCursor's
	// cached-terminal behavior): once set, every further OnNext returns it.
	// Holds BOTH the exhaustion terminal and an out-of-band stop — Java caches
	// every no-next result, so a re-call replays verbatim and never re-pulls
	// the inner.
	terminal *recordlayer.RecordCursorResult[QueryResult]
	closed   bool
}

func newLimitEnvelopeCursor(inner recordlayer.RecordCursor[QueryResult], remOffset, remLimit int) *limitEnvelopeCursor {
	if remOffset < 0 {
		remOffset = 0
	}
	return &limitEnvelopeCursor{
		inner:     inner,
		remOffset: remOffset,
		remLimit:  remLimit,
		unbounded: remLimit < 0,
	}
}

func (c *limitEnvelopeCursor) OnNext(ctx context.Context) (recordlayer.RecordCursorResult[QueryResult], error) {
	if c.terminal != nil {
		return *c.terminal, nil
	}

	// Limit fully consumed (also covers LIMIT 0 and a resume with remLimit==0):
	// the operator is EXHAUSTED — it will never emit another row — so it stops
	// with SourceExhausted+EndContinuation. This is the correct terminal in the
	// paginatingRows architecture: an end continuation ends the page drain (a
	// resumable continuation here would loop forever, since each fresh page
	// would rebuild an already-empty window). Java's RowLimitedCursor returns
	// the in-band RETURN_LIMIT_REACHED and lets the client driver stop; in our
	// page-drain driver the equivalent "nothing more, ever" signal is end.
	if !c.unbounded && c.remLimit <= 0 {
		return c.exhaust(), nil
	}

	for {
		if err := ctx.Err(); err != nil {
			return recordlayer.RecordCursorResult[QueryResult]{}, err
		}
		result, err := c.inner.OnNext(ctx)
		if err != nil {
			return result, err
		}
		if !result.HasNext() {
			reason := result.GetNoNextReason()
			if reason == recordlayer.SourceExhausted {
				// Inner drained: the LIMIT is genuinely exhausted, no resume.
				return c.exhaust(), nil
			}
			// Inner stopped out-of-band (page/scan boundary): envelope the
			// inner continuation with the CURRENT remaining offset/limit so the
			// next page resumes mid-window (the next request opens a fresh
			// cursor from this continuation). Cached in terminal so a
			// contract-violating re-call on THIS instance replays it verbatim
			// (Java's cached no-next result) instead of re-pulling the inner.
			contBytes, encErr := encodeLimitContinuation(result.GetContinuation(), c.remOffset, c.remLimit)
			if encErr != nil {
				return recordlayer.RecordCursorResult[QueryResult]{}, encErr
			}
			res := recordlayer.NewResultNoNext[QueryResult](
				reason, recordlayer.NewBytesContinuation(contBytes),
			)
			c.terminal = &res
			return res, nil
		}

		if c.remOffset > 0 {
			c.remOffset--
			continue // OFFSET: skip this row.
		}

		// Emit this row. After the emit one fewer row remains in the window;
		// the envelope records the inner continuation positioned PAST this row
		// plus remOffset==0 and the decremented remLimit, so a resume neither
		// re-skips nor re-emits it. (Unbounded: remLimit stays negative, the
		// cap never fires — OFFSET-only stream.)
		if !c.unbounded {
			c.remLimit--
		}
		contBytes, encErr := encodeLimitContinuation(result.GetContinuation(), 0, c.remLimit)
		if encErr != nil {
			return recordlayer.RecordCursorResult[QueryResult]{}, encErr
		}
		return recordlayer.NewResultWithValue(result.GetValue(), recordlayer.NewBytesContinuation(contBytes)), nil
	}
}

// exhaust builds and caches the sticky terminal SourceExhausted result. Once the
// LIMIT window is fully emitted (or empty), the operator is done for good — the
// page-drain driver stops on the EndContinuation.
func (c *limitEnvelopeCursor) exhaust() recordlayer.RecordCursorResult[QueryResult] {
	res := recordlayer.NewResultNoNext[QueryResult](
		recordlayer.SourceExhausted, &recordlayer.EndContinuation{},
	)
	c.terminal = &res
	return res
}

func (c *limitEnvelopeCursor) Close() error {
	c.closed = true
	return c.inner.Close()
}

func (c *limitEnvelopeCursor) IsClosed() bool { return c.closed }

// LimitContinuation is the RFC-128 §3.3 envelope for a RecordQueryLimitPlan's
// continuation: the inner cursor's continuation plus the skip/limit window left
// to apply. It is Go-only and INTERNAL to executeLimit — it never becomes a SQL
// resume token or a wire/Java-interop continuation (no .proto, no
// magic-6773487359078157740 surface). The encoding is a hand-rolled
// length-prefixed blob, sufficient because it round-trips only within this
// process across paginatingRows' per-page transaction rollover.
//
// Layout (all integers big-endian):
//
//	[1]   version byte (limitContVersion)
//	[8]   remaining offset (int64)
//	[8]   remaining limit  (int64)
//	[4]   inner continuation length (uint32; 0xFFFFFFFF == "nil/no inner")
//	[...] inner continuation bytes
const limitContVersion byte = 1

// limitContNilInner marks an absent inner continuation (start-from-begin),
// distinct from a present-but-empty inner continuation (length 0).
const limitContNilInner uint32 = 0xFFFFFFFF

func encodeLimitContinuation(innerCont recordlayer.RecordCursorContinuation, remOffset, remLimit int) ([]byte, error) {
	var innerBytes []byte
	haveInner := false
	if innerCont != nil && !innerCont.IsEnd() {
		b, err := innerCont.ToBytes()
		if err != nil {
			return nil, err
		}
		// ToBytes returns nil for Start/End-like positions; treat a nil byte
		// slice as "no inner continuation" so resume starts the child fresh.
		if b != nil {
			innerBytes = b
			haveInner = true
		}
	}

	buf := make([]byte, 0, 1+8+8+4+len(innerBytes))
	buf = append(buf, limitContVersion)
	buf = appendInt64BE(buf, int64(remOffset))
	buf = appendInt64BE(buf, int64(remLimit))
	if haveInner {
		buf = appendUint32BE(buf, uint32(len(innerBytes)))
		buf = append(buf, innerBytes...)
	} else {
		buf = appendUint32BE(buf, limitContNilInner)
	}
	return buf, nil
}

// decodeLimitContinuation parses a LimitContinuation. An empty continuation
// (first page) yields (nil inner, fullOffset, fullLimit).
func decodeLimitContinuation(continuation []byte, fullOffset, fullLimit int) (innerCont []byte, remOffset, remLimit int, err error) {
	if len(continuation) == 0 {
		return nil, fullOffset, fullLimit, nil
	}
	if len(continuation) < 1+8+8+4 {
		return nil, 0, 0, fmt.Errorf("limit continuation too short: %d bytes", len(continuation))
	}
	if continuation[0] != limitContVersion {
		return nil, 0, 0, fmt.Errorf("unknown limit continuation version %d", continuation[0])
	}
	pos := 1
	ro := int64(readUint64BE(continuation[pos:]))
	pos += 8
	rl := int64(readUint64BE(continuation[pos:]))
	pos += 8
	innerLen := readUint32BE(continuation[pos:])
	pos += 4
	if innerLen == limitContNilInner {
		// Defence-in-depth: a nil-inner envelope must have no trailing bytes
		// (the encoder writes none). Reject a malformed continuation rather than
		// silently ignoring junk — symmetric with the length check below.
		if pos != len(continuation) {
			return nil, 0, 0, fmt.Errorf("limit continuation: %d trailing bytes after nil inner", len(continuation)-pos)
		}
		return nil, int(ro), int(rl), nil
	}
	if int64(pos)+int64(innerLen) != int64(len(continuation)) {
		return nil, 0, 0, fmt.Errorf("limit continuation inner length mismatch: want %d, have %d", innerLen, len(continuation)-pos)
	}
	inner := make([]byte, innerLen)
	copy(inner, continuation[pos:])
	return inner, int(ro), int(rl), nil
}

func appendInt64BE(b []byte, v int64) []byte {
	return appendUint64BE(b, uint64(v))
}

func appendUint64BE(b []byte, v uint64) []byte {
	return append(b,
		byte(v>>56), byte(v>>48), byte(v>>40), byte(v>>32),
		byte(v>>24), byte(v>>16), byte(v>>8), byte(v))
}

func appendUint32BE(b []byte, v uint32) []byte {
	return append(b, byte(v>>24), byte(v>>16), byte(v>>8), byte(v))
}

func readUint64BE(b []byte) uint64 {
	return uint64(b[0])<<56 | uint64(b[1])<<48 | uint64(b[2])<<40 | uint64(b[3])<<32 |
		uint64(b[4])<<24 | uint64(b[5])<<16 | uint64(b[6])<<8 | uint64(b[7])
}

func readUint32BE(b []byte) uint32 {
	return uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3])
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

	// RFC-130: the distinct seen-set is a cardinality-growing buffer (one
	// key string per distinct row, held for the whole scan). Charge each NEW
	// key's bytes against the statement memory budget via boundedSet.
	seen := newBoundedSet[string](props.State)
	filtered := &filterResultCursor{
		inner: innerCursor,
		pred: func(qr QueryResult) (bool, error) {
			key, err := distinctKey(qr)
			if err != nil {
				return false, err
			}
			added, err := seen.Add(key, int64(len(key)))
			if err != nil {
				return false, err
			}
			return added, nil
		},
	}
	// Live-bytes model: the seen-set is rebuilt (and re-charged) per page;
	// release its tally at page teardown. The hook reads Charged() at close
	// time because the set grows while the page streams.
	return newCloseHookCursor(
		applySkipLimit(filtered, props.Skip, props.ReturnedRowLimit),
		func() { props.State.ReleaseMemory(seen.Charged()) },
	), nil
}

// distinctKey builds a DISTINCT dedup key by packing the row's ordinal slot
// values into an FDB tuple, mirroring Java's RecordQueryUnorderedDistinctPlan,
// which dedups via Set<Key.Evaluated> — structured value equality, never a
// delimiter-joined string. Tuple encoding is length-prefixed and type-tagged,
// so no slot value can forge an inter-column boundary (a string containing the
// old "|NAME=type:" separator no longer collides two distinct rows) and SQL
// NULL has its own tuple code, distinct from any string. Names are not part of
// the key: a physical distinct's inner produces ONE schema, so every row shares
// the same slot names in the same positions — the value tuple alone is the
// comparison key, exactly as Java's comparison-key VALUES are. A nil-Positional
// row yields the empty key.
func distinctKey(qr QueryResult) (string, error) {
	if qr.Positional == nil || qr.Positional.Type == nil {
		return "", nil
	}
	return packedDedupKey(qr.Positional.Slots)
}

// packedDedupKey encodes a row's slot values into an unambiguous FDB-tuple key
// for DISTINCT / UNION-DISTINCT deduplication — the single encoder every dedup
// path routes through (distinctKey, cteDedupKeyer.key, queryResultKey). Tuple
// encoding is typed and length-prefixed, so no value can forge an inter-column
// boundary (the defect of a '|'-joined %v string, where a value containing the
// delimiter collapsed two different rows) and NULL has its own tuple code
// distinct from any string (the "\x00NULL\x00"-sentinel collision). Java dedups
// via Set<Key.Evaluated> structured value equality; this is the Go equivalent.
// The [16]byte→UUID arm mirrors intersectionCompKeyFunc's tuple
// canonicalization; the composite %T:%v fallback keeps distinct concrete types
// key-distinct. (The merge-sort union dedups via compareValues on evaluated
// keys instead — Java UnionCursor's advance-all-equal — so it no longer packs
// a dedup key at all.)
func packedDedupKey(slots []any) (string, error) {
	t := make(tuple.Tuple, len(slots))
	for i, v := range slots {
		switch tv := v.(type) {
		case nil, int64, int, uint, uint64, float32, float64, string, []byte, bool:
			t[i] = tv
		case int32:
			t[i] = int64(tv)
		case [16]byte:
			t[i] = tuple.UUID(tv)
		default:
			// Composite/nested slot (struct, array, message): the tuple layer
			// cannot pack it. Encode LOSSLESSLY via the continuation codec
			// (type-tagged, length-prefixed, recursive) as one []byte tuple
			// slot — boundary-safe AND collision-free. The retired %T:%v
			// rendering collided on composites ([]any{"a b"} vs
			// []any{"a","b"} both rendered "[a b]") and split equal protos
			// across generated/dynamicpb representations (RFC-180 C1).
			b, err := appendContValue(nil, v)
			if err != nil {
				return "", fmt.Errorf("dedup key slot %d: %w", i, err)
			}
			t[i] = b
		}
	}
	return string(t.Pack()), nil
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
	// On the positional frontier an outer correlation resolves via
	// the eval context's binder before the bare-positional frontier fallback.
	posNeedsCtx := hasBindingContext(evalCtx)
	// The projection's output schema is row-invariant — compute it ONCE.
	// posNames names the EMITTED positional
	// row's slots by OUTPUT name — alias-preferring, via the shared
	// values.OutputColumnName authority (the recursive-CTE leg wrap re-reads by
	// the same rule) — so a downstream ordinal consumer resolves this derived
	// table's OUTPUT columns, not the source column a field ref reads from — the
	// buried-reference fix's ordinal counterpart.
	posNames := make([]string, len(projections))
	for i, proj := range projections {
		alias := ""
		if i < len(aliases) {
			alias = aliases[i]
		}
		posNames[i] = values.OutputColumnName(proj, alias)
	}
	projType := positionalTypeFromNames(posNames)
	// When the input flows a 2-way ordinal join's merged
	// positional row, projections evaluate under the LEG WINDOWS — computed
	// once, from the input plan's result value.
	legSpans, windowsOK := downstreamLegWindows(p.GetInner())
	var evalErr error
	mapped := recordlayer.MapCursor(innerCursor, func(qr QueryResult) QueryResult {
		if evalErr != nil {
			return qr
		}
		slots := make([]any, len(projections))
		var rowCtx any
		if qr.Positional != nil && windowsOK {
			// The merged positional row of a gated 2-way
			// ordinal join — a leg reference QOV(leg).col needs its
			// leg window (unconditional; see executeFilter).
			rowCtx = legWindowRowContext(qr.Positional, evalCtx, legSpans)
		} else if qr.Positional != nil {
			// The non-join frontier flows an authoritative ordinal
			// row — resolve projections by ordinal (loud on a miss).
			rowCtx = frontierRowContext(qr.Positional, evalCtx, posNeedsCtx)
		}
		for i, proj := range projections {
			val, err := proj.Evaluate(rowCtx)
			if err != nil {
				evalErr = err
				return qr
			}
			slots[i] = val // dense positional slot (kept even on dup names)
		}
		// A projection's output IS a PositionalRow — ALWAYS emit it, built by
		// parallel construction from the projected values, named by the output
		// schema (projType).
		return QueryResult{
			Positional: &PositionalRow{Type: projType, Slots: slots},
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
	// lastNoNext replays the terminal result on a contract-violating re-call
	// (Java's cached no-next result) — never re-pulls the inner.
	lastNoNext *recordlayer.RecordCursorResult[QueryResult]
}

func (c *errCheckCursor) OnNext(ctx context.Context) (recordlayer.RecordCursorResult[QueryResult], error) {
	if c.lastNoNext != nil {
		return *c.lastNoNext, nil
	}
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
	if !result.HasNext() {
		c.lastNoNext = &result
	}
	return result, nil
}

func (c *errCheckCursor) Close() error   { return c.inner.Close() }
func (c *errCheckCursor) IsClosed() bool { return c.inner.IsClosed() }

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
			return executeUnionStreaming(ctx, inners, store, evalCtx, continuation, props, md, firstBranchKeys)
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
	continuation []byte,
	props recordlayer.ExecuteProperties,
	md *recordlayer.RecordMetaData,
	targetKeys []string,
) (recordlayer.RecordCursor[QueryResult], error) {
	// Each branch is a lazy CursorFactory resumable from its OWN continuation,
	// folded through recordlayer.ConcatCursors — Java's ConcatCursor with the
	// wire-compatible branch-tagged ConcatContinuation proto (an N-way union
	// is a right-nested chain of binary concats). Before RFC-180 (A1) the
	// incoming continuation was silently DISCARDED and every child started at
	// nil, so a mid-union page break replayed the union from row 0 on resume:
	// an outer aggregate that correctly restores its own partial state then
	// re-consumed the whole union per page (SUM over a grouped UNION ALL
	// returned 3×13=39 for 13 — yamsql union_aggregate_java).
	branchFactory := func(i int) recordlayer.CursorFactory[QueryResult] {
		inner := inners[i]
		return func(cont []byte) recordlayer.RecordCursor[QueryResult] {
			c, err := ExecutePlan(ctx, inner, store, evalCtx, cont, props.ClearSkipAndLimit())
			if err != nil {
				return &errResultCursor{err: fmt.Errorf("union branch %d: %w", i, err)}
			}
			if i > 0 {
				srcKeys := planColumnNamesWithMD(inner, md)
				if srcKeys != nil && !slices.Equal(srcKeys, targetKeys) {
					c = recordlayer.MapCursor(c, func(qr QueryResult) QueryResult {
						return remapUnionColumnsByPosition(qr, srcKeys, targetKeys)
					})
				}
			}
			return c
		}
	}
	factories := make([]recordlayer.CursorFactory[QueryResult], len(inners))
	for i := range inners {
		factories[i] = branchFactory(i)
	}
	return applySkipLimit(concatFactories(factories, continuation), props.Skip, props.ReturnedRowLimit), nil
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
	// This buffered fallback (branch column names not statically known)
	// cannot resume: before RFC-180 (A2) it fed the PARENT's continuation
	// verbatim to EVERY child — a raw-key scan child consumed it as a scan
	// position, silently starting mid-stream. Correct-or-loud: no
	// continuation support until per-branch states land (WS-A follow-up);
	// the streaming path above (all names known) resumes properly.
	if len(continuation) > 0 {
		return nil, &UnsupportedContinuationError{Shape: "buffered union (branch column names not statically known)"}
	}
	var all []QueryResult
	var allCharged int64
	for branchIdx, inner := range inners {
		cursor, err := ExecutePlan(ctx, inner, store, evalCtx, nil, props.ClearSkipAndLimit())
		if err != nil {
			props.State.ReleaseMemory(allCharged)
			return nil, err
		}
		items, charged, err := CollectAllBounded(ctx, cursor, props.State, props.GetMaterializationLimit(), "buffered union branch")
		allCharged += charged
		cursor.Close()
		if err != nil {
			props.State.ReleaseMemory(allCharged)
			return nil, err
		}
		branchKeys := planColumnNames(inner)
		if branchIdx == 0 {
			firstBranchKeys = branchKeys
			if len(firstBranchKeys) == 0 && len(items) > 0 && items[0].Positional != nil {
				firstBranchKeys = items[0].Positional.TypeNames()
			}
		}
		if branchIdx > 0 && len(firstBranchKeys) > 0 {
			targetKeys := firstBranchKeys
			srcKeys := branchKeys
			if len(srcKeys) == 0 && len(items) > 0 && items[0].Positional != nil {
				srcKeys = items[0].Positional.TypeNames()
			}
			for i := range items {
				items[i] = remapUnionColumnsByPosition(items[i], srcKeys, targetKeys)
			}
		}
		// RFC-130: the cross-branch `all` slice holds exactly the rows already
		// charged per branch by CollectAllBounded above (the per-branch `items`
		// slices are GC'd; `all` is the surviving copy). Charging again here
		// would double-count the same resident rows, so this append is plain —
		// the budget is already advanced by the per-branch CollectAllBounded.
		all = append(all, items...)
	}
	return newChargeReleasingCursor(
		applySkipLimit(recordlayer.FromList(all), props.Skip, props.ReturnedRowLimit),
		props.State, allCharged,
	), nil
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
				alias := ""
				if i < len(aliases) {
					alias = aliases[i]
				}
				names[i] = values.OutputColumnName(v, alias)
			}
			return names
		}
		// A RecordQueryMapPlan reports its OWN output column names from its result value
		// — do NOT descend through it to the pre-rename names. Mirrors
		// physicalPlanColumnNames (rule_implement_unordered_union.go) so a branch that
		// ImplementUnorderedUnionRule already wrapped in a rename Map reports the SAME
		// (post-rename) names here. Without this, the union position-remap would see the
		// pre-rename names, differ from the first branch, and remap a SECOND time over the
		// already-renamed row → reads missing keys → NULLs. Falls through to the
		// descend/scan path when the Map has no RecordConstructorValue result.
		if mp, ok := p.(*plans.RecordQueryMapPlan); ok {
			if rcv, ok := mp.GetResultValue().(*values.RecordConstructorValue); ok && len(rcv.Fields) > 0 {
				names := make([]string, len(rcv.Fields))
				for i, f := range rcv.Fields {
					// Report the EXACT field name — RecordConstructorValue.Evaluate keys the
					// output row by f.Name verbatim (values.go), so this is the literal row
					// key the union remap must read. Upper-casing it would mismatch a
					// non-uppercase Map field and read a missing key → NULL. Union
					// branch fields are upper in practice (SQL upper-cases identifiers/aliases),
					// so this equals the prior upper-case for every real query.
					names[i] = f.Name
				}
				return names
			}
			sawMap = true
		}
		// A bare STREAMING-AGGREGATE plan defines its OWN output schema (group keys +
		// aggregate outputs) — report it, do NOT descend to the input scan. StreamingAgg
		// implements innerPlanAccessor, so without this the loop walks past it to the Scan
		// and returns the scan's columns, mis-naming the branch for the UNION position-remap
		// and silently dropping a mismatched-alias aggregate branch's rows (RFC-078, TODO
		// 7.6-union-remap). The names match the keys aggregateCursor writes (streaming_cursors.go)
		// and the schema the translator derives (aggregateOutputColumns).
		//
		//
		// INVARIANT (RFC-081): every physical realization of a bare aggregate union
		// branch MUST report its output schema here — the gate (unionBranchNormalizable)
		// admits a bare LogicalAggregate on the assumption that whatever it plans as is
		// reportable. The three realizations are StreamingAgg, AggregateIndex, and
		// MultiIntersection (all handled below). A future aggregate physical plan added
		// without an arm here would fall through to nil and silently mis-remap a union branch
		// → wrong rows. Add the arm with the new plan.
		if agg, ok := p.(*plans.RecordQueryStreamingAggregationPlan); ok {
			return streamingAggOutputNames(agg)
		}
		// A bare AGGREGATE-INDEX plan likewise defines its own output schema (group cols +
		// the canonical aggregate name). Its GetResultType is UnknownType, so without this it
		// would fall through to nil and a grouped aggregate-index UNION branch could not be
		// position-remapped (RFC-081). OutputColumnNames returns exactly the keys the
		// aggregateIndexCursor writes. A bare aggregate-index branch is always UNALIASED (an
		// aliased SELECT tops with a Project), so there is no alias to carry here.
		if aggIdx, ok := p.(*plans.RecordQueryAggregateIndexPlan); ok {
			return aggIdx.OutputColumnNames()
		}
		// A MULTI-aggregate-intersection plan's result value (a RecordConstructorValue) names
		// its output columns; the merge cursor keys each row by those exact field names
		// (RecordConstructorValue.Evaluate). Report them VERBATIM — the GetResultType fallback
		// below would upper-case them, which only matches because the names are upper in
		// practice; reading f.Name directly is byte-identical to the row keys regardless
		// (mirrors the MapPlan arm, RFC-078). RFC-081.
		if mi, ok := p.(*plans.RecordQueryMultiIntersectionOnValuesPlan); ok {
			if rcv, ok := mi.GetResultValue().(*values.RecordConstructorValue); ok && len(rcv.Fields) > 0 {
				names := make([]string, len(rcv.Fields))
				for i, f := range rcv.Fields {
					names[i] = f.Name
				}
				return names
			}
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
	// A UNION aligns legs BY ORDINAL (SQL positional union) — re-type the
	// leg's Positional to the union's output column names (leg 2's [REGION] → the
	// output's [STATUS]); the slots stay in ordinal place, only the names change.
	// The first leg's names already ARE the
	// output names, so it flows through unchanged.
	if qr.Positional == nil || len(srcKeys) != len(targetKeys) || len(qr.Positional.Slots) != len(targetKeys) {
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
	return QueryResult{
		Positional: &PositionalRow{Type: positionalTypeFromNames(targetKeys), Slots: qr.Positional.Slots},
		Record:     qr.Record,
		PrimaryKey: qr.PrimaryKey,
	}
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

	cursors, resume, err := buildIntersectionChildCursors(ctx, inners, store, evalCtx, continuation, props)
	if err != nil {
		return nil, err
	}

	keyVals := p.GetComparisonKeyValues()
	compKeyFunc := intersectionCompKeyFunc(keyVals)
	return applySkipLimit(
		recordlayer.IntersectionResume(cursors, compKeyFunc, false, resume),
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
// The returned resume slice seeds each child's mergeChildState continuation
// (via IntersectionResume) so the next checkpoint re-encodes MID/END/START
// children correctly. With a nil/empty incoming continuation every child is
// START (unchanged first-page behavior). Shared by executeIntersection and
// executeMultiIntersection.
func buildIntersectionChildCursors(
	ctx context.Context,
	inners []plans.RecordQueryPlan,
	store *recordlayer.FDBRecordStore,
	evalCtx *EvaluationContext,
	continuation []byte,
	props recordlayer.ExecuteProperties,
) ([]recordlayer.RecordCursor[QueryResult], []recordlayer.IntersectionChildResume, error) {
	resume, err := recordlayer.DecodeIntersectionContinuation(continuation, len(inners))
	if err != nil {
		return nil, nil, err
	}
	childProps := props.ClearSkipAndLimit()
	cursors := make([]recordlayer.RecordCursor[QueryResult], len(inners))
	for i, inner := range inners {
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
	return cursors, resume, nil
}

// intersectionCompKeyFunc builds a ComparisonKeyFunc that extracts a
// tuple-encoded comparison key from a QueryResult. Uses the plan's
// comparison-key values when available, falls back to PrimaryKey, then
// to a string representation of the datum.
// widenInt32 normalizes an intersection/union merge comparison-key element so the
// FDB tuple layer can Pack it. The tuple layer has no int32 case — Pack panics on
// it — and the index key encoding already widens int32 columns to int64
// (key_expression_compiled.go), so widening here keeps the in-memory merge key
// byte-identical to the children's sort order (int32->int64 sign-extension is
// value-preserving and tuple integer encoding is monotonic). Matches Java, whose
// Tuple stores Long and never sees a 32-bit key element. Only int32 is handled:
// it's the unique order-preserving widening and the only confirmed-reachable
// unpackable comparison-key type (field reads pre-widen at query_result.go); a
// genuinely exotic type stays raw so compareKeys' Pack-error path catches it rather
// than risk a non-monotonic coercion. See RFC-092.
func widenInt32(v any) any {
	if i32, ok := v.(int32); ok {
		return int64(i32)
	}
	return v
}

// compKeyEvalArg is the eval argument for an intersection/merge-sort COMPARISON
// KEY extraction. When the row carries an
// authoritative ordinal row (qr.Positional != nil — every merge/intersection
// branch is a plan whose output is positional), the comparison-key value resolves
// against that bare PositionalRow (FieldValue.Evaluate → evaluateOrdinal: by
// ordinal, loud on a miss — never a name-keyed read). A comparison key is
// a field extraction with no param / subquery / correlation binding, so the bare
// positional row is the whole context — frontierRowContext(pos, nil, false)
// collapses to exactly this. Both merge branches resolve the SAME field by
// ordinal, so merge/intersection order is preserved byte-for-byte.
func compKeyEvalArg(qr QueryResult) any {
	return qr.Positional
}

func intersectionCompKeyFunc(keyVals []values.Value) recordlayer.ComparisonKeyFunc[QueryResult] {
	return func(qr QueryResult) tuple.Tuple {
		if len(keyVals) > 0 {
			t := make(tuple.Tuple, len(keyVals))
			for i, kv := range keyVals {
				// Comparison/merge keys are field extractions over the
				// row; the runtime typed-error family is unreachable
				// here. ComparisonKeyFunc has no error channel, so a
				// stray error is a planner invariant violation (panic,
				// matching the prior no-recover behaviour). Resolves
				// against the ordinal row via compKeyEvalArg.
				v, err := kv.Evaluate(compKeyEvalArg(qr))
				if err != nil {
					panic(err)
				}
				// widenInt32: tuple has no int32 (RFC-092). uuidToTupleElement:
				// a UUID comparison/PK key is a neutral [16]byte, which the tuple
				// packer cannot encode — convert it to tuple.UUID, exactly as
				// packedDedupKey does, so compareKeys' Pack doesn't
				// panic on an intersection over a UUID key (RFC-162).
				t[i] = uuidToTupleElement(widenInt32(v))
			}
			return t
		}
		if qr.PrimaryKey != nil {
			return qr.PrimaryKey
		}
		// No comparison keys and no PK: match rows by their FULL positional
		// content, encoded LOSSLESSLY via the continuation codec — the
		// retired fmt %v rendering collapsed distinct composite rows and
		// collapsed every nil-layout row into one. Encode failure is a
		// planner invariant violation (this closure's documented no-error-
		// channel contract, as with Evaluate above).
		if qr.Positional == nil {
			// A row with no keys, no PK and no positional content cannot be
			// matched against anything; a constant key would intersect ALL
			// such rows. Planner invariant violation (documented contract).
			panic(fmt.Errorf("intersection row carries no comparison keys, primary key, or positional content — malformed plan"))
		}
		b, err := appendContValue(nil, qr.Positional.Slots)
		if err != nil {
			panic(err)
		}
		return tuple.Tuple{b}
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
		if err := proto.Unmarshal(continuation, &fmc); err != nil {
			// Java: RecordCursor.flatMapPipelined —
			//   throw new RecordCoreException("error parsing continuation", ex).
			// A corrupt continuation must fail, not silently restart from
			// scratch (which re-emits rows the caller already consumed).
			return nil, &recordlayer.ContinuationParseError{RawBytes: continuation, Cause: err}
		}
		outerCont = fmc.OuterContinuation
		innerCont = fmc.InnerContinuation
		checkValue = fmc.CheckValue
	}

	outerCursor, err := ExecutePlan(ctx, p.GetOuter(), store, evalCtx, outerCont, nestedProps)
	if err != nil {
		return nil, err
	}

	cursor, err := newFlatMapCursor(
		outerCursor, p.GetOuter(), p.GetInner(), store, evalCtx,
		p.GetOuterAlias(), p.GetInnerAlias(),
		p.GetResultValue(),
		nestedProps,
	)
	if err != nil {
		outerCursor.Close()
		return nil, err
	}
	cursor.initialInnerCont = innerCont
	cursor.hasPendingInner = innerCont != nil
	cursor.pendingCheckValue = checkValue
	// Seed lastOuterContinuation from the saved OuterContinuation whenever one is
	// present — NOT only when an inner continuation is also present. The very next
	// outer advance copies it into priorOuterContinuation (the position AT the
	// resumed outer row), which is what a subsequent mid-inner buildContinuation
	// pairs with the check_value. If a page ended via wrapOuterContinuation (outer
	// hit an out-of-band limit → innerCont nil but outerCont set), gating on
	// innerCont != nil left priorOuterContinuation nil on the resumed page, so the
	// next mid-inner continuation encoded an EMPTY OuterContinuation (resume from
	// the start) while carrying a non-start check_value → check_value mismatch on
	// the following resume.
	if outerCont != nil {
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
	innerRows, innerCharged, err := CollectAllBounded(ctx, innerCursor, props.State, props.GetMaterializationLimit(), "nested loop join inner side")
	innerCursor.Close()
	if err != nil {
		props.State.ReleaseMemory(innerCharged)
		return nil, err
	}

	// Decode the NLJ page continuation (nljContinuation: a
	// FlatMapContinuation envelope wrapping the outer position, the current
	// outer's check value, and the tuple-packed inner position). Anything
	// else — including the retired pre-continuation binary's one-byte fake
	// marker — declines loudly: forwarding unrecognized bytes to the outer
	// child risks a key-value cursor's raw-suffix fallback silently
	// accepting them as a scan position (wrong rows).
	if p.GetJoinType() == plans.JoinFullOuter && len(continuation) > 0 {
		// No FULL OUTER continuation resumes — the cross-outer matchedInner
		// bitmap has no serialized form. New tokens carry the declining FULL
		// marker, but a token minted by a PREVIOUS binary version packs
		// ordinary mid-inner state; decoding it here would rebuild the
		// bitmap zeroed and re-pad already-matched inner rows. Join-type
		// context is the authority, not the token's own claim.
		props.State.ReleaseMemory(innerCharged)
		return nil, &UnsupportedContinuationError{Shape: "nested loop join FULL OUTER (does not resume)"}
	}
	outerContinuation, nljResume, decodeErr := decodeNLJContinuation(continuation)
	if decodeErr != nil {
		props.State.ReleaseMemory(innerCharged)
		return nil, decodeErr
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
	outerCursor, err := ExecutePlan(ctx, p.GetOuter(), store, evalCtx, outerContinuation, outerProps)
	if err != nil {
		props.State.ReleaseMemory(innerCharged)
		return nil, err
	}

	cursor, err := newNLJCursor(
		outerCursor, innerRows,
		p.GetJoinType(), p.GetOuterAlias(), p.GetInnerAlias(),
		p.GetPredicates(), p.GetResultValue(), evalCtx, props.State,
	)
	if err != nil {
		outerCursor.Close()
		props.State.ReleaseMemory(innerCharged)
		return nil, err
	}
	// Live-bytes model: the inner side is re-materialized (and re-charged)
	// on every page against the statement-wide state; the cursor releases
	// its charges — inner rows plus any hash index — at page teardown. ADD
	// to the tally: newNLJCursor already accumulated the hash-index charges.
	cursor.chargedBytes += innerCharged
	cursor.armResume(outerContinuation, nljResume)
	return applySkipLimit(cursor, props.Skip, props.ReturnedRowLimit), nil
}

// concatLegPositionals builds a leg-windowed merged PositionalRow by CONCATENATING
// two leg rows' own Positionals (parallel construction). The merged Type carries
// top-level Legs [outerAlias@[0,Wo),
// innerAlias@[Wo,Wo+Wi)] so a qualified reference ("LA.K") binds its leg's window
// (rowLegsBinder / legWindowBinder); a bare read reads its baked slot. Nested Legs (a leg
// that is itself a merge) are preserved, the inner leg's shifted by Wo. Returns nil
// when either leg lacks a Positional. This is the merge for a NON-build join
// cursor (the nljCursor's build path OVERWRITES .Positional when a gated join
// has an ordinal seed).
// legFieldName names a leg's merged column: a bare-scalar UNNEST element (a
// 1-field `_0` row — `t.arr AS X`) is renamed to its AS alias so a downstream
// BARE read of the alias ("X") resolves to the element directly. Any other field
// keeps its own name.
func legFieldName(fieldName, alias string, legWidth int) string {
	if alias != "" && legWidth == 1 && fieldName == values.OrdinalFieldName(0) {
		return strings.ToUpper(alias)
	}
	return fieldName
}

func concatLegPositionals(outer, inner *PositionalRow, outerAlias, innerAlias string) *PositionalRow {
	if outer == nil || inner == nil || outer.Type == nil || inner.Type == nil {
		return nil
	}
	nOuter := len(outer.Type.Fields)
	nInner := len(inner.Type.Fields)
	fields := make([]values.Field, 0, nOuter+nInner)
	for i, f := range outer.Type.Fields {
		fields = append(fields, values.Field{Name: legFieldName(f.Name, outerAlias, nOuter), FieldType: f.FieldType, Ordinal: i})
	}
	for i, f := range inner.Type.Fields {
		fields = append(fields, values.Field{Name: legFieldName(f.Name, innerAlias, nInner), FieldType: f.FieldType, Ordinal: nOuter + i})
	}
	slots := make([]any, 0, len(outer.Slots)+len(inner.Slots))
	slots = append(slots, outer.Slots...)
	slots = append(slots, inner.Slots...)
	legs := make([]values.RecordTypeLeg, 0, len(outer.Type.Legs)+len(inner.Type.Legs)+2)
	if outerAlias != "" {
		legs = append(legs, values.RecordTypeLeg{Name: strings.ToUpper(outerAlias), Start: 0, Width: nOuter})
	}
	if innerAlias != "" {
		legs = append(legs, values.RecordTypeLeg{Name: strings.ToUpper(innerAlias), Start: nOuter, Width: nInner})
	}
	for _, lg := range outer.Type.Legs {
		legs = append(legs, lg)
	}
	for _, lg := range inner.Type.Legs {
		legs = append(legs, values.RecordTypeLeg{Name: lg.Name, Start: lg.Start + nOuter, Width: lg.Width})
	}
	return &PositionalRow{Type: &values.RecordType{Fields: fields, Legs: legs}, Slots: slots}
}

func mergeRows(outer, inner QueryResult, outerAlias, innerAlias string) QueryResult {
	// A join merge's output IS a leg-windowed PositionalRow — the two
	// leg rows' own Positionals concatenated (concatLegPositionals), with leg windows
	// so a qualified read (`A.ID`) resolves to its leg and a bare read by name-in-row.
	return QueryResult{
		Positional: concatLegPositionals(outer.Positional, inner.Positional, outerAlias, innerAlias),
		Record:     outer.Record,
		PrimaryKey: outer.PrimaryKey,
	}
}

// passesJoinPredicates evaluates a join's residual predicates against the merged
// leg-windowed positional row (the nil-legs entry point to passesJoinPredicatesLegs).
func passesJoinPredicates(combined QueryResult, preds []predicates.QueryPredicate, evalCtx *EvaluationContext) (bool, error) {
	return passesJoinPredicatesLegs(combined, preds, evalCtx, nil)
}

// passesJoinPredicatesLegs is passesJoinPredicates extended with the
// ordinal-build leg bindings. legs nil (the non-build merged-row path)
// resolves through the merged row's leg windows (spansFromMergedLegs below).
// legs non-nil (an ordinal-build cursor) evaluates the predicates against a
// RowEvalContext carrying the DIRECT per-leg bindings (the cursor's pre-built
// twoLegBinder — predicates need no windows at build; legs are
// PRE-adapted, one small binder per pair, never a map or a re-adaptation): a
// lazy leg reference QOV(leg).col resolves leg-relative against the adapted leg
// row (correct even for the second leg), a BAKED one by its baked ordinal, an
// outer correlation via the binder's base, and a qualified read ("A.ID", a flat
// FieldValue) resolves via the merged row's leg windows.
// legs is the CONCRETE *twoLegBinder (not the CorrelationBinder interface) so
// the cursor's `var pair *twoLegBinder` typed-nil passes as a genuine nil —
// an interface-typed param would make a typed-nil non-nil and route the
// non-build path through a nil binder.
// spansFromMergedLegs derives the per-leg windows from a merged row's own leg
// metadata (RecordType.Legs), for the non-build join-predicate path: each leg's
// alias maps to its window [Start, Start+Width) with the sub-slice of fields as
// its leg type, so a QOV(leg).col reference resolves leg-locally over the flat
// merged row.
func spansFromMergedLegs(pos *PositionalRow) []legSpan {
	if pos == nil || pos.Type == nil {
		return nil
	}
	spans := make([]legSpan, 0, len(pos.Type.Legs))
	for _, leg := range pos.Type.Legs {
		end := leg.Start + leg.Width
		if leg.Start < 0 || end > len(pos.Type.Fields) {
			continue
		}
		legFields := make([]values.Field, leg.Width)
		for i := 0; i < leg.Width; i++ {
			f := pos.Type.Fields[leg.Start+i]
			f.Ordinal = i
			legFields[i] = f
		}
		spans = append(spans, legSpan{
			Alias:   values.NamedCorrelationIdentifier(leg.Name),
			LegType: &values.RecordType{Fields: legFields},
			Offset:  leg.Start,
			Width:   leg.Width,
		})
	}
	return spans
}

func passesJoinPredicatesLegs(combined QueryResult, preds []predicates.QueryPredicate, evalCtx *EvaluationContext, legs *twoLegBinder) (bool, error) {
	if len(preds) == 0 {
		return true, nil
	}
	// Resolve join/EXISTS predicates against the merged row's ordinal
	// Positional. A build-active cursor supplies a
	// per-leg binder (legs); a non-build merge derives leg windows from the merged
	// row's own leg metadata (Type.Legs) so a QOV(leg).col — e.g. QOV(B).ID —
	// resolves to leg B's window instead of first-matching leg A's bare "ID" on the
	// flat merged row. An outer correlation / param / scalar subquery resolves
	// through evalCtx.
	var rowCtx any
	switch {
	case legs != nil:
		rc := &values.RowEvalContext{Positional: combined.Positional, Correlations: legs}
		if evalCtx != nil {
			rc.Binder = evalCtx
			rc.ScalarSubqueries = evalCtx.scalarSubqueries
		}
		rowCtx = rc
	case combined.Positional != nil && combined.Positional.Type != nil && len(combined.Positional.Type.Legs) > 0:
		rowCtx = legWindowRowContext(combined.Positional, evalCtx, spansFromMergedLegs(combined.Positional))
	default:
		rc := &values.RowEvalContext{Positional: combined.Positional}
		if evalCtx != nil {
			rc.Correlations = evalCtx
			rc.Binder = evalCtx
			rc.ScalarSubqueries = evalCtx.scalarSubqueries
		}
		rowCtx = rc
	}
	for _, pred := range preds {
		res, err := pred.Eval(rowCtx)
		if err != nil {
			return false, err
		}
		if res != predicates.TriTrue {
			return false, nil
		}
	}
	return true, nil
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
	// buildAgg deserializes an aggregate continuation (if resuming from a
	// previous transaction), extracts the inner continuation for the leaf
	// cursor plus the single in-progress group's partial state, and builds the
	// aggregate cursor. Mirrors Java's
	// RecordQueryStreamingAggregationPlan.executePlan().
	buildAgg := func(aggCont []byte) (recordlayer.RecordCursor[QueryResult], error) {
		var innerContinuation []byte
		var priorGroupKey string
		var priorState *groupState

		if aggCont != nil {
			ic, gk, gs, decErr := decodeAggregateContinuation(aggCont, len(aggregates))
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

		cursor := newAggregateCursor(innerCursor, groupingKeys, aggregates, inner, evalCtx)
		if len(innerContinuation) > 0 {
			// A resumed cursor's initial resume point is the decoded INNER
			// position: if its first row group-breaks against restored partial
			// state (the prior page's time/scan limit landed exactly on a group
			// boundary), the emitted group's continuation is (this inner
			// position, NO state) — resume reads the next group fresh. Java
			// instead re-wraps the WHOLE parsed AggregateCursorContinuation as
			// the initial previousContinuationInGroup (constructor param), whose
			// re-emit NESTS the aggregate proto inside itself and would hand
			// aggregate bytes to the leaf plan on a second resume — Go encodes
			// the intended flat position (Java's own movement-table comment).
			cursor.previousContinuationInGroup = recordlayer.NewBytesContinuation(innerContinuation)
		}
		if priorState != nil {
			cursor.withPartialState(priorGroupKey, priorState.keyVals, priorState)
		}
		return cursor, nil
	}

	if len(groupingKeys) > 0 {
		cursor, err := buildAgg(continuation)
		if err != nil {
			return nil, err
		}
		return applySkipLimit(cursor, props.Skip, props.ReturnedRowLimit), nil
	}

	// Scalar aggregation (no grouping keys): Java plans it as
	// DefaultOnEmpty(StreamingAggregation) — RecordQueryStreamingAggregationPlan
	// REFUSES an inline default (fromProto throws on isCreateDefaultOnEmpty) and
	// the COUNT(*)-on-empty default row rides RecordCursor.orElse in
	// RecordQueryDefaultOnEmptyPlan.executePlan. Mirror that structure: the
	// OrElse state machine (UNDECIDED/USE_INNER/USE_OTHER) is what makes BOTH
	// emitted-row shapes resumable — a resume from the aggregate row's
	// continuation stays USE_INNER (the exhausted aggregate reports
	// SourceExhausted, never fabricating a duplicate default), and a resume from
	// the default row's continuation stays USE_OTHER (the single-row list cursor
	// is past its row). The child clears skip/limit; skip/limit apply to the
	// whole OrElse — Java's clearSkipAndLimit-on-child + skipThenLimit split.
	primaryFactory := func(cont []byte) recordlayer.RecordCursor[QueryResult] {
		cursor, err := buildAgg(cont)
		if err != nil {
			return &errResultCursor{err: err}
		}
		return cursor
	}
	alternativeFactory := func(cont []byte) recordlayer.RecordCursor[QueryResult] {
		return recordlayer.FromListWithContinuation([]QueryResult{emptyScalarAggregateRow(aggregates)}, cont)
	}
	orElse := recordlayer.OrElseWithContinuation(primaryFactory, alternativeFactory, continuation)
	return applySkipLimit(orElse, props.Skip, props.ReturnedRowLimit), nil
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

// aggKeyName delegates to the single naming authority
// (expressions.AggregateKeyColumnName): the executor's emitted slot name and the
// translator's baked ordinal must derive from ONE rule or they drift.
func aggKeyName(k values.Value) string { return expressions.AggregateKeyColumnName(k) }

// streamingAggOutputNames returns the OUTPUT column names a streaming-aggregate
// plan's rows are keyed by — the single naming authority's
// [groupingKeys..., aggregates...], alias-preferring. Exactly one name per output
// column, matching the keys aggregateCursor.finalizeGroup writes and the schema
// the translator bakes ordinals against. Used by planColumnNamesWithMD so the
// UNION position-remap (remapUnionColumnsByPosition) can normalize a
// mismatched-alias aggregate branch to the first branch's names (RFC-078).
func streamingAggOutputNames(p *plans.RecordQueryStreamingAggregationPlan) []string {
	names := expressions.GroupByOutputColumnNames(p.GetGroupingKeys(), p.GetAggregates())
	if len(names) == 0 {
		return nil
	}
	return names
}

func isNumeric(v any) bool {
	switch v.(type) {
	case int64, int32, int, float64, float32:
		return true
	}
	return false
}

// aggResultName derives the canonical group-result slot key for an aggregate,
// delegating to the single naming authority (expressions.AggregateResultColumnName)
// so the executor's emitted slot name and the translator's baked ordinal derive
// from ONE rule. It ToUppers the rendered name, which FOLDS two aggregates that
// differ only in a case-sensitive token (e.g. a string literal: `COUNT(CASE WHEN
// s='x' …)` vs `…'X'…`) into ONE slot — finalizeGroup then writes both under the
// same key. Currently a LATENT silent-wrong (grouped CASE aggregation does not
// compute in this engine), not a live one; part of the read-surface uppercasing
// family booked in TODO.md.
func aggResultName(agg expressions.AggregateSpec) string {
	return expressions.AggregateResultColumnName(agg)
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
	defer innerCursor.Close()

	// Pre-materialize the full target set BEFORE deleting anything. A resource-limit
	// cut-off (CollectAllBounded → errIfBufferTruncated → 54F01) must abort the DELETE
	// with ZERO records removed — never leave a partially-applied DELETE staged in an
	// explicit transaction that a later commit would persist (RFC-106a). DML runs
	// in one transaction, so the target set is bounded by the tx; the materialization
	// cap is the memory backstop.
	targets, _, err := CollectAllBounded(ctx, innerCursor, props.State, props.GetMaterializationLimit(), "DELETE target set")
	if err != nil {
		return nil, err
	}

	// Re-check the statement deadline AFTER collection and BEFORE any mutation: if the
	// deadline already passed (collection itself is ctx-gated, but the window after it
	// is not), abort with ZERO records changed (RFC-106a). The mutation loop then
	// runs to completion uninterrupted — checking ctx mid-loop would reintroduce the
	// partial-mutation hazard pre-materialization exists to prevent; the loop only
	// stages local writes over a tx-bounded target set.
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	// RFC-130: the DML results echo is NOT separately byte-charged.
	// The mutation's memory is bounded by its pre-materialized + charged target
	// set (CollectAllBounded above). Charging the echo here would (a) for DELETE
	// re-count the same already-charged target rows, and (b) fire AFTER
	// store.DeleteRecord/SaveRecord has staged a write — and runInTx does NOT roll
	// back on a statement error, so a 54F01 mid-loop could persist a PARTIAL
	// mutation. The no-partial-mutation guarantee (all accounting before the
	// mutation loop) outranks a ~2x precision gain on the echo.
	var results []QueryResult
	for _, qr := range targets {
		if qr.PrimaryKey == nil {
			continue
		}
		var deleted bool
		var err error
		if props.DryRun {
			// DRY RUN: validate + preview the delete without staging a write
			// (Java RecordQueryDeletePlan + dryRunDeleteRecordAsync). The
			// `if deleted` echo filter below is preserved (Java's
			// .filter(isDeleted -> isDeleted)) so only would-be-deleted PKs echo.
			deleted, err = store.DryRunDeleteRecord(qr.PrimaryKey)
		} else {
			deleted, err = store.DeleteRecord(qr.PrimaryKey)
		}
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
	defer innerCursor.Close()

	// Materialize the inner rows BEFORE writing any record so that
	// INSERT … SELECT reading the target table doesn't re-scan its own
	// freshly-inserted rows (the Halloween problem). Bounded by the
	// materialization limit, the same guard the other buffering operators
	// use. (Note: a single INSERT that paginates across transactions can
	// still re-read across page boundaries — that extreme case is a known
	// limitation, RFC-035.)
	innerRows, _, err := CollectAllBounded(ctx, innerCursor, props.State, props.GetMaterializationLimit(), "INSERT source")
	if err != nil {
		return nil, err
	}

	// Re-check the statement deadline after collection, before any write — abort
	// with ZERO records inserted if already expired (RFC-106a; see executeDelete).
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	// Resolved lazily on the first computed-row datum.
	var targetDesc protoreflect.MessageDescriptor

	// Phase 1: build every record to insert and charge its ACTUAL size against the
	// budget — BEFORE any write. Charging the built record (not the source row) accounts
	// INSERT … VALUES with a large literal / a growing projection for its true bytes;
	// all charging precedes phase 2's saves, so a budget
	// breach — or any build error — returns with zero SaveRecord calls (no partial
	// INSERT). The built messages ARE the echo content, so no extra residency.
	// proto.Size is gated on HasMemLimit (zero-overhead when off).
	built := make([]proto.Message, 0, len(innerRows))
	for _, qr := range innerRows {
		// INSERT always coerces the inner result to the target type (Java's
		// InsertExpression computation value), so build from the row datum
		// — INSERT … VALUES (Explode of literal RecordConstructors) and
		// INSERT … SELECT (projection aliased to the target columns) both
		// produce a datum keyed by the target column names. A datum-less
		// stored record (rare) is saved as-is.
		var msg proto.Message
		if datum := positionalToMap(qr.Positional); datum != nil {
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
		} else {
			if qr.Record == nil || qr.Record.Record == nil {
				continue
			}
			msg = qr.Record.Record
		}

		if props.State.HasMemLimit() {
			// Match the stored-row estimator: proto wire size PLUS the packed PK tuple
			// the echo's FDBStoredRecord holds separately. The PK is not
			// assigned until SaveRecord, so derive it from the built record via the target
			// type's primary-key expression (best-effort: a derivation error charges the
			// record size alone — still a conservative ceiling for the dominant payload).
			pkBytes := int64(0)
			if rt := store.GetMetaData().GetRecordType(p.GetTargetRecordType()); rt != nil && rt.PrimaryKey != nil {
				if kt, kerr := rt.PrimaryKey.Evaluate(nil, msg); kerr == nil && len(kt) > 0 {
					pk := make(tuple.Tuple, len(kt[0]))
					for i, e := range kt[0] {
						pk[i] = e
					}
					pkBytes = int64(len(pk.Pack()))
				}
			}
			if cerr := props.State.ChargeMemory(int64(proto.Size(msg)) + pkBytes); cerr != nil {
				return nil, cerr
			}
		}
		built = append(built, msg)
	}

	// Phase 2: save the already-charged records (no further charging — the budget is
	// settled before the first write).
	results := make([]QueryResult, 0, len(built))
	for _, msg := range built {
		var stored *recordlayer.FDBStoredRecord[proto.Message]
		var serr error
		if props.DryRun {
			// DRY RUN: validate (incl. the existence check → 23505 on an existing
			// PK, parity with the real path) and preview the insert without
			// staging a write; echo from the returned would-be-stored record
			// (Java RecordQueryInsertPlan + dryRunSaveRecordAsync), not a real save.
			stored, serr = store.DryRunSaveRecord(msg, recordlayer.RecordExistenceCheckErrorIfExists)
		} else {
			stored, serr = store.SaveRecordWithOptions(msg, recordlayer.RecordExistenceCheckErrorIfExists)
		}
		if serr != nil {
			return nil, fmt.Errorf("executor: inserting record: %w", serr)
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
	defer innerCursor.Close()

	transforms := p.GetTransforms()

	// Pre-materialize the full target set BEFORE applying any update — a resource-limit
	// cut-off must abort with ZERO records changed, never a partially-applied UPDATE
	// staged in an explicit transaction (RFC-106a; see executeDelete).
	targets, _, err := CollectAllBounded(ctx, innerCursor, props.State, props.GetMaterializationLimit(), "UPDATE target set")
	if err != nil {
		return nil, err
	}

	// Re-check the statement deadline after collection, before any mutation — abort
	// with ZERO records changed if already expired (RFC-106a; see executeDelete).
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	// Phase 1: build every updated record and charge its ACTUAL post-transform size
	// against the budget — BEFORE any write is staged. Charging the built record (not
	// the source row) accounts a growing UPDATE (small row → large value) for its true
	// bytes; doing all of it before phase 2's saves means a
	// budget breach — or any build/transform error — returns with zero SaveRecord calls
	// (no partial mutation). The built messages ARE the echo content, so no extra
	// residency. proto.Size is gated on HasMemLimit (zero-overhead when off).
	built := make([]proto.Message, 0, len(targets))
	for _, qr := range targets {
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
			// The SET expr resolves each referenced column by ordinal against
			// the base record's PositionalRow (an unset optional reads as a nil slot →
			// SQL NULL); RowContextPositional also binds any params / scalar subqueries.
			var rowCtx any
			if qr.Positional != nil {
				rowCtx = evalCtx.RowContextPositional(qr.Positional)
			}
			newVal, err := t.NewValue.Evaluate(rowCtx)
			if err != nil {
				return nil, err
			}
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

		if props.State.HasMemLimit() {
			// Match the stored-row estimator (estimateQueryResultBytes): proto wire size
			// PLUS the packed PK tuple, which the echo's FDBStoredRecord holds separately.
			// An UPDATE does not change the PK, so the target's PK is the echo's PK.
			if err := props.State.ChargeMemory(int64(proto.Size(msg)) + int64(len(qr.Record.PrimaryKey.Pack()))); err != nil {
				return nil, err
			}
		}
		built = append(built, msg)
	}

	// Phase 2: save the already-charged records (no further charging — the budget is
	// settled before the first write).
	results := make([]QueryResult, 0, len(built))
	for _, msg := range built {
		var stored *recordlayer.FDBStoredRecord[proto.Message]
		var err error
		if props.DryRun {
			// DRY RUN: validate + preview the update without staging a write; echo
			// from the returned would-be-stored record (Java RecordQueryUpdatePlan
			// + dryRunSaveRecordAsync), not a real save.
			stored, err = store.DryRunSaveRecord(msg, recordlayer.RecordExistenceCheckErrorIfNotExistsOrTypeChanged)
		} else {
			stored, err = store.SaveRecordWithOptions(msg, recordlayer.RecordExistenceCheckErrorIfNotExistsOrTypeChanged)
		}
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
		// INT/LONG→FLOAT are promotable in Java's lattice; widen rather than
		// falling through to the 22000 reject (e.g. SUM(BIGINT) into a FLOAT
		// column). Matches ConvertToProtoValue's VALUES path. No range check,
		// deliberately: Java's PromoteValue.LONG_TO_FLOAT is the plain
		// widening cast Float.valueOf((Long)in) — precision-lossy above 2^24,
		// never ±Inf (MaxInt64 ≈ 9.2e18 < MaxFloat32 ≈ 3.4e38, so overflow is
		// unreachable from an integer; the float64 arm above range-checks
		// because float64 CAN exceed float32 range). Go's float32(n) is the
		// identical semantics.
		case int64:
			return protoreflect.ValueOfFloat32(float32(n)), nil
		case int:
			return protoreflect.ValueOfFloat32(float32(n)), nil
		}
	case protoreflect.DoubleKind:
		switch n := v.(type) {
		case float64:
			return protoreflect.ValueOfFloat64(n), nil
		// INT/LONG→DOUBLE are promotable; a SUM(BIGINT)/COUNT into a DOUBLE
		// column must widen (this path previously fell through and errored).
		case int64:
			return protoreflect.ValueOfFloat64(float64(n)), nil
		case int:
			return protoreflect.ValueOfFloat64(float64(n)), nil
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
	case protoreflect.MessageKind:
		// A UUID column is the tuple_fields.UUID message. UPDATE SET uuid_col =
		// … and INSERT … SELECT flow the value through here (INSERT … VALUES uses
		// functions.ConvertToProtoValue instead). Accept the neutral [16]byte
		// (SET v = v, an index/record-sourced value) and the canonical string
		// (SET v = '<uuid>'), building the same msb/lsb message Java writes —
		// otherwise a valid UUID assignment fell through to the 22000 reject.
		if msg := fd.Message(); msg != nil && string(msg.FullName()) == uuidProtoMessageName {
			switch u := v.(type) {
			case [16]byte:
				return uuidBytesToProtoMessage(fd, u)
			case uuid.UUID:
				return uuidBytesToProtoMessage(fd, u)
			case string:
				parsed, perr := uuid.Parse(u)
				if perr != nil {
					return protoreflect.Value{}, api.NewErrorf(api.ErrCodeCannotConvertType,
						"Invalid UUID value for the UUID type %s", u)
				}
				return uuidBytesToProtoMessage(fd, parsed)
			}
		}
	}
	// All promotable conversions are handled above, so anything reaching here is
	// a genuinely incompatible assignment (e.g. a float64/DOUBLE into an integer
	// column — DOUBLE→LONG has no edge in Java's promotion lattice). Emit the
	// verbatim 22000 SemanticException, matching Java's PromoteValue rejection and
	// the sibling ConvertToProtoValue fallthrough — not a generic Go error.
	return protoreflect.Value{}, api.NewErrorf(api.ErrCodeCannotConvertType,
		"A value cannot be assigned to a variable because the type of the value does not match the type of the variable and cannot be promoted to the type of the variable.")
}

// uuidBytesToProtoMessage builds a tuple_fields.UUID proto message (msb/lsb,
// big-endian) from a 16-byte UUID — the write-side inverse of uuidMessageToBytes
// and the mirror of functions.uuidStringToProtoMessage, so a UUID written via
// UPDATE / INSERT…SELECT is byte-identical to one written via INSERT … VALUES.
func uuidBytesToProtoMessage(fd protoreflect.FieldDescriptor, b [16]byte) (protoreflect.Value, error) {
	msgDesc := fd.Message()
	mostFD := msgDesc.Fields().ByName("most_significant_bits")
	leastFD := msgDesc.Fields().ByName("least_significant_bits")
	if mostFD == nil || leastFD == nil {
		return protoreflect.Value{}, api.NewErrorf(api.ErrCodeInternalError,
			"UUID message descriptor missing most/least_significant_bits fields")
	}
	dyn := dynamicpb.NewMessage(msgDesc)
	dyn.Set(mostFD, protoreflect.ValueOfInt64(int64(binary.BigEndian.Uint64(b[0:8]))))   //nolint:gosec
	dyn.Set(leastFD, protoreflect.ValueOfInt64(int64(binary.BigEndian.Uint64(b[8:16])))) //nolint:gosec
	return protoreflect.ValueOfMessage(dyn), nil
}

func executeTempTableScan(
	p *plans.RecordQueryTempTableScanPlan,
	evalCtx *EvaluationContext,
	props recordlayer.ExecuteProperties,
) (recordlayer.RecordCursor[QueryResult], error) {
	tt := evalCtx.GetOrCreateTempTable(p.GetTempTableAlias(), props.State)
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

	tt := evalCtx.GetOrCreateTempTable(p.GetTempTableAlias(), props.State)

	// RFC-130: charge the temp-table working set; tt.Add returns the budget
	// error, propagated via MapErrCursor (MapCursor cannot return an error).
	// The table outlives this cursor — it is the recursion's shared working
	// set (BFS ping-pong locals, or the DFS accumulator GetOrCreate mints
	// into the shared bindings map) — so its charges are released by the
	// recursion's teardown, never here.
	mapped := recordlayer.MapErrCursor(innerCursor, func(qr QueryResult) (QueryResult, error) {
		if err := tt.Add(qr); err != nil {
			return QueryResult{}, err
		}
		return qr, nil
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
	result, err := sv.Evaluate(evalCtx)
	if err != nil {
		return nil, err
	}
	if result == nil {
		return applySkipLimit(recordlayer.Empty[QueryResult](), props.Skip, props.ReturnedRowLimit), nil
	}
	list, ok := result.([]any)
	if !ok {
		return applySkipLimit(
			recordlayer.FromList([]QueryResult{{Positional: explodeElementRow(result, arrayElementType(sv))}}),
			props.Skip, props.ReturnedRowLimit,
		), nil
	}
	items := make([]QueryResult, len(list))
	for i, elem := range list {
		items[i] = QueryResult{Positional: explodeElementRow(elem, arrayElementType(sv))}
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
	result, err := cv.Evaluate(evalCtx)
	if err != nil {
		return nil, err
	}
	if result == nil {
		return applySkipLimit(recordlayer.Empty[QueryResult](), props.Skip, props.ReturnedRowLimit), nil
	}
	// The WITH-ORDINALITY box's ordinal output type (`[_0,_1]`), recovered
	// once from the plan's result type so every emitted row carries a
	// PositionalRow of that schema. Nil unless with-ordinality.
	var ordType *values.RecordType
	if p.IsWithOrdinality() {
		if rt, ok := p.GetResultType().(*values.RecordType); ok {
			ordType = rt
		}
	}
	list, ok := result.([]any)
	if !ok {
		// A non-list scalar yields a single row. With ordinality it gets
		// ordinal 1 (the SQL standard's 1-based position of the sole element).
		if p.IsWithOrdinality() {
			return applySkipLimit(
				recordlayer.FromList([]QueryResult{explodeOrdinalityResult(ordType, result, 1)}),
				props.Skip, props.ReturnedRowLimit,
			), nil
		}
		return applySkipLimit(
			recordlayer.FromList([]QueryResult{{Positional: explodeElementRow(result, p.GetElementType())}}),
			props.Skip, props.ReturnedRowLimit,
		), nil
	}
	items := make([]QueryResult, len(list))
	for i, elem := range list {
		if p.IsWithOrdinality() {
			// WITH ORDINALITY: each element becomes a 2-field anonymous record
			// {_0: element, _1: i+1}. The ordinal is the element's 1-based
			// position in THIS array (the cursor re-runs per outer row, so the
			// counter naturally resets per outer binding — Java's
			// IntStream.rangeClosed(1, list.size())). Mirrors
			// RecordQueryExplodePlan.executePlan's DynamicMessage(field0,field1).
			items[i] = explodeOrdinalityResult(ordType, elem, i+1)
			continue
		}
		items[i] = QueryResult{Positional: explodeElementRow(elem, p.GetElementType())}
	}
	return applySkipLimit(recordlayer.FromList(items), props.Skip, props.ReturnedRowLimit), nil
}

// explodeOrdinalityResult builds a WITH-ORDINALITY box output row: a
// PositionalRow of the box's `[_0,_1]` schema (element at slot 0, 1-based
// ordinal at slot 1), so a downstream reference over the box (a pushed-down
// WHERE predicate on the AS element / AT ordinal, evaluated by the
// PredicatesFilter over the Explode) resolves by ORDINAL.
func explodeOrdinalityResult(ordType *values.RecordType, element any, ordinal int) QueryResult {
	if ordType == nil {
		ordType = positionalTypeFromNames([]string{"_0", "_1"})
	}
	pos := NewPositionalRow(ordType)
	pos.Set(0, element)
	pos.Set(1, int64(ordinal))
	return QueryResult{Positional: pos}
}

// scalarPositionalRow wraps a BARE scalar UNNEST element (RFC-142: `t.arr AS x`
// flows a raw int64, not a row) as a 1-slot PositionalRow named `_0` so it flows in
// the ordinal model. The FlatMap binds QOV(alias) to it and concatenates the leg
// into the merged row; a downstream read of the element resolves against slot 0.
func scalarPositionalRow(v any) *PositionalRow {
	return &PositionalRow{Type: positionalTypeFromNames([]string{"_0"}), Slots: []any{v}}
}

// explodeElementRow wraps one Explode/Stream element as a PositionalRow: a
// ROW-shaped element (a map[string]any — e.g. a constant array of records, a
// VALUES list) becomes a multi-column row laid out in the element type's
// DECLARED field order — the order the plan-time ordinals were baked against.
// A baked ordinal reads by POSITION, so the row MUST match declared order, not
// an alphabetical (or map-iteration) order, or a reference baked at ordinal i
// silently reads a different field. Any other (scalar) element wraps in the
// 1-slot `_0` bare-scalar shape. elemType is the array element type (declared
// RecordType); nil/non-record falls back to sorted names (best-effort — a baked
// ordinal against an unknown declared order cannot be made sound here).
func explodeElementRow(elem any, elemType values.Type) *PositionalRow {
	m, ok := elem.(map[string]any)
	if !ok {
		return scalarPositionalRow(elem)
	}
	if rt, isRT := elemType.(*values.RecordType); isRT && len(rt.Fields) > 0 {
		slots := make([]any, len(rt.Fields))
		names := make([]string, len(rt.Fields))
		for i, f := range rt.Fields {
			names[i] = f.Name
			slots[i] = mapFieldFold(m, f.Name)
		}
		return &PositionalRow{Type: positionalTypeFromNames(names), Slots: slots}
	}
	names := make([]string, 0, len(m))
	for k := range m {
		names = append(names, k)
	}
	sort.Strings(names)
	slots := make([]any, len(names))
	for i, n := range names {
		slots[i] = m[n]
	}
	return &PositionalRow{Type: positionalTypeFromNames(names), Slots: slots}
}

// mapFieldFold looks up a record field's value in a name-keyed element map,
// case-insensitively (the element map may be keyed by the raw or upper-cased
// field name); an absent field is a NULL slot.
func mapFieldFold(m map[string]any, field string) any {
	if v, ok := m[field]; ok {
		return v
	}
	for k, v := range m {
		if strings.EqualFold(k, field) {
			return v
		}
	}
	return nil
}

// arrayElementType returns the declared element type of an array-typed value —
// the order plan-time ordinals were baked against — or nil when unknown.
func arrayElementType(v values.Value) values.Type {
	if v == nil {
		return nil
	}
	if at, ok := v.Type().(*values.ArrayType); ok {
		return at.ElementType
	}
	return nil
}

// resultFromValue wraps a single Value's evaluation (against a nil row — a
// constant default) into a Positional QueryResult: a RecordConstructor evaluates
// field-by-field into dense ordinal slots (a duplicate output name keeps both
// slots), any other (scalar) value wraps in a 1-slot `_0` row. Used by the
// default-on-empty producers (FirstOrDefault / DefaultOnEmpty).
func resultFromValue(v values.Value) (QueryResult, error) {
	if rc, ok := v.(*values.RecordConstructorValue); ok {
		names := make([]string, len(rc.Fields))
		slots := make([]any, len(rc.Fields))
		for i, f := range rc.Fields {
			names[i] = f.Name
			fv, err := f.Value.Evaluate(nil)
			if err != nil {
				return QueryResult{}, err
			}
			slots[i] = fv
		}
		return QueryResult{Positional: &PositionalRow{Type: positionalTypeFromNames(names), Slots: slots}}, nil
	}
	val, err := v.Evaluate(nil)
	if err != nil {
		return QueryResult{}, err
	}
	return QueryResult{Positional: scalarPositionalRow(val)}, nil
}

// isBareScalarRow reports whether pos is the 1-slot `_0` wrapper scalarPositionalRow
// produces — a bare-scalar UNNEST element (`t.arr AS x` flowing a raw int64). The
// filter/map dispatch uses it to bind the UNWRAPPED scalar under the quantifier
// alias, since a bare QOV(alias) reference over such a row must resolve to the
// scalar, not the 1-slot row itself. A WITH-ORDINALITY explode's `{_0,_1}` row is
// NOT bare (2 slots) and reads its fields by ordinal FieldValue instead.
func isBareScalarRow(pos *PositionalRow) bool {
	return pos != nil && pos.Type != nil && len(pos.Type.Fields) == 1 &&
		pos.Type.Fields[0].Name == values.OrdinalFieldName(0)
}

func executeValues(p *plans.RecordQueryValuesPlan, evalCtx *EvaluationContext) (recordlayer.RecordCursor[QueryResult], error) {
	cols := p.GetColumns()
	names := make([]string, len(cols))
	slots := make([]any, len(cols))
	for i, col := range cols {
		v, err := col.Evaluate(evalCtx)
		if err != nil {
			return nil, err
		}
		names[i] = col.Name()
		slots[i] = v
	}
	pos := &PositionalRow{Type: positionalTypeFromNames(names), Slots: slots}
	return recordlayer.FromList([]QueryResult{{Positional: pos}}), nil
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

	scanTable := NewTempTableWithState(props.State)
	insertTable := NewTempTableWithState(props.State)
	// Live-bytes model: every byte the recursion charges (ping-pong temp
	// tables via TempTableInsert, the UNION-DISTINCT seen-set) stands in for
	// the accumulated result set and is released in one place — at the final
	// cursor's teardown on success, or right here on an error return.
	var seenCharge func() int64 = func() int64 { return 0 }
	releaseWorkingSet := func() {
		scanTable.ReleaseCharges()
		insertTable.ReleaseCharges()
		props.State.ReleaseMemory(seenCharge())
	}
	handedOff := false
	defer func() {
		if !handedOff {
			releaseWorkingSet()
		}
	}()

	levelCtx := evalCtx.WithBinding(scanAlias, scanTable)
	levelCtx = levelCtx.WithBinding(insertAlias, insertTable)

	initialCursor, err := ExecutePlan(ctx, p.GetInitialState(), store, levelCtx, continuation, props.ClearSkipAndLimit())
	if err != nil {
		return nil, fmt.Errorf("executor: recursive level union initial: %w", err)
	}

	var allResults []QueryResult
	items, err := collectAllRowCapped(ctx, initialCursor, props.GetMaterializationLimit(), "recursive CTE initial state")
	initialCursor.Close()
	if err != nil {
		return nil, fmt.Errorf("executor: recursive level union initial collect: %w", err)
	}

	// UNION DISTINCT: track seen rows via a string key to detect
	// and filter duplicates (cycle detection on cyclic graphs).
	// Extract canonical column names from the seed plan so the dedup
	// key only considers CTE-relevant columns (ignoring extra join
	// columns the recursive branch may carry in its rows).
	distinct := p.IsDistinct()
	// RFC-130: the UNION-DISTINCT seen-set is a cross-level cardinality-growing
	// buffer (one key per distinct row, held across all levels) — charge each
	// NEW key via boundedSet.
	var seen *boundedSet[string]
	var keyer *cteDedupKeyer
	if distinct {
		seen = newBoundedSet[string](props.State)
		seenCharge = seen.Charged
		// Dedup on the CTE's OUTPUT columns. Prefer the seed plan's projection
		// OUTPUT schema: after the temp table is keyed under OUTPUT names, the
		// seed row can carry INERT extra columns (e.g. the source column a rename
		// projects from — {SRC, N} for `reach(n)` seeded by `SELECT src`). Those
		// inert columns are absent from the recursive leg's rows, so keying ALL
		// seed columns would wrongly treat a recursive row and a seed row with the
		// same OUTPUT value as distinct (breaking cycle detection). The projection
		// schema restricts the dedup to the real output columns. Fall back to
		// the first row's layout when the seed has no projection (e.g. SELECT *).
		canonicalCols := recursiveUnionOutputColumns(p.GetInitialState())
		if len(canonicalCols) == 0 && len(items) > 0 && items[0].Positional != nil {
			// Positional column order is already deterministic (ordinal order),
			// so no sort is needed.
			canonicalCols = items[0].Positional.TypeNames()
		}
		keyer = newCTEDedupKeyer(canonicalCols)
		var deduped []QueryResult
		for _, it := range items {
			k, kerr := keyer.key(it)
			if kerr != nil {
				return nil, kerr
			}
			added, err := seen.Add(k, int64(len(k)))
			if err != nil {
				return nil, err
			}
			if !added {
				continue
			}
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
		items, err := collectAllRowCapped(ctx, recursiveCursor, props.GetMaterializationLimit(), "recursive CTE recursive level")
		recursiveCursor.Close()
		if err != nil {
			return nil, fmt.Errorf("executor: recursive level union recursive collect: %w", err)
		}
		if distinct {
			var newItems []QueryResult
			for _, it := range items {
				k, kerr := keyer.key(it)
				if kerr != nil {
					return nil, kerr
				}
				added, err := seen.Add(k, int64(len(k)))
				if err != nil {
					return nil, err
				}
				if !added {
					continue
				}
				newItems = append(newItems, it)
			}
			items = newItems
			// Also replace insertTable contents with only the new
			// (non-duplicate) rows so the next level's scan sees only
			// genuinely new rows. These rows were already charged when the
			// recursive plan's TempTableInsertPlan added them this level, so
			// ReplaceList swaps without re-charging (RFC-130).
			insertTable.ReplaceList(items)
		}
		allResults = append(allResults, items...)
	}

	handedOff = true
	return newCloseHookCursor(
		applySkipLimit(recordlayer.FromList(allResults), props.Skip, props.ReturnedRowLimit),
		releaseWorkingSet,
	), nil
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

	// RFC-130 (the charge-once fix, extended to the DFS path): the root/child
	// cursors have a TempTableInsertPlan at the top that already charges each row
	// in tt.Add — draining with the byte-charging CollectAllBounded would
	// double-count and trip the budget at ~half its true value (the same defect
	// collectAllRowCapped fixes for executeRecursiveLevelUnion's initial state).
	// Ordinalizing the recursive body flips the cost
	// so this DFS plan wins over the level union for wide-payload recursive CTEs.
	rootRows, err := collectAllRowCapped(ctx, rootCursor, props.GetMaterializationLimit(), "recursive DFS join root")
	rootCursor.Close()
	if err != nil {
		return nil, fmt.Errorf("executor: recursive dfs join root collect: %w", err)
	}

	preorder := p.GetTraversalStrategy() == plans.DfsPreorder
	var results []QueryResult
	// RFC-130 live-bytes model: every DFS row is charged ONCE — at tt.Add,
	// when the root/child TempTableInsertPlan appends it to the shared
	// accumulator table GetOrCreate mints into this context's bindings map.
	// That table's charge stands in for the result set (same rows); the
	// teardown below returns it (and the DISTINCT seen-set) to the budget,
	// so a per-page re-execution does not re-accumulate the recursion.
	var seenCharge func() int64 = func() int64 { return 0 }
	releaseWorkingSet := func() {
		evalCtx.ReleaseAllTempTableCharges()
		props.State.ReleaseMemory(seenCharge())
	}
	handedOff := false
	defer func() {
		if !handedOff {
			releaseWorkingSet()
		}
	}()
	// RFC-130: the DFS dedup seen-set is a cross-traversal cardinality-growing
	// buffer (one key per distinct visited row) — charge each NEW key via
	// boundedSet.
	var seen *boundedSet[string]
	// For UNION DISTINCT, dedup on the CTE's OUTPUT columns. Prefer the root
	// plan's projection OUTPUT schema: after the temp table is keyed under OUTPUT
	// names, the root row can carry INERT extra columns (the source column a rename
	// projects from), absent from the recursive rows — keying ALL root columns
	// would then treat equal-output rows as distinct and break cycle detection.
	// Fall back to the first row's layout when there is no projection (SELECT *).
	var keyer *cteDedupKeyer
	if p.IsDistinct() {
		seen = newBoundedSet[string](props.State)
		seenCharge = seen.Charged
		canonicalCols := recursiveUnionOutputColumns(p.GetRoot())
		if len(canonicalCols) == 0 && len(rootRows) > 0 && rootRows[0].Positional != nil {
			// Ordinal order is already deterministic; no sort needed.
			canonicalCols = rootRows[0].Positional.TypeNames()
		}
		keyer = newCTEDedupKeyer(canonicalCols)
	}

	const maxRecursionDepth = 256

	for _, root := range rootRows {
		if seen != nil {
			k, kerr := keyer.key(root)
			if kerr != nil {
				return nil, kerr
			}
			added, err := seen.Add(k, int64(len(k)))
			if err != nil {
				return nil, err
			}
			if !added {
				continue
			}
		}
		if err := dfsVisit(ctx, root, p, store, evalCtx, preorder, props, &results, 0, maxRecursionDepth, seen, keyer); err != nil {
			return nil, err
		}
	}

	handedOff = true
	return newCloseHookCursor(
		applySkipLimit(recordlayer.FromList(results), props.Skip, props.ReturnedRowLimit),
		releaseWorkingSet,
	), nil
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
	seen *boundedSet[string],
	keyer *cteDedupKeyer,
) error {
	if depth >= maxDepth {
		return &RecursiveCTEDepthExceededError{MaxDepth: maxDepth}
	}

	if preorder {
		*results = append(*results, node)
	}

	// singleRow is a transient per-visit binding holder for the prior-
	// correlation row — NOT a cardinality-growing buffer (one row, GC'd after
	// the child plan runs), and node is already charged where it was
	// collected. Use a non-charging temp table so it is not double-counted.
	singleRow := NewTempTable()
	if err := singleRow.Add(node); err != nil {
		return err
	}
	childCtx := evalCtx.WithBinding(p.GetPriorCorrelation(), singleRow)
	childCursor, err := ExecutePlan(ctx, p.GetChild(), store, childCtx, nil, props.ClearSkipAndLimit())
	if err != nil {
		return fmt.Errorf("recursive DFS child plan: %w", err)
	}

	// RFC-130: the child's TempTableInsertPlan already charged these rows in
	// tt.Add — use the row-capped (non-byte-charging) drain to avoid the
	// double-count (see the root collect above).
	children, err := collectAllRowCapped(ctx, childCursor, props.GetMaterializationLimit(), "recursive DFS children")
	childCursor.Close()
	if err != nil {
		return fmt.Errorf("recursive DFS collect children: %w", err)
	}

	for _, child := range children {
		if seen != nil {
			k, kerr := keyer.key(child)
			if kerr != nil {
				return kerr
			}
			added, err := seen.Add(k, int64(len(k)))
			if err != nil {
				return err
			}
			if !added {
				continue
			}
		}
		if err := dfsVisit(ctx, child, p, store, evalCtx, preorder, props, results, depth+1, maxDepth, seen, keyer); err != nil {
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
	pred   func(QueryResult) (bool, error)
	closed bool
	// lastNoNext replays the terminal result on a contract-violating re-call
	// (Java FilterCursor's cached no-next result) — never re-pulls the inner.
	lastNoNext *recordlayer.RecordCursorResult[QueryResult]
}

func (c *filterResultCursor) OnNext(ctx context.Context) (result recordlayer.RecordCursorResult[QueryResult], err error) {
	if c.lastNoNext != nil {
		return *c.lastNoNext, nil
	}
	for {
		if err = ctx.Err(); err != nil {
			return recordlayer.RecordCursorResult[QueryResult]{}, err
		}
		result, err = c.inner.OnNext(ctx)
		if err != nil {
			return result, err
		}
		if !result.HasNext() {
			c.lastNoNext = &result
			return result, nil
		}
		keep, perr := c.pred(result.GetValue())
		if perr != nil {
			return recordlayer.RecordCursorResult[QueryResult]{}, perr
		}
		if keep {
			return result, nil
		}
	}
}

func (c *filterResultCursor) Close() error {
	c.closed = true
	return c.inner.Close()
}

func (c *filterResultCursor) IsClosed() bool { return c.closed }

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
			if lerr := errIfBufferTruncated(result); lerr != nil {
				return nil, lerr
			}
			break
		}
		results = append(results, result.GetValue())
	}
	return results, nil
}

// CollectAllBounded drains a cursor into a slice through an accounted
// boundedBuffer (RFC-130): every row is charged against the statement-wide
// memory byte budget (st) AND counted against the row-count materialization
// limit, so a missed accumulation site is impossible — the buffer cannot exist
// without the accountant. st is the always-present statement ExecuteState
// (props.State); a nil/zero-limit st makes the byte charge a no-op while the
// row-count cap still applies. Returns MaterializationLimitExceededError on the
// row cap and MemoryLimitExceededError (→ 54F01) on the byte budget.
// The second return is the total bytes charged against st — the owner of the
// returned rows releases exactly that at teardown (live-bytes model: the
// buffer is rebuilt per page against the statement-wide state, so an
// unreleased charge re-accumulates once per page).
func CollectAllBounded(ctx context.Context, cursor recordlayer.RecordCursor[QueryResult], st *recordlayer.ExecuteState, limit int, opName string) ([]QueryResult, int64, error) {
	buf := newBoundedBuffer[QueryResult](st, limit, opName, estimateQueryResultBytes)
	for {
		if err := ctx.Err(); err != nil {
			return nil, buf.Charged(), err
		}
		result, err := cursor.OnNext(ctx)
		if err != nil {
			return nil, buf.Charged(), err
		}
		if !result.HasNext() {
			if lerr := errIfBufferTruncated(result); lerr != nil {
				return nil, buf.Charged(), lerr
			}
			break
		}
		v := result.GetValue()
		if err := buf.Append(v); err != nil {
			return nil, buf.Charged(), err
		}
	}
	return buf.Items(), buf.Charged(), nil
}

// collectAllRowCapped drains a cursor into a slice enforcing the MaterializationLimit
// ROW cap but charging NO bytes against the statement memory budget — for cursors
// whose rows are already byte-charged upstream. The recursive-CTE initial/recursive
// level cursors have a TempTableInsertPlan at the top, which charges each row in
// tt.Add (monotonic, statement-wide, surviving the per-level Clear); re-charging the
// same shared records here would double-count and trip the budget at ~half its true
// value (RFC-130, code-review #328). Passing a nil ExecuteState makes the boundedBuffer
// skip both the estimate and the charge while keeping the row cap.
func collectAllRowCapped(ctx context.Context, cursor recordlayer.RecordCursor[QueryResult], limit int, opName string) ([]QueryResult, error) {
	rows, _, err := CollectAllBounded(ctx, cursor, nil, limit, opName)
	return rows, err
}

// errIfBufferTruncated returns a 54F01-mapped error when an eager/buffered
// collect's source cursor stopped OUT-OF-BAND — i.e. a scan/byte/time resource
// limit (RFC-106a) cut it off, not true exhaustion or a legitimate
// ReturnedRowLimit. A buffered operator (union/NLJ-inner/INSERT/recursive-CTE,
// scalar subquery, DML drain) materializes its source in one shot and cannot
// paginate a continuation, so an out-of-band stop means the buffer is INCOMPLETE.
// Erroring (→ 54F01) is correct; silently returning the partial buffer would be a
// silent truncation (CLAUDE.md: no silent caps). Mirrors Java's
// RecordCursor.NoNextReason.isOutOfBand() — the streaming operators (sort/group)
// instead capture the partial state in a continuation and paginate, which a
// one-shot buffer cannot.
func errIfBufferTruncated(result recordlayer.RecordCursorResult[QueryResult]) error {
	if result.GetNoNextReason().IsOutOfBand() {
		return &recordlayer.ScanLimitReachedError{Reason: result.GetNoNextReason()}
	}
	return nil
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

// projectionColumnName delegates to the shared naming contract in the values
// package (values.ProjectionColumnName) so the planner/translator side can read
// a projection's output by the exact key the executor writes.
func projectionColumnName(v values.Value) string {
	return values.ProjectionColumnName(v)
}

// sortEvalRow returns the row a sort-key Value expression should be evaluated
// against: the authoritative ordinal positional row (Value.Evaluate then
// resolves by ordinal, loud on a miss).
func sortEvalRow(qr QueryResult) any {
	return qr.Positional
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
	var emitPhase bool
	if continuation != nil {
		// Buffered STRUCT column slots rebuild as their ENCODED representation
		// (concrete-type identity with fresh rows, which the %T-keyed
		// group/dedup paths depend on): a generated message (flag 0) restores
		// via protoregistry.GlobalTypes; a *dynamicpb.Message (flag 1) restores
		// via this metadata resolver — never across representations. A nil
		// store leaves the resolver nil: a buffer with dynamic struct slots
		// then fails the resume loudly rather than leaking a descriptor-less
		// placeholder into the row domain.
		var resolve protoDescriptorResolver
		if store != nil {
			resolve = metadataMessageResolver(store.GetRecordMetaData())
		}
		ic, buf, exhausted, decErr := decodeSortContinuation(continuation, resolve)
		if decErr != nil {
			return nil, fmt.Errorf("invalid sort continuation: %w", decErr)
		}
		innerContinuation = ic
		priorBuf = buf
		emitPhase = exhausted
	}

	innerCursor, err := ExecutePlan(ctx, p.GetInner(), store, evalCtx, innerContinuation, props.ClearSkipAndLimit())
	if err != nil {
		return nil, err
	}

	keys := p.GetSortKeys()
	// When the sort input flows a merged ordinal-join row, evaluate the
	// sort keys through the SAME leg-window context the downstream projection/
	// filter use (legWindowRowContext -> spanAwareRow), so a QUALIFIED key
	// (`p.name`) over a multi-way join resolves to its BURIED leg — the box span's
	// buried-leg windows — instead of the naked merged row's first bare match.
	legSpans, joinWindowsOK := downstreamLegWindows(p.GetInner())
	sortKeyValue := func(qr QueryResult, k plans.SortKey) (any, error) {
		kv := k.ValueExpr
		if kv == nil {
			// Every sort key carries its Value (the planner rules set
			// ValueExpr unconditionally; the ordinal is baked at plan time). A
			// nil ValueExpr is a malformed plan — loud, never a name read.
			return nil, fmt.Errorf("in-memory sort: sort key %q carries no value expression — malformed plan", k.Field)
		}
		if joinWindowsOK && qr.Positional != nil {
			return kv.Evaluate(legWindowRowContext(qr.Positional, evalCtx, legSpans))
		}
		// The authoritative ordinal row on the non-join frontier (loud on a
		// miss via FieldValue.evaluateOrdinal).
		return kv.Evaluate(sortEvalRow(qr))
	}
	sortFn := func(results []QueryResult) error {
		pkDesc := false
		if len(keys) > 0 {
			pkDesc = keys[len(keys)-1].Desc
		}
		var sortErr error
		sort.SliceStable(results, func(i, j int) bool {
			if sortErr != nil {
				return false
			}
			for _, k := range keys {
				var ci, cj any
				var err error
				if ci, err = sortKeyValue(results[i], k); err != nil {
					sortErr = err
					return false
				}
				if cj, err = sortKeyValue(results[j], k); err != nil {
					sortErr = err
					return false
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
				cmp, cmpErr := compareValues(ci, cj)
				if cmpErr != nil {
					if sortErr == nil {
						sortErr = cmpErr
					}
					return false
				}
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
		return sortErr
	}

	cursor := newCustomSortCursor(innerCursor, sortFn, props.State)
	// RFC-180 H4: a RESUMED buffer is memory like any other — the fill loop
	// charges every fresh row against the RFC-130 statement budget
	// (customSortCursor.OnNext), so restored rows must charge identically at
	// injection or a resume smuggles an arbitrarily large buffer past the
	// limit. The cursor tracks its charges and releases them on Close, so a
	// statement-wide ExecuteState reused across pages accounts the buffer's
	// LIVE bytes once, not once per page boundary.
	if len(priorBuf) > 0 {
		if cerr := cursor.chargeResumedBuffer(priorBuf); cerr != nil {
			_ = cursor.Close()
			return nil, cerr
		}
	}
	switch {
	case emitPhase:
		// Emit-phase resume (sortEmitContinuation): the inner was EXHAUSTED when
		// the continuation was taken and priorBuf is the remaining SORTED output —
		// go straight to emit. Re-running the fill loop would re-scan the inner
		// from scratch and duplicate every restored row (the buffer is a slice,
		// not Java MemorySortCursor's key-deduped scratchpad map). An EMPTY
		// remaining buffer (resume taken at the last row) still sets loaded so the
		// cursor reports SourceExhausted instead of re-filling.
		cursor.buf = priorBuf
		cursor.loaded = true
	case len(priorBuf) > 0:
		// Fill-phase resume: the buffer so far, inner resumes from its position;
		// sortFn runs over the COMBINED buffer at exhaustion.
		cursor.buf = priorBuf
	}
	return applySkipLimit(cursor, props.Skip, props.ReturnedRowLimit), nil
}

// recursiveUnionOutputColumns returns the OUTPUT column names of a recursive
// union leg by walking to its outermost projection plan and reading each slot's
// alias (or the projection column name when unaliased), upper-cased. Returns nil
// when no single-child path reaches a projection (e.g. a SELECT * seed), so the
// caller falls back to the first row's layout. Used to restrict UNION DISTINCT
// dedup to the CTE's real output columns (cteDedupKeyer), ignoring inert extra
// columns the temp-table normalization may carry.
func recursiveUnionOutputColumns(p plans.RecordQueryPlan) []string {
	for cur := p; cur != nil; {
		if proj, ok := cur.(*plans.RecordQueryProjectionPlan); ok {
			projs := proj.GetProjections()
			aliases := proj.GetAliases()
			out := make([]string, len(projs))
			for i, pv := range projs {
				if i < len(aliases) && aliases[i] != "" {
					out[i] = strings.ToUpper(aliases[i])
				} else {
					// Upper-case symmetrically with the aliased branch: the dedup
					// keyer binds these names against the UPPER-cased row layouts
					// (exact FieldIndex match), so a mixed-case name here would miss
					// every layout and collapse distinct rows / break cycle detection.
					// projectionColumnName already uppers the non-field
					// path; this makes the field path explicit too.
					out[i] = strings.ToUpper(projectionColumnName(pv))
				}
			}
			return out
		}
		children := cur.GetChildren()
		if len(children) != 1 {
			return nil
		}
		cur = children[0]
	}
	return nil
}

// cteDedupKeyer builds the recursive-CTE UNION-DISTINCT dedup key by reading
// the canonical output columns POSITIONALLY (row.Get(ordinal)). The canonical
// columns are plan-derived (recursiveUnionOutputColumns — the seed projection's
// output layout); each leg's rows flow ONE layout (the leg plan's outermost
// projection output), so the column→ordinal bind is resolved once per row
// LAYOUT — against the row's *RecordType, plan-produced metadata — and cached,
// never re-derived per row. A canonical column absent from a layout keys as
// NULL (dimension-preserving with the seed leg).
// Go extension: Java's recursive union has
// no DISTINCT arm (RecordQueryRecursiveLevelUnionPlan is UNION ALL only).
type cteDedupKeyer struct {
	cols []string // canonical output columns, UPPER-cased
	ords map[*values.RecordType][]int
}

func newCTEDedupKeyer(cols []string) *cteDedupKeyer {
	upper := make([]string, len(cols))
	for i, c := range cols {
		upper[i] = strings.ToUpper(c)
	}
	return &cteDedupKeyer{cols: upper, ords: make(map[*values.RecordType][]int)}
}

// layoutOrdinals binds the canonical columns to rt's slot ordinals (-1 =
// absent), memoized per layout. The bind rule is RecordType.FieldIndex —
// first-match on the exact (already upper-cased) name, the same plan-time
// rule every construction-time bake uses.
func (k *cteDedupKeyer) layoutOrdinals(rt *values.RecordType) []int {
	if ords, ok := k.ords[rt]; ok {
		return ords
	}
	ords := make([]int, len(k.cols))
	for i, col := range k.cols {
		if idx, ok := rt.FieldIndex(col); ok {
			ords[i] = idx
		} else {
			ords[i] = -1
		}
	}
	k.ords[rt] = ords
	return ords
}

func (k *cteDedupKeyer) key(qr QueryResult) (string, error) {
	if len(k.cols) == 0 {
		return queryResultKey(qr)
	}
	if qr.Positional == nil || qr.Positional.Type == nil {
		// A layout-less row carries no dedupable values: any constant key
		// would collapse EVERY such row into one (the retired
		// fmt.Sprintf("%v", nil) rendering did exactly that). Loud, never
		// wrong rows (RFC-180 C4).
		return "", fmt.Errorf("CTE dedup over a row with no positional layout — malformed plan")
	}
	ords := k.layoutOrdinals(qr.Positional.Type)
	slots := make([]any, len(ords))
	for i, ord := range ords {
		if ord >= 0 {
			slots[i], _ = qr.Positional.Get(ord)
		}
	}
	return packedDedupKey(slots)
}

// queryResultKey produces a stable string key from a QueryResult's positional
// row for UNION DISTINCT deduplication in recursive CTEs. The key is built
// from VALUES ONLY (sorted by column name for determinism) so rows with
// different column names but identical values (e.g. seed {SRC:1} and
// recursive {DST:1}) are correctly identified as duplicates.
func queryResultKey(qr QueryResult) (string, error) {
	pos := qr.Positional
	if pos == nil || pos.Type == nil {
		// See cteDedupKeyer.key: a layout-less row must not collapse into
		// a shared constant key (RFC-180 C4).
		return "", fmt.Errorf("UNION DISTINCT dedup over a row with no positional layout — malformed plan")
	}
	type nv struct {
		name string
		val  any
	}
	pairs := make([]nv, 0, len(pos.Type.Fields))
	for i, f := range pos.Type.Fields {
		var v any
		if i < len(pos.Slots) {
			v = pos.Slots[i]
		}
		pairs = append(pairs, nv{name: f.Name, val: v})
	}
	sort.Slice(pairs, func(a, b int) bool { return pairs[a].name < pairs[b].name })
	slots := make([]any, len(pairs))
	for i, p := range pairs {
		slots[i] = p.val
	}
	return packedDedupKey(slots)
}
