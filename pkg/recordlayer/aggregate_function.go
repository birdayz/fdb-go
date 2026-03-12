package recordlayer

import (
	"bytes"
	"context"
	"fmt"

	"github.com/apple/foundationdb/bindings/go/src/fdb/tuple"
)

// Aggregate function name constants matching Java's FunctionNames.
const (
	FunctionNameCount        = "count"
	FunctionNameCountNotNull = "count_not_null"
	FunctionNameCountUpdates = "count_updates"
	FunctionNameSum          = "sum"
	FunctionNameMinEver      = "min_ever"
	FunctionNameMaxEver      = "max_ever"
	FunctionNameMin          = "min"
	FunctionNameMax          = "max"

	// RANK aggregate function names.
	FunctionNameRankForScore          = "rank_for_score"
	FunctionNameScoreForRank          = "score_for_rank"
	FunctionNameScoreForRankElseSkip  = "score_for_rank_else_skip"
	FunctionNameCountDistinct         = "count_distinct"
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
			return nil, fmt.Errorf("aggregate function %q: %w", fn.Name, &IndexNotFoundError{IndexName: fn.Index})
		}
		if !store.IsIndexReadable(idx.Name) {
			return nil, fmt.Errorf("aggregate function %q: %w", fn.Name, &IndexNotReadableError{IndexName: idx.Name, CurrentState: store.GetIndexState(idx.Name)})
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
	case IndexTypeCountNotNull:
		return fn.Name == FunctionNameCountNotNull && isGroupPrefix(fn.Operand, idx.RootExpression)
	case IndexTypeCountUpdates:
		return fn.Name == FunctionNameCountUpdates && isGroupPrefix(fn.Operand, idx.RootExpression)
	case IndexTypeSum:
		return fn.Name == FunctionNameSum && isGroupPrefix(fn.Operand, idx.RootExpression)
	case IndexTypeMaxEverLong:
		return (fn.Name == FunctionNameMaxEver || fn.Name == IndexTypeMaxEverLong) &&
			isGroupPrefix(fn.Operand, idx.RootExpression)
	case IndexTypeMinEverLong:
		return (fn.Name == FunctionNameMinEver || fn.Name == IndexTypeMinEverLong) &&
			isGroupPrefix(fn.Operand, idx.RootExpression)
	case IndexTypeMaxEverTuple:
		return (fn.Name == FunctionNameMaxEver || fn.Name == IndexTypeMaxEverTuple) &&
			isGroupPrefix(fn.Operand, idx.RootExpression)
	case IndexTypeMaxEverVersion:
		return (fn.Name == FunctionNameMaxEver || fn.Name == IndexTypeMaxEverVersion) &&
			isGroupPrefix(fn.Operand, idx.RootExpression)
	case IndexTypeMinEverTuple:
		return (fn.Name == FunctionNameMinEver || fn.Name == IndexTypeMinEverTuple) &&
			isGroupPrefix(fn.Operand, idx.RootExpression)
	case IndexTypePermutedMin:
		return fn.Name == FunctionNameMin && isGroupPrefix(fn.Operand, idx.RootExpression)
	case IndexTypePermutedMax:
		return fn.Name == FunctionNameMax && isGroupPrefix(fn.Operand, idx.RootExpression)
	case IndexTypeValue:
		// VALUE indexes can serve MIN/MAX by scanning 1 entry forward/reverse.
		// The operand's ungrouped part must be a prefix of the index expression.
		return (fn.Name == FunctionNameMin || fn.Name == FunctionNameMax) &&
			isUngroupedPrefixOf(fn.Operand, idx.RootExpression)
	case IndexTypeRank:
		return canEvaluateRankAggregate(fn, idx)
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
	// For PERMUTED_MIN/MAX indexes: delegate to permuted-specific evaluation.
	// Must check before the generic MIN/MAX path which assumes a plain VALUE index.
	if pm, ok := maintainer.(*permutedMinMaxIndexMaintainer); ok {
		return evaluatePermutedMinMaxAggregate(ctx, fn, pm, scanRange, isolationLevel)
	}

	// For VALUE indexes doing MIN/MAX: scan 1 entry
	if fn.Name == FunctionNameMin || fn.Name == FunctionNameMax {
		return evaluateMinMaxFromValueIndex(ctx, fn, maintainer, scanRange, isolationLevel)
	}

	// For RANK index aggregate functions: delegate to rank-specific evaluation.
	if rm, ok := maintainer.(*RankIndexMaintainer); ok {
		return evaluateRankAggregate(fn, rm, scanRange)
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
	case FunctionNameCount, FunctionNameCountNotNull, FunctionNameCountUpdates, FunctionNameSum:
		return tuple.Tuple{int64(0)}, func(accum, entry tuple.Tuple) tuple.Tuple {
			a := accum[0].(int64)
			b := int64(0)
			if len(entry) > 0 {
				b = entry[0].(int64)
			}
			return tuple.Tuple{a + b}
		}
	case FunctionNameMaxEver, IndexTypeMaxEverLong, IndexTypeMaxEverTuple, IndexTypeMaxEverVersion:
		return nil, func(accum, entry tuple.Tuple) tuple.Tuple {
			if accum == nil {
				return entry
			}
			if len(entry) == 0 {
				return accum
			}
			if len(accum) == 0 || tupleGreater(entry, accum) {
				return entry
			}
			return accum
		}
	case FunctionNameMinEver, IndexTypeMinEverLong, IndexTypeMinEverTuple:
		return nil, func(accum, entry tuple.Tuple) tuple.Tuple {
			if accum == nil {
				return entry
			}
			if len(entry) == 0 {
				return accum
			}
			if len(accum) == 0 || tupleLess(entry, accum) {
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

// tupleGreater returns true if a > b using FDB tuple byte ordering.
// Used for MAX_EVER aggregation on tuple-packed values.
func tupleGreater(a, b tuple.Tuple) bool {
	return bytes.Compare(a.Pack(), b.Pack()) > 0
}

// tupleLess returns true if a < b using FDB tuple byte ordering.
// Used for MIN_EVER aggregation on tuple-packed values.
func tupleLess(a, b tuple.Tuple) bool {
	return bytes.Compare(a.Pack(), b.Pack()) < 0
}

// canEvaluateRankAggregate checks if a RANK index can serve a given aggregate function.
// Matches Java's RankIndexMaintainer.canEvaluateAggregateFunction().
func canEvaluateRankAggregate(fn *IndexAggregateFunction, idx *Index) bool {
	switch fn.Name {
	case FunctionNameCountDistinct:
		return expressionsEqual(fn.Operand, idx.RootExpression)
	case FunctionNameCount:
		// COUNT on a unique RANK index where the operand covers only grouping columns.
		if !idx.IsUnique() {
			return false
		}
		groupingCount := 0
		if g, ok := idx.RootExpression.(*GroupingKeyExpression); ok {
			groupingCount = g.GetGroupingCount()
		}
		return keyExpressionColumnSize(fn.Operand) == groupingCount &&
			isGroupPrefix(fn.Operand, idx.RootExpression)
	case FunctionNameScoreForRank, FunctionNameScoreForRankElseSkip, FunctionNameRankForScore:
		return expressionsEqual(fn.Operand, idx.RootExpression)
	default:
		return false
	}
}

// evaluateRankAggregate evaluates a RANK aggregate function using the ranked set.
// The scanRange must be an "equals" range (all Low == High values).
// Matches Java's RankIndexMaintainer.evaluateAggregateFunction().
func evaluateRankAggregate(
	fn *IndexAggregateFunction,
	rm *RankIndexMaintainer,
	scanRange TupleRange,
) (tuple.Tuple, error) {
	groupPrefixSize := rm.getGroupingCount()

	// Extract the group prefix and the trailing values from the scan range.
	// The scan range for RANK aggregates must be an "equals" range.
	groupPrefix, trailingValues, err := splitEqualRangeForRank(scanRange, groupPrefixSize)
	if err != nil {
		return nil, fmt.Errorf("evaluate %s: %w", fn.Name, err)
	}

	// Build the ranked set subspace for this group.
	rankSubspace := rm.secondarySubspace
	if len(groupPrefix) > 0 {
		elems := make(tuple.Tuple, len(groupPrefix))
		for i, v := range groupPrefix {
			elems[i] = v
		}
		rankSubspace = rankSubspace.Sub(elems...)
	}
	rankedSet := NewRankedSet(rankSubspace, rm.rankedSetConfig)

	// Init if needed.
	needed, err := rankedSet.InitNeeded(rm.tx.Snapshot())
	if err != nil {
		return nil, err
	}
	if needed {
		if err := rankedSet.Init(rm.tx); err != nil {
			return nil, err
		}
	}

	switch fn.Name {
	case FunctionNameCount, FunctionNameCountDistinct:
		size, err := rankedSet.Size(rm.tx)
		if err != nil {
			return nil, err
		}
		return tuple.Tuple{size}, nil

	case FunctionNameScoreForRank, FunctionNameScoreForRankElseSkip:
		if len(trailingValues) == 0 {
			return nil, nil
		}
		rank, ok := trailingValues[0].(int64)
		if !ok {
			return nil, fmt.Errorf("evaluate %s: rank must be int64, got %T", fn.Name, trailingValues[0])
		}
		scoreBytes, err := rankedSet.GetNth(rm.tx, rank)
		if err != nil {
			return nil, err
		}
		if scoreBytes == nil {
			if fn.Name == FunctionNameScoreForRankElseSkip {
				// Return a sentinel value matching Java's COMPARISON_SKIPPED_BINDING.
				return tuple.Tuple{"*"}, nil
			}
			return nil, nil
		}
		scoreTuple, err := tuple.Unpack(scoreBytes)
		if err != nil {
			return nil, fmt.Errorf("evaluate %s: unpack score: %w", fn.Name, err)
		}
		return scoreTuple, nil

	case FunctionNameRankForScore:
		if len(trailingValues) == 0 {
			return nil, nil
		}
		// The trailing values form the score tuple. Pack the full sub-tuple,
		// matching Java's rankForScore(state, rankedSet, values, false)
		// where values is the complete sub-tuple after group prefix.
		rankResult, err := rankedSet.Rank(rm.tx, trailingValues.Pack(), false)
		if err != nil {
			return nil, err
		}
		if rankResult == nil {
			return nil, nil
		}
		return tuple.Tuple{*rankResult}, nil

	default:
		return nil, fmt.Errorf("unsupported RANK aggregate function: %s", fn.Name)
	}
}

// splitEqualRangeForRank extracts group prefix and trailing values from a TupleRange
// that must be an "equals" range (Low == High). Returns the group prefix elements
// and the trailing values tuple (rank or score components), if any.
// Matches Java's evaluateEqualRange which uses subTuple(values, groupingCount, size).
func splitEqualRangeForRank(scanRange TupleRange, groupPrefixSize int) ([]any, tuple.Tuple, error) {
	if scanRange.Low == nil {
		return nil, nil, nil
	}

	values := scanRange.Low
	if len(values) <= groupPrefixSize {
		// Only group prefix, no trailing values.
		groupPrefix := make([]any, len(values))
		for i, v := range values {
			groupPrefix[i] = v
		}
		return groupPrefix, nil, nil
	}

	groupPrefix := make([]any, groupPrefixSize)
	for i := range groupPrefixSize {
		groupPrefix[i] = values[i]
	}
	trailingValues := tuple.Tuple(values[groupPrefixSize:])
	return groupPrefix, trailingValues, nil
}

// expressionsEqual checks if two key expressions are structurally equivalent.
func expressionsEqual(a, b KeyExpression) bool {
	if a == nil && b == nil {
		return true
	}
	if a == nil || b == nil {
		return false
	}
	// Compare by field names and column sizes.
	aNames := a.FieldNames()
	bNames := b.FieldNames()
	if len(aNames) != len(bNames) {
		return false
	}
	for i := range aNames {
		if aNames[i] != bNames[i] {
			return false
		}
	}
	// Also check grouping structure.
	aGrouping, aOk := a.(*GroupingKeyExpression)
	bGrouping, bOk := b.(*GroupingKeyExpression)
	if aOk != bOk {
		return false
	}
	if aOk && bOk {
		return aGrouping.groupedCount == bGrouping.groupedCount
	}
	// Check KeyWithValueExpression structure.
	aKwv, aOk := a.(*KeyWithValueExpression)
	bKwv, bOk := b.(*KeyWithValueExpression)
	if aOk != bOk {
		return false
	}
	if aOk && bOk {
		return aKwv.splitPoint == bKwv.splitPoint
	}
	return true
}
