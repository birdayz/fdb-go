package recordlayer

import (
	"context"
	"fmt"

	"github.com/apple/foundationdb/bindings/go/src/fdb/tuple"
)

// Aggregate function name constants matching Java's FunctionNames.
const (
	FunctionNameCount     = "count"
	FunctionNameSum       = "sum"
	FunctionNameMinEver   = "min_ever"
	FunctionNameMaxEver   = "max_ever"
	FunctionNameMin       = "min"
	FunctionNameMax       = "max"
)

// IndexAggregateFunction specifies an aggregate computation to evaluate via an index.
// Matches Java's com.apple.foundationdb.record.metadata.IndexAggregateFunction.
type IndexAggregateFunction struct {
	Name    string        // Function name (e.g. "count", "sum", "min_ever")
	Operand KeyExpression // The operand (typically a GroupingKeyExpression)
	Index   string        // Optional: explicit index name. Empty = auto-select.
}

// EvaluateAggregateFunction evaluates an aggregate function using the best matching index.
// Returns the aggregate result as a tuple, or nil if no matching entries exist.
//
// For COUNT/SUM indexes: scans all group entries and reduces them.
// For MIN_EVER/MAX_EVER indexes: scans all group entries and reduces them.
// For VALUE indexes with MIN/MAX: scans 1 entry in the right direction.
//
// Matches Java's FDBRecordStore.evaluateAggregateFunction().
func (store *FDBRecordStore) EvaluateAggregateFunction(
	ctx context.Context,
	recordTypeNames []string,
	fn *IndexAggregateFunction,
	scanRange TupleRange,
	isolationLevel IsolationLevel,
) (tuple.Tuple, error) {
	index, err := store.findIndexForAggregateFunction(fn, recordTypeNames)
	if err != nil {
		return nil, err
	}

	maintainer := store.getIndexMaintainer(index)
	return evaluateAggregate(ctx, fn, maintainer, scanRange, isolationLevel)
}

// findIndexForAggregateFunction locates the best index that can evaluate the given
// aggregate function. If fn.Index is set, uses that index directly. Otherwise,
// searches all READABLE indexes for the given record types.
// Matches Java's IndexFunctionHelper.indexMaintainerForAggregateFunction().
func (store *FDBRecordStore) findIndexForAggregateFunction(
	fn *IndexAggregateFunction,
	recordTypeNames []string,
) (*Index, error) {
	// Explicit index specified
	if fn.Index != "" {
		idx := store.metaData.GetIndex(fn.Index)
		if idx == nil {
			return nil, fmt.Errorf("aggregate function %q: index %q not found", fn.Name, fn.Index)
		}
		if !store.IsIndexReadable(idx.Name) {
			return nil, fmt.Errorf("aggregate function %q: index %q is not readable", fn.Name, fn.Index)
		}
		return idx, nil
	}

	// Auto-select: find all indexes for the given record types and pick the first
	// that can evaluate this function (smallest column size = least work).
	var candidates []*Index
	if len(recordTypeNames) == 0 {
		// All indexes
		for _, idx := range store.metaData.GetAllIndexes() {
			candidates = append(candidates, idx)
		}
	} else {
		seen := make(map[string]bool)
		for _, rtName := range recordTypeNames {
			for _, idx := range store.metaData.GetIndexesForRecordType(rtName) {
				if !seen[idx.Name] {
					seen[idx.Name] = true
					candidates = append(candidates, idx)
				}
			}
		}
	}

	var best *Index
	bestColSize := int(^uint(0) >> 1) // MaxInt

	for _, idx := range candidates {
		if !store.IsIndexReadable(idx.Name) {
			continue
		}
		if canEvaluateAggregate(fn, idx) {
			colSize := keyExpressionColumnSize(idx.RootExpression)
			if colSize < bestColSize {
				best = idx
				bestColSize = colSize
			}
		}
	}

	if best == nil {
		return nil, fmt.Errorf("no index found for aggregate function %q with operand %T", fn.Name, fn.Operand)
	}
	return best, nil
}

// canEvaluateAggregate checks if an index can serve a given aggregate function.
// Matches Java's IndexMaintainer.canEvaluateAggregateFunction().
func canEvaluateAggregate(fn *IndexAggregateFunction, idx *Index) bool {
	switch idx.Type {
	case IndexTypeCount:
		return fn.Name == FunctionNameCount && isGroupPrefix(fn.Operand, idx.RootExpression)
	case IndexTypeSum:
		return fn.Name == FunctionNameSum && isGroupPrefix(fn.Operand, idx.RootExpression)
	case IndexTypeMaxEverLong:
		return (fn.Name == FunctionNameMaxEver || fn.Name == IndexTypeMaxEverLong) &&
			isGroupPrefix(fn.Operand, idx.RootExpression)
	case IndexTypeMinEverLong:
		return (fn.Name == FunctionNameMinEver || fn.Name == IndexTypeMinEverLong) &&
			isGroupPrefix(fn.Operand, idx.RootExpression)
	case IndexTypeValue:
		// VALUE indexes can serve MIN/MAX by scanning 1 entry forward/reverse.
		// The operand's ungrouped part must be a prefix of the index expression.
		return (fn.Name == FunctionNameMin || fn.Name == FunctionNameMax) &&
			isUngroupedPrefixOf(fn.Operand, idx.RootExpression)
	default:
		return false
	}
}

// evaluateAggregate dispatches to the appropriate evaluation strategy.
func evaluateAggregate(
	ctx context.Context,
	fn *IndexAggregateFunction,
	maintainer IndexMaintainer,
	scanRange TupleRange,
	isolationLevel IsolationLevel,
) (tuple.Tuple, error) {
	// For VALUE indexes doing MIN/MAX: scan 1 entry
	if fn.Name == FunctionNameMin || fn.Name == FunctionNameMax {
		return evaluateMinMaxFromValueIndex(ctx, fn, maintainer, scanRange, isolationLevel)
	}

	// For atomic mutation indexes (COUNT/SUM/MIN_EVER/MAX_EVER): scan all + reduce
	return evaluateAtomicAggregate(ctx, fn, maintainer, scanRange, isolationLevel)
}

// evaluateMinMaxFromValueIndex gets MIN or MAX from a VALUE index by scanning
// 1 entry in the appropriate direction (forward for MIN, reverse for MAX).
// Matches Java's ValueIndexMaintainer.evaluateAggregateFunction().
func evaluateMinMaxFromValueIndex(
	ctx context.Context,
	fn *IndexAggregateFunction,
	maintainer IndexMaintainer,
	scanRange TupleRange,
	isolationLevel IsolationLevel,
) (tuple.Tuple, error) {
	reverse := fn.Name == FunctionNameMax

	props := ScanProperties{
		ExecuteProperties: ExecuteProperties{
			ReturnedRowLimit: 1,
			IsolationLevel:   isolationLevel,
		},
		Reverse: reverse,
	}

	entry, err := First(ctx, maintainer.Scan(scanRange, nil, props))
	if err != nil {
		return nil, fmt.Errorf("evaluate %s from VALUE index: %w", fn.Name, err)
	}
	if entry == nil {
		return nil, nil
	}

	// Extract the aggregated column from the index key.
	// For a GroupingKeyExpression operand, the grouping columns come first,
	// then the aggregated columns.
	groupSize := 0
	totalSize := keyExpressionColumnSize(fn.Operand)
	if g, ok := fn.Operand.(*GroupingKeyExpression); ok {
		groupSize = g.GetGroupingCount()
	}

	key := (*entry).Key
	if groupSize < len(key) && totalSize <= len(key) {
		return tuple.Tuple(key[groupSize:totalSize]), nil
	}
	return tuple.Tuple(key), nil
}

// evaluateAtomicAggregate evaluates an aggregate by scanning all entries and
// reducing them. Used for COUNT, SUM, MIN_EVER_LONG, MAX_EVER_LONG indexes.
// Matches Java's AtomicMutationIndexMaintainer.evaluateAggregateFunction().
func evaluateAtomicAggregate(
	ctx context.Context,
	fn *IndexAggregateFunction,
	maintainer IndexMaintainer,
	scanRange TupleRange,
	isolationLevel IsolationLevel,
) (tuple.Tuple, error) {
	props := ScanProperties{
		ExecuteProperties: ExecuteProperties{
			IsolationLevel: isolationLevel,
		},
	}

	entries, err := AsList(ctx, maintainer.Scan(scanRange, nil, props))
	if err != nil {
		return nil, fmt.Errorf("evaluate %s aggregate: %w", fn.Name, err)
	}

	identity, aggregator := getAggregator(fn.Name)

	result := identity
	for _, e := range entries {
		result = aggregator(result, e.Value)
	}
	return result, nil
}

// getAggregator returns the identity value and aggregation function for a given
// aggregate function name. Matches Java's AtomicMutation.getIdentity()/getAggregator().
func getAggregator(name string) (tuple.Tuple, func(accum, entry tuple.Tuple) tuple.Tuple) {
	switch name {
	case FunctionNameCount, FunctionNameSum:
		return tuple.Tuple{int64(0)}, func(accum, entry tuple.Tuple) tuple.Tuple {
			a := accum[0].(int64)
			b := int64(0)
			if len(entry) > 0 {
				b = entry[0].(int64)
			}
			return tuple.Tuple{a + b}
		}
	case FunctionNameMaxEver, IndexTypeMaxEverLong:
		return nil, func(accum, entry tuple.Tuple) tuple.Tuple {
			if accum == nil {
				return entry
			}
			if len(entry) == 0 {
				return accum
			}
			if len(accum) == 0 || entry[0].(int64) > accum[0].(int64) {
				return entry
			}
			return accum
		}
	case FunctionNameMinEver, IndexTypeMinEverLong:
		return nil, func(accum, entry tuple.Tuple) tuple.Tuple {
			if accum == nil {
				return entry
			}
			if len(entry) == 0 {
				return accum
			}
			if len(accum) == 0 || entry[0].(int64) < accum[0].(int64) {
				return entry
			}
			return accum
		}
	default:
		// Fallback: just return last entry
		return nil, func(_, entry tuple.Tuple) tuple.Tuple {
			return entry
		}
	}
}

// isGroupPrefix checks if the function operand's grouping part is a prefix of
// the index root expression's grouping part.
// Matches Java's IndexFunctionHelper.isGroupPrefix().
func isGroupPrefix(operand KeyExpression, indexRoot KeyExpression) bool {
	operandGrouping := getGroupingColumns(operand)
	indexGrouping := getGroupingColumns(indexRoot)

	// Operand's grouping part must be a prefix of index's grouping part
	if len(operandGrouping) > len(indexGrouping) {
		return false
	}
	for i := range operandGrouping {
		if operandGrouping[i] != indexGrouping[i] {
			return false
		}
	}
	return true
}

// isUngroupedPrefixOf checks if the operand's ungrouped (aggregated) part
// is a prefix of the index root expression. Used for VALUE index MIN/MAX.
// Matches Java's ValueIndexMaintainer.canEvaluateAggregateFunction().
func isUngroupedPrefixOf(operand KeyExpression, indexRoot KeyExpression) bool {
	operandFields := operand.FieldNames()
	indexFields := indexRoot.FieldNames()

	if len(operandFields) > len(indexFields) {
		return false
	}
	for i := range operandFields {
		if operandFields[i] != indexFields[i] {
			return false
		}
	}
	return true
}

// getGroupingColumns returns the field names of the grouping (non-aggregated) part.
func getGroupingColumns(expr KeyExpression) []string {
	if g, ok := expr.(*GroupingKeyExpression); ok {
		all := g.wholeKey.FieldNames()
		groupingCount := g.GetGroupingCount()
		if groupingCount <= len(all) {
			return all[:groupingCount]
		}
		return all
	}
	// Non-grouped expression: all columns are grouping
	return expr.FieldNames()
}
