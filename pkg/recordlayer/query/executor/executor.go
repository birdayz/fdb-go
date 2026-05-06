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
		return executeScan(ctx, p, store, continuation, props)
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
	case *plans.RecordQueryHashAggregationPlan:
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
	continuation []byte,
	props recordlayer.ExecuteProperties,
) (recordlayer.RecordCursor[QueryResult], error) {
	scanProps := recordlayer.ScanProperties{
		ExecuteProperties: props,
		Reverse:           p.IsReverse(),
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
		ExecuteProperties: props,
		Reverse:           p.IsReverse(),
	}

	indexCursor := maintainer.Scan(scanRange, continuation, scanProps)

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
			return recordlayer.NewResultNoNext[QueryResult](recordlayer.SourceExhausted, &recordlayer.EndContinuation{}), nil
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
	innerCursor, err := ExecutePlan(ctx, children[0], store, evalCtx, continuation, props)
	if err != nil {
		return nil, err
	}

	limit := int(p.GetLimit())
	offset := int(p.GetOffset())
	return recordlayer.SkipThenLimit(innerCursor, offset, limit), nil
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
	if m, ok := qr.Datum.(map[string]any); ok {
		return fmt.Sprintf("%v", m)
	}
	return fmt.Sprintf("%v", qr.Datum)
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

func executeSort(
	ctx context.Context,
	p *plans.RecordQuerySortPlan,
	store *recordlayer.FDBRecordStore,
	evalCtx *EvaluationContext,
	continuation []byte,
	props recordlayer.ExecuteProperties,
) (recordlayer.RecordCursor[QueryResult], error) {
	innerCursor, err := ExecutePlan(ctx, p.GetInner(), store, evalCtx, continuation, props.ClearSkipAndLimit())
	if err != nil {
		return nil, err
	}

	items, err := CollectAll(ctx, innerCursor)
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
	sortByKeys(items, keyNames, directions)

	cursor := newSortResultCursor(items)
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
			firstBranchKeys = planColumnNames(inner)
			if firstBranchKeys == nil && len(items) > 0 {
				if m, ok := items[0].Datum.(map[string]any); ok {
					firstBranchKeys = mapKeysOrdered(m)
				}
			}
		}
		if branchIdx > 0 && len(firstBranchKeys) > 0 {
			srcKeys := planColumnNames(inner)
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
			return nil
		}
	}
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
	outerCursor, err := ExecutePlan(ctx, p.GetOuter(), store, evalCtx, continuation, props.ClearSkipAndLimit())
	if err != nil {
		return nil, err
	}

	outerRows, err := CollectAll(ctx, outerCursor)
	if err != nil {
		return nil, err
	}

	preds := p.GetPredicates()
	joinType := p.GetJoinType()
	var results []QueryResult

	if joinType == plans.JoinExists || joinType == plans.JoinNotExists {
		if len(preds) == 0 {
			for _, outerRow := range outerRows {
				innerCursor, innerErr := ExecutePlan(ctx, p.GetInner(), store, evalCtx, nil, props.ClearSkipAndLimit())
				if innerErr != nil {
					return nil, innerErr
				}
				innerResult, innerErr := innerCursor.OnNext(ctx)
				_ = innerCursor.Close()
				if innerErr != nil {
					return nil, innerErr
				}
				hasRow := innerResult.HasNext() && innerResult.GetValue().Datum != nil
				if (joinType == plans.JoinExists && hasRow) || (joinType == plans.JoinNotExists && !hasRow) {
					results = append(results, outerRow)
				}
			}
		} else {
			innerCursor, innerErr := ExecutePlan(ctx, p.GetInner(), store, evalCtx, nil, props.ClearSkipAndLimit())
			if innerErr != nil {
				return nil, innerErr
			}
			innerRows, innerErr := CollectAll(ctx, innerCursor)
			if innerErr != nil {
				return nil, innerErr
			}
			for _, outerRow := range outerRows {
				matched := false
				for _, innerRow := range innerRows {
					combined := mergeRows(outerRow, innerRow, p.GetOuterAlias(), p.GetInnerAlias())
					if passesJoinPredicates(combined, preds, evalCtx) {
						matched = true
						break
					}
				}
				if (joinType == plans.JoinExists && matched) || (joinType == plans.JoinNotExists && !matched) {
					results = append(results, outerRow)
				}
			}
		}
		return applySkipLimit(recordlayer.FromList(results), props.Skip, props.ReturnedRowLimit), nil
	}

	for _, outerRow := range outerRows {
		innerCursor, err := ExecutePlan(ctx, p.GetInner(), store, evalCtx, nil, props.ClearSkipAndLimit())
		if err != nil {
			return nil, err
		}

		innerRows, err := CollectAll(ctx, innerCursor)
		if err != nil {
			return nil, err
		}

		matched := false
		for _, innerRow := range innerRows {
			combined := mergeRows(outerRow, innerRow, p.GetOuterAlias(), p.GetInnerAlias())
			if passesJoinPredicates(combined, preds, evalCtx) {
				results = append(results, combined)
				matched = true
			}
		}

		if !matched && joinType == plans.JoinLeftOuter {
			// Emit the outer row with qualified alias keys so that
			// downstream projections (e.g. "CUSTOMER.NAME") resolve
			// correctly. Inner-table columns are absent from the map;
			// the ResultSet treats missing keys as NULL.
			results = append(results, qualifyOuterRow(outerRow, p.GetOuterAlias()))
		}
	}

	return applySkipLimit(recordlayer.FromList(results), props.Skip, props.ReturnedRowLimit), nil
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
		if outerQual != "" {
			merged[outerQual+"."+strings.ToUpper(k)] = v
		}
		if outerAlias != "" && outerType != "" && outerAlias != outerType {
			merged[outerType+"."+strings.ToUpper(k)] = v
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
			merged[innerType+"."+strings.ToUpper(k)] = v
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
	innerCursor, err := ExecutePlan(ctx, inner, store, evalCtx, continuation, props.ClearSkipAndLimit())
	if err != nil {
		return nil, err
	}

	rows, err := CollectAll(ctx, innerCursor)
	if err != nil {
		return nil, err
	}

	type groupState struct {
		keyVals []any
		count   int64
		counts  []int64
		sums    []float64
		sumsI   []int64
		allInt  []bool
		mins    []any
		maxs    []any
	}

	groups := make(map[string]*groupState)
	var groupOrder []string

	for _, row := range rows {
		keyParts := make([]any, len(groupingKeys))
		var keyStr strings.Builder
		for i, k := range groupingKeys {
			v := k.Evaluate(row.Datum)
			keyParts[i] = v
			if v == nil {
				keyStr.WriteString("N|")
			} else {
				fmt.Fprintf(&keyStr, "%T:%v|", v, v)
			}
		}
		gk := keyStr.String()

		gs, exists := groups[gk]
		if !exists {
			allIntInit := make([]bool, len(aggregates))
			for j := range allIntInit {
				allIntInit[j] = true
			}
			gs = &groupState{
				keyVals: keyParts,
				counts:  make([]int64, len(aggregates)),
				sums:    make([]float64, len(aggregates)),
				sumsI:   make([]int64, len(aggregates)),
				allInt:  allIntInit,
				mins:    make([]any, len(aggregates)),
				maxs:    make([]any, len(aggregates)),
			}
			groups[gk] = gs
			groupOrder = append(groupOrder, gk)
		}
		gs.count++

		for i, agg := range aggregates {
			val := agg.Operand.Evaluate(row.Datum)
			if val == nil {
				continue
			}
			gs.counts[i]++
			if agg.Function == expressions.AggSum || agg.Function == expressions.AggAvg {
				if !isNumeric(val) {
					return nil, fmt.Errorf("cannot aggregate non-numeric value of type %T", val)
				}
			}
			num := toFloat64(val)
			gs.sums[i] += num
			if intVal, ok := val.(int64); ok {
				gs.sumsI[i] += intVal
			} else {
				gs.allInt[i] = false
			}

			if gs.mins[i] == nil || compareAny(val, gs.mins[i]) < 0 {
				gs.mins[i] = val
			}
			if gs.maxs[i] == nil || compareAny(val, gs.maxs[i]) > 0 {
				gs.maxs[i] = val
			}
		}
	}

	if len(groups) == 0 && len(groupingKeys) == 0 {
		result := make(map[string]any)
		for _, agg := range aggregates {
			name := aggResultName(agg)
			var val any
			switch agg.Function {
			case expressions.AggCount:
				val = int64(0)
			default:
				val = nil
			}
			result[name] = val
			if agg.Alias != "" && agg.Alias != name {
				result[agg.Alias] = val
			}
		}
		return recordlayer.FromList([]QueryResult{{Datum: result}}), nil
	}

	var results []QueryResult
	for _, gk := range groupOrder {
		gs := groups[gk]
		result := make(map[string]any)
		for i, k := range groupingKeys {
			result[aggKeyName(k)] = gs.keyVals[i]
		}
		for i, agg := range aggregates {
			name := aggResultName(agg)
			var val any
			switch agg.Function {
			case expressions.AggCount:
				if isCountStar(agg) {
					val = gs.count
				} else {
					val = gs.counts[i]
				}
			case expressions.AggSum:
				if gs.counts[i] == 0 {
					val = nil
				} else if gs.allInt[i] {
					val = gs.sumsI[i]
				} else {
					val = gs.sums[i]
				}
			case expressions.AggMin:
				val = gs.mins[i]
			case expressions.AggMax:
				val = gs.maxs[i]
			case expressions.AggAvg:
				if gs.counts[i] > 0 {
					val = gs.sums[i] / float64(gs.counts[i])
				} else {
					val = nil
				}
			}
			result[name] = val
			if agg.Alias != "" && agg.Alias != name {
				result[agg.Alias] = val
			}
		}
		results = append(results, QueryResult{Datum: result})
	}

	return applySkipLimit(recordlayer.FromList(results), props.Skip, props.ReturnedRowLimit), nil
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
			return protoreflect.ValueOfInt32(int32(n)), nil
		case int32:
			return protoreflect.ValueOfInt32(n), nil
		case int:
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
			return protoreflect.ValueOfUint32(uint32(n)), nil
		case uint32:
			return protoreflect.ValueOfUint32(n), nil
		}
	case protoreflect.Uint64Kind, protoreflect.Fixed64Kind:
		switch n := v.(type) {
		case int64:
			return protoreflect.ValueOfUint64(uint64(n)), nil
		case uint64:
			return protoreflect.ValueOfUint64(n), nil
		}
	case protoreflect.FloatKind:
		switch n := v.(type) {
		case float64:
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
	allResults = append(allResults, items...)

	const maxRecursionDepth = 1000
	for level := 0; ; level++ {
		if len(insertTable.GetList()) == 0 || level >= maxRecursionDepth {
			break
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

	const maxRecursionDepth = 256

	for _, root := range rootRows {
		if err := dfsVisit(ctx, root, p, store, evalCtx, preorder, props, &results, 0, maxRecursionDepth); err != nil {
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
) error {
	if depth >= maxDepth {
		return nil
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
		if err := dfsVisit(ctx, child, p, store, evalCtx, preorder, props, results, depth+1, maxDepth); err != nil {
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
		return false
	})
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
	innerCursor, err := ExecutePlan(ctx, p.GetInner(), store, evalCtx, continuation, props.ClearSkipAndLimit())
	if err != nil {
		return nil, err
	}
	results, err := CollectAll(ctx, innerCursor)
	if err != nil {
		return nil, err
	}

	keys := p.GetSortKeys()
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
		return false
	})

	return applySkipLimit(recordlayer.FromList(results), props.Skip, props.ReturnedRowLimit), nil
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
