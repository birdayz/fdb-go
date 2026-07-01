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
	"bytes"
	"container/heap"
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
		// enormous horizon (codex delta P2-A). K ≥ 2 here ⇒ k-1 cannot overflow.
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
	// query, Torvalds 094.4 nit). 0 = "use the maintainer's default"; only
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
		// forced up to the horizon — the spfresh-reviewer / Torvalds Phase B NAK).
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
		// dead ≤0 check (Torvalds + Graefe convergence nits).
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
	return evalCtx.RowContext(nil)
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

// tupleElementToUUID is the inverse of uuidToTupleElement: it normalizes a
// tuple.UUID read back off an index entry / primary key into the neutral
// [16]byte the value layer works with (cmpAny, PromoteValue, materialization).
// Applied at the covering-index read boundary so a UUID column flows downstream
// as [16]byte regardless of whether it was sourced from a stored record
// (protoFieldToGo) or an index entry — the two must be interchangeable for
// residual filters and INL join keys. Non-UUID tuple elements pass through.
func tupleElementToUUID(v any) any {
	if u, ok := v.(tuple.UUID); ok {
		return [16]byte(u)
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
	// RFC-173 P2: dual-emit a DENSE positional row over the covering index's schema
	// (value columns then PK columns, in order). An out-of-range column is a nil
	// slot (NULL) — matching the map omitting its key.
	posNames := make([]string, 0, len(c.columns)+len(c.pkColumns))
	posSlots := make([]any, 0, len(c.columns)+len(c.pkColumns))
	for i, col := range c.columns {
		key := strings.ToUpper(col)
		var v any
		if i < len(vals) {
			v = tupleElementToUUID(vals[i])
			datum[key] = v
		}
		posNames = append(posNames, key)
		posSlots = append(posSlots, v)
	}
	// PrimaryKey() may include a record type key prefix (e.g., (recTypeKey, id)).
	// The user-level PK columns are at the tail. Skip the prefix.
	pkOffset := 0
	if len(pk) > len(c.pkColumns) {
		pkOffset = len(pk) - len(c.pkColumns)
	}
	for i, col := range c.pkColumns {
		key := strings.ToUpper(col)
		idx := i + pkOffset
		var v any
		if idx < len(pk) {
			v = tupleElementToUUID(pk[idx])
			datum[key] = v
		}
		posNames = append(posNames, key)
		posSlots = append(posSlots, v)
	}
	pos := &PositionalRow{Type: positionalTypeFromNames(posNames), Slots: posSlots}
	return recordlayer.NewResultWithValue(QueryResult{Datum: datum, Positional: pos}, result.GetContinuation()), nil
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
	needsRowCtx := len(evalCtx.params) > 0 || len(evalCtx.scalarSubqueries) > 0 || len(evalCtx.bindings) > 0
	filtered := &filterResultCursor{
		inner: innerCursor,
		pred: func(qr QueryResult) (bool, error) {
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
	// before it skips the offset (codex: `SELECT COUNT(*) FROM t LIMIT 1 OFFSET 1`
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
			// next page resumes mid-window. Not sticky — the next request opens
			// a fresh cursor from this continuation.
			contBytes, encErr := encodeLimitContinuation(result.GetContinuation(), c.remOffset, c.remLimit)
			if encErr != nil {
				return recordlayer.RecordCursorResult[QueryResult]{}, encErr
			}
			return recordlayer.NewResultNoNext[QueryResult](
				reason, recordlayer.NewBytesContinuation(contBytes),
			), nil
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
			key := distinctKey(qr)
			added, err := seen.Add(key, int64(len(key)))
			if err != nil {
				return false, err
			}
			return added, nil
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
	// RFC-173 P2: the projection's output schema is row-invariant — compute the
	// column names and the (dup-safe) positional RecordType ONCE, then emit a
	// PositionalRow per row alongside the name-keyed map.
	projNames := make([]string, len(projections))
	for i, proj := range projections {
		projNames[i] = projectionColumnName(proj)
	}
	projType := positionalTypeFromNames(projNames)
	var evalErr error
	mapped := recordlayer.MapCursor(innerCursor, func(qr QueryResult) QueryResult {
		if evalErr != nil {
			return qr
		}
		projected := make(map[string]any, len(projections))
		slots := make([]any, len(projections))
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
			key := projNames[i]
			val, err := proj.Evaluate(rowCtx)
			if err != nil {
				evalErr = err
				return qr
			}
			projected[key] = val
			slots[i] = val // RFC-173 P2: dense positional slot (kept even on dup names)
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
		}
		return QueryResult{
			Datum:      projected,
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

	cursor := newMemorySortCursor(innerCursor, keyNames, directions, props.State)
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
		items, err := CollectAllBounded(ctx, cursor, props.State, props.GetMaterializationLimit(), "buffered union branch")
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
		// RFC-130: the cross-branch `all` slice holds exactly the rows already
		// charged per branch by CollectAllBounded above (the per-branch `items`
		// slices are GC'd; `all` is the surviving copy). Charging again here
		// would double-count the same resident rows, so this append is plain —
		// the budget is already advanced by the per-branch CollectAllBounded.
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
		// A RecordQueryMapPlan reports its OWN output column names from its result value
		// — do NOT descend through it to the pre-rename names. Mirrors
		// physicalPlanColumnNames (rule_implement_unordered_union.go) so a branch that
		// ImplementUnorderedUnionRule already wrapped in a rename Map reports the SAME
		// (post-rename) names here. Without this, the union position-remap would see the
		// pre-rename names, differ from the first branch, and remap a SECOND time over the
		// already-renamed row → reads missing keys → NULLs (codex). Falls through to the
		// descend/scan path when the Map has no RecordConstructorValue result.
		if mp, ok := p.(*plans.RecordQueryMapPlan); ok {
			if rcv, ok := mp.GetResultValue().(*values.RecordConstructorValue); ok && len(rcv.Fields) > 0 {
				names := make([]string, len(rcv.Fields))
				for i, f := range rcv.Fields {
					// Report the EXACT field name — RecordConstructorValue.Evaluate keys the
					// output row by f.Name verbatim (values.go), so this is the literal row
					// key the union remap must read. Upper-casing it would mismatch a
					// non-uppercase Map field and read a missing key → NULL (codex). Union
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
		// INVARIANT (RFC-081, Graefe): every physical realization of a bare aggregate union
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
		// (mirrors the MapPlan arm, RFC-078 codex). RFC-081.
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

func intersectionCompKeyFunc(keyVals []values.Value) recordlayer.ComparisonKeyFunc[QueryResult] {
	return func(qr QueryResult) tuple.Tuple {
		if len(keyVals) > 0 {
			t := make(tuple.Tuple, len(keyVals))
			for i, kv := range keyVals {
				// Comparison/merge keys are field extractions over the
				// datum; the runtime typed-error family is unreachable
				// here. ComparisonKeyFunc has no error channel, so a
				// stray error is a planner invariant violation (panic,
				// matching the prior no-recover behaviour).
				v, err := kv.Evaluate(qr.Datum)
				if err != nil {
					panic(err)
				}
				// widenInt32: tuple has no int32 (RFC-092). uuidToTupleElement:
				// a UUID comparison/PK key is a neutral [16]byte, which the tuple
				// packer cannot encode — convert it to tuple.UUID, exactly as
				// mergeSortCursor.extractKey does, so compareKeys' Pack doesn't
				// panic on an intersection over a UUID key (RFC-162).
				t[i] = uuidToTupleElement(widenInt32(v))
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
		p.IsLeftOuter(),
		nestedProps,
	)
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
	innerRows, err := CollectAllBounded(ctx, innerCursor, props.State, props.GetMaterializationLimit(), "nested loop join inner side")
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
		p.GetPredicates(), evalCtx, props.State,
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
	// Pass 1 — copy ALL keys verbatim (bare columns AND already-qualified
	// "LEG.COL" keys). A MERGED row (NLJ / EXISTS-over-join output) already
	// carries the authoritative per-leg "A.ID"/"B.ID" keys here.
	for k, v := range outerMap {
		qualified[k] = v
	}
	// Pass 2 — add alias/type-qualified keys for each BARE column, but NEVER
	// overwrite a qualified key already present. Re-qualifying a merged row's
	// BARE column (which is last-leg-wins on a cross-leg name collision, e.g.
	// bare "ID" == the inner leg's id) under a SINGLE leg's alias/type would
	// otherwise clobber that leg's correct "A.ID" with the other leg's value —
	// and since Pass-1's verbatim copy and this re-qualification race on Go map
	// iteration order, the clobber was nondeterministic (a.id/b.id collapsing to
	// one leg on some process seeds; the EXISTS-over-join misroute, RFC-077).
	// The two-pass "copy first, fill-if-absent second" makes the authoritative
	// qualified keys always win.
	for k, v := range outerMap {
		if strings.Contains(k, ".") {
			continue
		}
		if outerQual != "" {
			qk := outerQual + "." + strings.ToUpper(k)
			if _, exists := qualified[qk]; !exists {
				qualified[qk] = v
			}
		}
		if outerAlias != "" && outerType != "" && outerAlias != outerType {
			qk := outerType + "." + strings.ToUpper(k)
			if _, exists := qualified[qk]; !exists {
				qualified[qk] = v
			}
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

func passesJoinPredicates(combined QueryResult, preds []predicates.QueryPredicate, evalCtx *EvaluationContext) (bool, error) {
	if len(preds) == 0 {
		return true, nil
	}
	var rowCtx any = combined.Datum
	if len(evalCtx.params) > 0 || len(evalCtx.scalarSubqueries) > 0 || len(evalCtx.bindings) > 0 {
		if m, ok := combined.Datum.(map[string]any); ok {
			rowCtx = evalCtx.RowContext(m)
		}
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

// streamingAggOutputNames returns the OUTPUT column names a streaming-aggregate
// plan's rows are keyed by — the grouping keys (aggKeyName) followed by each
// aggregate's SQL-visible name: its Alias when present (upper-cased at
// construction), else the canonical aggResultName. Exactly one name per output
// column, matching the keys aggregateCursor.finalizeGroup writes and the schema
// the translator derives (aggregateOutputColumns). Used by planColumnNamesWithMD
// so the UNION position-remap (remapUnionColumnsByPosition) can normalize a
// mismatched-alias aggregate branch to the first branch's names (RFC-078).
func streamingAggOutputNames(p *plans.RecordQueryStreamingAggregationPlan) []string {
	keys := p.GetGroupingKeys()
	aggs := p.GetAggregates()
	names := make([]string, 0, len(keys)+len(aggs))
	for _, k := range keys {
		names = append(names, aggKeyName(k))
	}
	for _, agg := range aggs {
		if agg.Alias != "" {
			names = append(names, strings.ToUpper(agg.Alias))
		} else {
			names = append(names, aggResultName(agg))
		}
	}
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
	defer innerCursor.Close()

	// Pre-materialize the full target set BEFORE deleting anything. A resource-limit
	// cut-off (CollectAllBounded → errIfBufferTruncated → 54F01) must abort the DELETE
	// with ZERO records removed — never leave a partially-applied DELETE staged in an
	// explicit transaction that a later commit would persist (codex RFC-106a). DML runs
	// in one transaction, so the target set is bounded by the tx; the materialization
	// cap is the memory backstop.
	targets, err := CollectAllBounded(ctx, innerCursor, props.State, props.GetMaterializationLimit(), "DELETE target set")
	if err != nil {
		return nil, err
	}

	// Re-check the statement deadline AFTER collection and BEFORE any mutation: if the
	// deadline already passed (collection itself is ctx-gated, but the window after it
	// is not), abort with ZERO records changed (codex RFC-106a). The mutation loop then
	// runs to completion uninterrupted — checking ctx mid-loop would reintroduce the
	// partial-mutation hazard pre-materialization exists to prevent; the loop only
	// stages local writes over a tx-bounded target set.
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	// RFC-130 / codex #328: the DML results echo is NOT separately byte-charged.
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
	innerRows, err := CollectAllBounded(ctx, innerCursor, props.State, props.GetMaterializationLimit(), "INSERT source")
	if err != nil {
		return nil, err
	}

	// Re-check the statement deadline after collection, before any write — abort
	// with ZERO records inserted if already expired (codex RFC-106a; see executeDelete).
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	// Resolved lazily on the first computed-row datum.
	var targetDesc protoreflect.MessageDescriptor

	// Phase 1: build every record to insert and charge its ACTUAL size against the
	// budget — BEFORE any write. Charging the built record (not the source row) accounts
	// INSERT … VALUES with a large literal / a growing projection for its true bytes
	// (codex #328 re-review P2); all charging precedes phase 2's saves, so a budget
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

		if props.State.HasMemLimit() {
			// Match the stored-row estimator: proto wire size PLUS the packed PK tuple
			// the echo's FDBStoredRecord holds separately (codex #328 P2). The PK is not
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
	// staged in an explicit transaction (codex RFC-106a; see executeDelete).
	targets, err := CollectAllBounded(ctx, innerCursor, props.State, props.GetMaterializationLimit(), "UPDATE target set")
	if err != nil {
		return nil, err
	}

	// Re-check the statement deadline after collection, before any mutation — abort
	// with ZERO records changed if already expired (codex RFC-106a; see executeDelete).
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	// Phase 1: build every updated record and charge its ACTUAL post-transform size
	// against the budget — BEFORE any write is staged. Charging the built record (not
	// the source row) accounts a growing UPDATE (small row → large value) for its true
	// bytes (codex #328 re-review P2); doing all of it before phase 2's saves means a
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
			var rowCtx any = qr.Datum
			if len(evalCtx.params) > 0 || len(evalCtx.scalarSubqueries) > 0 {
				if m, ok := qr.Datum.(map[string]any); ok {
					rowCtx = evalCtx.RowContext(m)
				}
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
			// PLUS the packed PK tuple, which the echo's FDBStoredRecord holds separately
			// (codex #328 P2). An UPDATE does not change the PK, so the target's PK is the
			// echo's PK.
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
		// column). Matches ConvertToProtoValue's VALUES path.
		// TODO(RFC-083 follow-up): int64→float32 silently produces ±Inf for
		// |n| > MaxFloat32 (~3.4e38); the float64→FLOAT arm above range-checks but
		// these do not. Verify Java's CastValue.LONG_TO_FLOAT behaviour and mirror it
		// (the same gap pre-exists in ConvertToProtoValue's FloatKind branch).
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
	result, err := cv.Evaluate(evalCtx)
	if err != nil {
		return nil, err
	}
	if result == nil {
		return applySkipLimit(recordlayer.Empty[QueryResult](), props.Skip, props.ReturnedRowLimit), nil
	}
	list, ok := result.([]any)
	if !ok {
		// A non-list scalar yields a single row. With ordinality it gets
		// ordinal 1 (the SQL standard's 1-based position of the sole element).
		if p.IsWithOrdinality() {
			return applySkipLimit(
				recordlayer.FromList([]QueryResult{{Datum: explodeOrdinalityRow(result, 1)}}),
				props.Skip, props.ReturnedRowLimit,
			), nil
		}
		return applySkipLimit(
			recordlayer.FromList([]QueryResult{{Datum: result}}),
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
			items[i] = QueryResult{Datum: explodeOrdinalityRow(elem, i+1)}
			continue
		}
		items[i] = QueryResult{Datum: elem}
	}
	return applySkipLimit(recordlayer.FromList(items), props.Skip, props.ReturnedRowLimit), nil
}

// explodeOrdinalityRow builds the 2-field anonymous record a WITH
// ORDINALITY Explode emits: the element under the ordinal-0 key and the
// 1-based ordinal under the ordinal-1 key (Java's q1._0 / q1._1). Keyed by
// the same `_0`/`_1` names values.OrdinalFieldName produces, so an
// ordinal-indexed FieldValue resolves them.
func explodeOrdinalityRow(element any, ordinal int) map[string]any {
	return map[string]any{
		values.OrdinalFieldName(0): element,
		values.OrdinalFieldName(1): int64(ordinal),
	}
}

func executeValues(p *plans.RecordQueryValuesPlan, evalCtx *EvaluationContext) (recordlayer.RecordCursor[QueryResult], error) {
	cols := p.GetColumns()
	row := make(map[string]any, len(cols))
	for _, col := range cols {
		v, err := col.Evaluate(evalCtx)
		if err != nil {
			return nil, err
		}
		row[col.Name()] = v
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

	scanTable := NewTempTableWithState(props.State)
	insertTable := NewTempTableWithState(props.State)

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
	// Extract canonical column names from the seed datum so the dedup
	// key only considers CTE-relevant columns (ignoring extra join
	// columns the recursive branch may carry in its datum).
	distinct := p.IsDistinct()
	// RFC-130: the UNION-DISTINCT seen-set is a cross-level cardinality-growing
	// buffer (one key per distinct row, held across all levels) — charge each
	// NEW key via boundedSet.
	var seen *boundedSet[string]
	var canonicalCols []string
	if distinct {
		seen = newBoundedSet[string](props.State)
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
				k := queryResultKeyForCols(it, canonicalCols)
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

	rootRows, err := CollectAllBounded(ctx, rootCursor, props.State, props.GetMaterializationLimit(), "recursive DFS join root")
	rootCursor.Close()
	if err != nil {
		return nil, fmt.Errorf("executor: recursive dfs join root collect: %w", err)
	}

	preorder := p.GetTraversalStrategy() == plans.DfsPreorder
	var results []QueryResult
	// RFC-130: the DFS dedup seen-set is a cross-traversal cardinality-growing
	// buffer (one key per distinct visited row) — charge each NEW key via
	// boundedSet.
	var seen *boundedSet[string]
	// For UNION DISTINCT, extract the canonical column names from the
	// root datum. The dedup key must use only these columns so that
	// root rows (with 1 column) and recursive rows (which may carry
	// extra join columns in the datum) produce matching keys.
	var canonicalCols []string
	if p.IsDistinct() {
		seen = newBoundedSet[string](props.State)
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
			added, err := seen.Add(k, int64(len(k)))
			if err != nil {
				return nil, err
			}
			if !added {
				continue
			}
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
	seen *boundedSet[string],
	canonicalCols []string,
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

	children, err := CollectAllBounded(ctx, childCursor, props.State, props.GetMaterializationLimit(), "recursive DFS children")
	childCursor.Close()
	if err != nil {
		return fmt.Errorf("recursive DFS collect children: %w", err)
	}

	for _, child := range children {
		if seen != nil {
			k := queryResultKeyForCols(child, canonicalCols)
			added, err := seen.Add(k, int64(len(k)))
			if err != nil {
				return err
			}
			if !added {
				continue
			}
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
	pred   func(QueryResult) (bool, error)
	closed bool
}

func (c *filterResultCursor) OnNext(ctx context.Context) (result recordlayer.RecordCursorResult[QueryResult], err error) {
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
func CollectAllBounded(ctx context.Context, cursor recordlayer.RecordCursor[QueryResult], st *recordlayer.ExecuteState, limit int, opName string) ([]QueryResult, error) {
	buf := newBoundedBuffer[QueryResult](st, limit, opName, estimateQueryResultBytes)
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
		v := result.GetValue()
		if err := buf.Append(v); err != nil {
			return nil, err
		}
	}
	return buf.Items(), nil
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
	return CollectAllBounded(ctx, cursor, nil, limit, opName)
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
	case [16]byte:
		// UUID sorts by unsigned big-endian bytes — same order as the tuple.UUID
		// wire encoding and predicates.cmpAny, so MIN/MAX-style ordering and the
		// aggregate-sort path agree with an ordered index scan.
		bv, ok := b.([16]byte)
		if !ok {
			return 0
		}
		return bytes.Compare(av[:], bv[:])
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
				if k.ValueExpr != nil {
					var err error
					if ci, err = k.ValueExpr.Evaluate(results[i].Datum); err != nil {
						sortErr = err
						return false
					}
					if cj, err = k.ValueExpr.Evaluate(results[j].Datum); err != nil {
						sortErr = err
						return false
					}
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
		return sortErr
	}

	cursor := newCustomSortCursor(innerCursor, sortFn, props.State)
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
