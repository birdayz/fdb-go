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
	"sort"
	"strings"

	"google.golang.org/protobuf/proto"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/predicates"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/plans"
)

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
	case *plans.RecordQueryValuesPlan:
		return executeValues(p)
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
		ExecuteProperties: props.ClearSkipAndLimit(),
		Reverse:           p.IsReverse(),
	}

	types := p.GetRecordTypes()
	var inner recordlayer.RecordCursor[*recordlayer.FDBStoredRecord[proto.Message]]
	if len(types) == 1 {
		inner = store.ScanRecordsByType(types[0], continuation, scanProps)
	} else {
		inner = store.ScanRecords(continuation, scanProps)
	}

	cursor := recordlayer.MapCursor(inner, FromStoredRecord)
	return applySkipLimit(cursor, props.Skip, props.ReturnedRowLimit), nil
}

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
	filtered := &filterResultCursor{
		inner: innerCursor,
		pred: func(qr QueryResult) bool {
			for _, pred := range preds {
				if pred.Eval(qr.Datum) != predicates.TriTrue {
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
	if qr.PrimaryKey != nil {
		return string(qr.PrimaryKey.Pack())
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
	mapped := recordlayer.MapCursor(innerCursor, func(qr QueryResult) QueryResult {
		projected := make(map[string]any, len(projections))
		for _, proj := range projections {
			projected[proj.Name()] = proj.Evaluate(qr.Datum)
		}
		return QueryResult{
			Datum:      projected,
			Record:     qr.Record,
			PrimaryKey: qr.PrimaryKey,
		}
	})
	return applySkipLimit(mapped, props.Skip, props.ReturnedRowLimit), nil
}

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

func executeValues(p *plans.RecordQueryValuesPlan) (recordlayer.RecordCursor[QueryResult], error) {
	cols := p.GetColumns()
	row := make(map[string]any, len(cols))
	for _, col := range cols {
		row[col.Name()] = col.Evaluate(nil)
	}
	return recordlayer.FromList([]QueryResult{{Datum: row}}), nil
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

func (c *filterResultCursor) OnNext(ctx context.Context) (recordlayer.RecordCursorResult[QueryResult], error) {
	for {
		result, err := c.inner.OnNext(ctx)
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
		bv, ok := b.(int64)
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
	case float64:
		bv, ok := b.(float64)
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
