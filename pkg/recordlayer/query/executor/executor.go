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
	"sort"
	"strings"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"

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

	case *plans.RecordQueryFetchFromPartialRecordPlan:
		return executeFetchFromPartialRecord(ctx, p, store, evalCtx, continuation, props)

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
		types := p.GetRecordTypes()
		if len(types) == 1 {
			md := store.GetMetaData()
			rt := md.GetRecordType(types[0])
			if rt != nil && rt.PrimaryKey != nil && recordlayer.KeyExpressionHasRecordTypePrefix(rt.PrimaryKey) {
				rtk := rt.GetRecordTypeKey()
				tupleRange = tupleRange.Prepend(tuple.Tuple{rtk})
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
		return &coveringIndexCursor{
			inner:   indexCursor,
			columns: p.GetCoveringColumns(),
		}, nil
	}

	resultCursor := &indexFetchCursor{
		inner: indexCursor,
		store: store,
	}

	return resultCursor, nil
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

	if len(prefix) == 0 {
		lowEndpoint = recordlayer.EndpointTypeTreeStart
		highEndpoint = recordlayer.EndpointTypeTreeEnd
	} else {
		lowEndpoint = recordlayer.EndpointTypeRangeInclusive
		highEndpoint = recordlayer.EndpointTypeRangeInclusive
	}

	for _, ineq := range nextRange.GetInequalityComparisons() {
		var comparand any
		if ineq.Operand != nil {
			comparand = ineq.Operand.Evaluate(binder)
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
			if !hasLow {
				lowEndpoint = recordlayer.EndpointTypeRangeExclusive
				hasLow = true
			}
		case predicates.ComparisonLessThanOrEq:
			highItem = comparand
			highEndpoint = recordlayer.EndpointTypeRangeInclusive
			hasHigh = true
			if !hasLow {
				lowEndpoint = recordlayer.EndpointTypeRangeExclusive
				hasLow = true
			}
		case predicates.ComparisonIsNotNull:
			if !hasLow {
				lowEndpoint = recordlayer.EndpointTypeRangeExclusive
				hasLow = true
			}
		}
	}

	var low, high tuple.Tuple
	if hasLow && lowItem != nil {
		low = append(append(tuple.Tuple{}, prefix...), lowItem)
	} else if len(prefix) > 0 {
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
	inner   recordlayer.RecordCursor[*recordlayer.IndexEntry]
	columns []string
	closed  bool
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
	datum := make(map[string]any, len(c.columns))
	for i, col := range c.columns {
		if i < len(vals) {
			datum[col] = vals[i]
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
	needsRowCtx := len(evalCtx.params) > 0 || len(evalCtx.scalarSubqueries) > 0
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
			if needsRowCtx {
				if m, ok := qr.Datum.(map[string]any); ok {
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
		return fmt.Sprintf("%v", qr.Datum)
	}
	// Sort keys for deterministic output — map iteration order is random.
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
		sb.WriteString(fmt.Sprintf("%v", m[k]))
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
		if needsRowCtx {
			if m, ok := qr.Datum.(map[string]any); ok {
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
		if decErr == nil {
			innerContinuation = ic
			priorBuf = buf
		} else {
			innerContinuation = continuation
		}
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

	var all []QueryResult
	var firstBranchKeys []string
	for branchIdx, inner := range inners {
		cursor, err := ExecutePlan(ctx, inner, store, evalCtx, continuation, props.ClearSkipAndLimit())
		if err != nil {
			return nil, err
		}
		items, err := CollectAll(ctx, cursor)
		if err != nil {
			return nil, err
		}
		if branchIdx == 0 {
			firstBranchKeys = planColumnNamesWithMD(inner, md)
			if firstBranchKeys == nil && len(items) > 0 {
				if m, ok := items[0].Datum.(map[string]any); ok {
					firstBranchKeys = mapKeysOrdered(m)
				}
			}
		}
		if branchIdx > 0 && len(firstBranchKeys) > 0 {
			srcKeys := planColumnNamesWithMD(inner, md)
			if srcKeys == nil && len(items) > 0 {
				if m, ok := items[0].Datum.(map[string]any); ok {
					srcKeys = mapKeysOrdered(m)
				}
			}
			if srcKeys != nil {
				for i := range items {
					items[i] = remapUnionColumnsByPosition(items[i], srcKeys, firstBranchKeys)
				}
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
	if md != nil {
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
	if !ok || len(srcKeys) != len(targetKeys) {
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
		if v, ok := m[srcKey]; ok {
			remapped[targetKeys[i]] = v
		}
	}
	return QueryResult{Datum: remapped, Record: qr.Record, PrimaryKey: qr.PrimaryKey}
}

func remapUnionColumns(qr QueryResult, targetKeys []string) QueryResult {
	m, ok := qr.Datum.(map[string]any)
	if !ok {
		return qr
	}
	srcKeys := mapKeysOrdered(m)
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

	keyVals := p.GetComparisonKeyValues()

	firstCursor, err := ExecutePlan(ctx, inners[0], store, evalCtx, continuation, props.ClearSkipAndLimit())
	if err != nil {
		return nil, err
	}
	firstItems, err := CollectAll(ctx, firstCursor)
	if err != nil {
		return nil, err
	}

	otherSets := make([]map[string]struct{}, len(inners)-1)
	for i := 1; i < len(inners); i++ {
		cursor, err := ExecutePlan(ctx, inners[i], store, evalCtx, continuation, props.ClearSkipAndLimit())
		if err != nil {
			return nil, err
		}
		items, err := CollectAll(ctx, cursor)
		if err != nil {
			return nil, err
		}
		set := make(map[string]struct{}, len(items))
		for _, item := range items {
			set[intersectionKey(item, keyVals)] = struct{}{}
		}
		otherSets[i-1] = set
	}

	var results []QueryResult
	for _, item := range firstItems {
		key := intersectionKey(item, keyVals)
		inAll := true
		for _, set := range otherSets {
			if _, ok := set[key]; !ok {
				inAll = false
				break
			}
		}
		if inAll {
			results = append(results, item)
		}
	}

	return applySkipLimit(recordlayer.FromList(results), props.Skip, props.ReturnedRowLimit), nil
}

func intersectionKey(qr QueryResult, keyVals []values.Value) string {
	if len(keyVals) == 0 {
		if qr.PrimaryKey != nil {
			return string(qr.PrimaryKey.Pack())
		}
		return fmt.Sprintf("%v", qr.Datum)
	}
	var b strings.Builder
	for _, kv := range keyVals {
		v := kv.Evaluate(qr.Datum)
		fmt.Fprintf(&b, "%v|", v)
	}
	return b.String()
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
	innerRows, err := CollectAll(ctx, innerCursor)
	if err != nil {
		return nil, err
	}

	// Stream the outer side one row at a time via nljCursor.
	outerCursor, err := ExecutePlan(ctx, p.GetOuter(), store, evalCtx, continuation, props.ClearSkipAndLimit())
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

	for k, v := range outerMap {
		merged[k] = v
		// If the key already contains a dot, it was qualified by a previous NLJ
		// level (e.g. "EMP.NAME" from an inner join). Preserve it as-is to avoid
		// double-qualification like "EMP.EMP.NAME". Only qualify bare keys.
		if strings.Contains(k, ".") {
			continue
		}
		// When the outer row is already a merged NLJ result, it may
		// contain both bare keys (e.g. "NAME") and qualified keys
		// (e.g. "EMP.NAME", "DEPT.NAME") from a previous join level.
		// The bare key holds the value from whichever side wrote last
		// (non-deterministic between outer/inner of the prior NLJ).
		// Re-qualifying this bare key under outerQual/outerType would
		// overwrite the correctly-qualified key that already exists.
		// Only set the qualified form when it isn't already present.
		if outerQual != "" {
			qualKey := outerQual + "." + strings.ToUpper(k)
			if _, exists := outerMap[qualKey]; !exists {
				merged[qualKey] = v
			}
		}
		if outerAlias != "" && outerType != "" && outerAlias != outerType {
			qualKey := outerType + "." + strings.ToUpper(k)
			if _, exists := outerMap[qualKey]; !exists {
				merged[qualKey] = v
			}
		}
	}
	for k, v := range innerMap {
		if innerQual == "" || innerQual != outerQual {
			merged[k] = v
		}
		if strings.Contains(k, ".") {
			continue
		}
		if innerQual != "" {
			merged[innerQual+"."+strings.ToUpper(k)] = v
		}
		if innerAlias != "" && innerType != "" && innerAlias != innerType {
			qualKey := innerType + "." + strings.ToUpper(k)
			if _, exists := merged[qualKey]; !exists {
				merged[qualKey] = v
			}
		}
	}
	return QueryResult{Datum: merged, Record: outer.Record, PrimaryKey: outer.PrimaryKey}
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

// equiJoinKey holds two Value extractors for one equi-join column pair.
// outerVal evaluates against the outer row, innerVal against the inner row.
type equiJoinKey struct {
	outerVal values.Value
	innerVal values.Value
}

// extractEquiJoinKeys analyses the NLJ predicates to find equi-join
// conditions (FieldValue = FieldValue) where one side references the
// outer table and the other the inner table. Returns the equi-join
// keys, the residual (non-equi) predicates, and whether at least one
// key was found.
func extractEquiJoinKeys(
	preds []predicates.QueryPredicate,
	outerRows, innerRows []QueryResult,
	outerAlias, innerAlias string,
) ([]equiJoinKey, []predicates.QueryPredicate, bool) {
	if len(preds) == 0 || len(outerRows) == 0 || len(innerRows) == 0 {
		return nil, preds, false
	}

	outerSample, ok1 := outerRows[0].Datum.(map[string]any)
	innerSample, ok2 := innerRows[0].Datum.(map[string]any)
	if !ok1 || !ok2 {
		return nil, preds, false
	}

	outerQual := outerAlias
	if outerQual == "" {
		outerQual = recordTypeName(outerRows[0])
	}
	innerQual := innerAlias
	if innerQual == "" {
		innerQual = recordTypeName(innerRows[0])
	}

	qualifiedOuter := qualifyMap(outerSample, outerQual, outerAlias, recordTypeName(outerRows[0]))
	qualifiedInner := qualifyMap(innerSample, innerQual, innerAlias, recordTypeName(innerRows[0]))

	var keys []equiJoinKey
	var residual []predicates.QueryPredicate

	for _, pred := range preds {
		cp, ok := pred.(*predicates.ComparisonPredicate)
		if !ok || cp.Comparison.Type != predicates.ComparisonEquals {
			residual = append(residual, pred)
			continue
		}
		if cp.Operand == nil || cp.Comparison.Operand == nil {
			residual = append(residual, pred)
			continue
		}

		// Try LHS→outer, RHS→inner
		lhsOuter := cp.Operand.Evaluate(qualifiedOuter)
		rhsInner := cp.Comparison.Operand.Evaluate(qualifiedInner)
		if lhsOuter != nil && rhsInner != nil {
			lhsInner := cp.Operand.Evaluate(qualifiedInner)
			if lhsInner == nil {
				keys = append(keys, equiJoinKey{outerVal: cp.Operand, innerVal: cp.Comparison.Operand})
				continue
			}
		}

		// Try LHS→inner, RHS→outer
		lhsInner := cp.Operand.Evaluate(qualifiedInner)
		rhsOuter := cp.Comparison.Operand.Evaluate(qualifiedOuter)
		if lhsInner != nil && rhsOuter != nil {
			rhsInnerCheck := cp.Comparison.Operand.Evaluate(qualifiedInner)
			if rhsInnerCheck == nil {
				keys = append(keys, equiJoinKey{outerVal: cp.Comparison.Operand, innerVal: cp.Operand})
				continue
			}
		}

		residual = append(residual, pred)
	}

	if len(keys) == 0 {
		return nil, preds, false
	}
	return keys, residual, true
}

// qualifyMap replicates the key qualification from mergeRows so that
// predicate evaluation against individual (non-merged) rows resolves
// qualified field names like "ORDERS.CUSTOMER_ID".
func qualifyMap(row map[string]any, qual, alias, typeName string) map[string]any {
	qualified := make(map[string]any, len(row)*3)
	for k, v := range row {
		qualified[k] = v
		if strings.Contains(k, ".") {
			continue
		}
		if qual != "" {
			qualified[qual+"."+strings.ToUpper(k)] = v
		}
		if alias != "" && typeName != "" && alias != typeName {
			qualified[typeName+"."+strings.ToUpper(k)] = v
		}
	}
	return qualified
}

// buildInnerHashIndex indexes inner rows by the equi-join key values.
// Rows with any NULL key component are excluded (SQL: NULL = NULL is UNKNOWN).
func buildInnerHashIndex(innerRows []QueryResult, keys []equiJoinKey, innerAlias string) (map[string][]QueryResult, []QueryResult) {
	index := make(map[string][]QueryResult, len(innerRows))
	var nullRows []QueryResult
	for _, row := range innerRows {
		k, hasNull := computeJoinKey(row, keys, true, innerAlias)
		if hasNull {
			nullRows = append(nullRows, row)
			continue
		}
		index[k] = append(index[k], row)
	}
	return index, nullRows
}

// computeJoinKey evaluates the equi-join key expressions against a row,
// producing a string hash key. inner=true uses innerVal, inner=false uses outerVal.
// Returns hasNull=true when any key component evaluates to NULL.
func computeJoinKey(row QueryResult, keys []equiJoinKey, inner bool, alias string) (string, bool) {
	rowMap, ok := row.Datum.(map[string]any)
	if !ok {
		return "", true
	}

	typeName := recordTypeName(row)
	qual := alias
	if qual == "" {
		qual = typeName
	}
	qualified := qualifyMap(rowMap, qual, alias, typeName)

	var b strings.Builder
	for _, k := range keys {
		var val any
		if inner {
			val = k.innerVal.Evaluate(qualified)
		} else {
			val = k.outerVal.Evaluate(qualified)
		}
		if val == nil {
			return "", true
		}
		fmt.Fprintf(&b, "%T:%v|", val, val)
	}
	return b.String(), false
}

func passesJoinPredicates(combined QueryResult, preds []predicates.QueryPredicate, evalCtx *EvaluationContext) bool {
	if len(preds) == 0 {
		return true
	}
	var rowCtx any = combined.Datum
	if len(evalCtx.params) > 0 || len(evalCtx.scalarSubqueries) > 0 {
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
		if decErr == nil {
			innerContinuation = ic
			priorGroupKey = gk
			priorState = gs
		} else {
			innerContinuation = continuation
		}
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
	if agg.Operand != nil {
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

	var results []QueryResult
	for {
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
		stored, err := store.SaveRecordWithOptions(qr.Record.Record, recordlayer.RecordExistenceCheckErrorIfExists)
		if err != nil {
			return nil, fmt.Errorf("executor: inserting record: %w", err)
		}
		results = append(results, FromStoredRecord(stored))
	}
	return recordlayer.FromList(results), nil
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
	items, err := CollectAll(ctx, initialCursor)
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
		items, err := CollectAll(ctx, recursiveCursor)
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

	rootRows, err := CollectAll(ctx, rootCursor)
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

	children, err := CollectAll(ctx, childCursor)
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

// CollectAll drains a cursor into a slice.
func CollectAll(ctx context.Context, cursor recordlayer.RecordCursor[QueryResult]) ([]QueryResult, error) {
	var results []QueryResult
	for {
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
		if decErr == nil {
			innerContinuation = ic
			priorBuf = buf
		} else {
			innerContinuation = continuation
		}
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
		sb.WriteString(fmt.Sprintf("%v", m[col]))
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
		sb.WriteString(fmt.Sprintf("%v", m[k]))
	}
	return sb.String()
}
