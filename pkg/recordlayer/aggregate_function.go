package recordlayer

import (
	"bytes"
	"context"
	"fmt"

	"fdb.dev/pkg/fdbgo/fdb/tuple"
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
	FunctionNameRankForScore         = "rank_for_score"
	FunctionNameScoreForRank         = "score_for_rank"
	FunctionNameScoreForRankElseSkip = "score_for_rank_else_skip"
	FunctionNameCountDistinct        = "count_distinct"

	// BITMAP_VALUE aggregate function name.
	FunctionNameBitmapValue = "bitmap_value"
)

// IndexAggregateFunction specifies an aggregate computation to evaluate via an index.
// Matches Java's com.apple.foundationdb.record.metadata.IndexAggregateFunction.
type IndexAggregateFunction struct {
	Name    string        // Function name (e.g. "count", "sum", "min_ever")
	Operand KeyExpression // The operand (typically a GroupingKeyExpression)
	Index   string        // Optional: explicit index name. Empty = auto-select.
}

// NewCountAggregateFunction creates a COUNT aggregate function.
// Matches Java's IndexAggregateFunction(FunctionNames.COUNT, operand).
func NewCountAggregateFunction(operand KeyExpression) *IndexAggregateFunction {
	return &IndexAggregateFunction{Name: FunctionNameCount, Operand: operand}
}

// NewSumAggregateFunction creates a SUM aggregate function.
func NewSumAggregateFunction(operand KeyExpression) *IndexAggregateFunction {
	return &IndexAggregateFunction{Name: FunctionNameSum, Operand: operand}
}

// NewMinAggregateFunction creates a MIN aggregate function (via VALUE index).
func NewMinAggregateFunction(operand KeyExpression) *IndexAggregateFunction {
	return &IndexAggregateFunction{Name: FunctionNameMin, Operand: operand}
}

// NewMaxAggregateFunction creates a MAX aggregate function (via VALUE index).
func NewMaxAggregateFunction(operand KeyExpression) *IndexAggregateFunction {
	return &IndexAggregateFunction{Name: FunctionNameMax, Operand: operand}
}

// NewMinEverAggregateFunction creates a MIN_EVER aggregate function.
func NewMinEverAggregateFunction(operand KeyExpression) *IndexAggregateFunction {
	return &IndexAggregateFunction{Name: FunctionNameMinEver, Operand: operand}
}

// NewMaxEverAggregateFunction creates a MAX_EVER aggregate function.
func NewMaxEverAggregateFunction(operand KeyExpression) *IndexAggregateFunction {
	return &IndexAggregateFunction{Name: FunctionNameMaxEver, Operand: operand}
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
	index, err := store.findIndexForAggregateFunction(fn, recordTypeNames, nil)
	if err != nil {
		return nil, err
	}

	maintainer, err := store.getIndexMaintainer(index)
	if err != nil {
		return nil, err
	}
	return evaluateAggregate(ctx, fn, maintainer, scanRange, isolationLevel)
}

// AggregateFunctionNotSupportedError is Java's AggregateFunctionNotSupportedException
// (thrown by FDBRecordStore.evaluateAggregateFunction, FDBRecordStore.java:2409-2412)
// — no readable, queryable index over the requested record types can evaluate the
// function.
//
// It is a distinct type because callers branch on it: Java's
// IndexBuildState.loadIndexBuildStateAsync catches exactly this exception to report
// "total unknown" and lets every other failure propagate
// (IndexBuildState.java:76-82).
type AggregateFunctionNotSupportedError struct {
	Function string
	Operand  string
}

func (e *AggregateFunctionNotSupportedError) Error() string {
	return fmt.Sprintf("Aggregate function requires appropriate index: function=%s, operand=%s", e.Function, e.Operand)
}

// indexesForRecordTypes returns the indexes that apply to EXACTLY the given record
// types, no more, no less. Port of Java's IndexFunctionHelper.indexesForRecordTypes
// (IndexFunctionHelper.java:178-189):
//
//	if (recordTypeNames.isEmpty()) {
//	    return metaData.getUniversalIndexes().stream();
//	} else if (recordTypeNames.size() == 1) {
//	    return metaData.getIndexableRecordType(...).getIndexes().stream();
//	} else {
//	    final Set<RecordType> asSet = ...;
//	    return asSet.iterator().next().getMultiTypeIndexes().stream()
//	            .filter(i -> asSet.equals(new HashSet<>(metaData.recordTypesForIndex(i))));
//	}
//
// "Exactly" is the whole point and each branch enforces it in its own way:
//
//   - No record types named means the caller is asking about the WHOLE store, so
//     only a universal index can answer. An index scoped to one record type holds
//     entries for that type alone and would answer a store-wide count with that
//     type's count.
//   - One record type uses RecordType.getIndexes() — the type's OWN single-type
//     indexes. Multi-type indexes are deliberately excluded: by definition they also
//     cover other types, so they cannot answer about this type alone. (A universal
//     index is likewise excluded here, which is why callers that want the universal
//     roll-up ask for it separately with an empty list.)
//   - More than one record type takes the multi-type indexes of an arbitrary member
//     and keeps only those whose covered-type set equals the requested set. An index
//     over a superset would over-count; one over a subset would under-count.
//
// The size test is on the LIST, as in Java, not on the de-duplicated set: a list
// naming the same type twice takes the multi-type branch and, since no multi-type
// index covers exactly one type, finds nothing. That is Java's answer too.
//
// An unknown record type is Java's RecordMetaData.getIndexableRecordType throw
// (RecordMetaData.java:277-279), not an empty candidate list — silently returning no
// candidates would turn a typo into "no appropriate index".
func indexesForRecordTypes(metaData *RecordMetaData, recordTypeNames []string) ([]*Index, error) {
	if len(recordTypeNames) == 0 {
		return metaData.GetUniversalIndexes(), nil
	}
	if len(recordTypeNames) == 1 {
		rt := metaData.GetRecordType(recordTypeNames[0])
		if rt == nil {
			return nil, &MetaDataError{Message: fmt.Sprintf("unknown record type %q", recordTypeNames[0])}
		}
		return rt.GetIndexes(), nil
	}

	asSet := make(map[string]bool, len(recordTypeNames))
	var first *RecordType
	for _, name := range recordTypeNames {
		rt := metaData.GetRecordType(name)
		if rt == nil {
			return nil, &MetaDataError{Message: fmt.Sprintf("unknown record type %q", name)}
		}
		if first == nil {
			first = rt
		}
		asSet[rt.Name] = true
	}

	var result []*Index
	for _, index := range first.GetMultiTypeIndexes() {
		covered := metaData.RecordTypesForIndex(index)
		if len(covered) != len(asSet) {
			continue
		}
		exact := true
		for _, rt := range covered {
			if !asSet[rt.Name] {
				exact = false
				break
			}
		}
		if exact {
			result = append(result, index)
		}
	}
	return result, nil
}

// findIndexForAggregateFunction locates the best index that can evaluate the given
// aggregate function. Port of
// IndexFunctionHelper.indexMaintainerForAggregateFunction
// (IndexFunctionHelper.java:105-122).
//
// queryable is Java's IndexQueryabilityFilter; a nil filter is
// IndexQueryabilityFilter.TRUE, which is what every public entry point passes.
func (store *FDBRecordStore) findIndexForAggregateFunction(
	fn *IndexAggregateFunction,
	recordTypeNames []string,
	queryable func(*Index) bool,
) (*Index, error) {
	// An explicitly named index is used only when it is READABLE. Java does NOT
	// fail when it is not — it falls through to the general search
	// (IndexFunctionHelper.java:110-115: the `if` returns only on the readable
	// path), so a WRITE_ONLY named index degrades to "pick another index that can
	// answer this" rather than to an error.
	if fn.Index != "" {
		idx := store.metaData.GetIndex(fn.Index)
		if idx == nil {
			return nil, fmt.Errorf("aggregate function %q: %w", fn.Name, &IndexNotFoundError{IndexName: fn.Index})
		}
		if store.IsIndexReadable(idx.Name) {
			return idx, nil
		}
	}

	candidates, err := indexesForRecordTypes(store.metaData, recordTypeNames)
	if err != nil {
		return nil, err
	}

	// Prefer the index that does it in the fewest columns, because that means less
	// rolling-up. Ties keep the first candidate, as Java's Stream.min does.
	var best *Index
	for _, idx := range candidates {
		if !store.IsIndexReadable(idx.Name) {
			continue
		}
		if queryable != nil && !queryable(idx) {
			continue
		}
		if !canEvaluateAggregate(fn, idx) {
			continue
		}
		if best == nil || idx.RootExpression.ColumnSize() < best.RootExpression.ColumnSize() {
			best = idx
		}
	}

	if best == nil {
		return nil, &AggregateFunctionNotSupportedError{
			Function: fn.Name,
			Operand:  fmt.Sprintf("%T", fn.Operand),
		}
	}
	return best, nil
}

// canEvaluateAggregate checks if an index can serve a given aggregate function.
// Matches Java's IndexMaintainer.canEvaluateAggregateFunction().
func canEvaluateAggregate(fn *IndexAggregateFunction, idx *Index) bool {
	switch canonicalIndexType(idx.Type) {
	case IndexTypeCount:
		return fn.Name == FunctionNameCount && isGroupPrefix(fn.Operand, idx.RootExpression)
	case IndexTypeCountNotNull:
		return fn.Name == FunctionNameCountNotNull && isGroupPrefix(fn.Operand, idx.RootExpression)
	case IndexTypeCountUpdates:
		return fn.Name == FunctionNameCountUpdates && isGroupPrefix(fn.Operand, idx.RootExpression)
	case IndexTypeSum:
		return fn.Name == FunctionNameSum && isGroupPrefix(fn.Operand, idx.RootExpression)
	case IndexTypeMaxEverLong, IndexTypeMaxEverTuple, IndexTypeMaxEverVersion:
		return fn.Name == FunctionNameMaxEver && isGroupPrefix(fn.Operand, idx.RootExpression)
	case IndexTypeMinEverLong, IndexTypeMinEverTuple:
		return fn.Name == FunctionNameMinEver && isGroupPrefix(fn.Operand, idx.RootExpression)
	case IndexTypePermutedMin:
		return fn.Name == FunctionNameMin && isGroupPrefix(fn.Operand, idx.RootExpression)
	case IndexTypePermutedMax:
		return fn.Name == FunctionNameMax && isGroupPrefix(fn.Operand, idx.RootExpression)
	case IndexTypeValue:
		// VALUE indexes can serve MIN/MAX by scanning 1 entry forward/reverse.
		// The operand's ungrouped part must be a prefix of the index expression.
		return (fn.Name == FunctionNameMin || fn.Name == FunctionNameMax) &&
			isUngroupedPrefixOf(fn.Operand, idx.RootExpression)
	case IndexTypeBitmapValue:
		return fn.Name == FunctionNameBitmapValue && isGroupPrefix(fn.Operand, idx.RootExpression)
	case IndexTypeRank:
		return canEvaluateRankAggregate(fn, idx)
	case IndexTypeTimeWindowLeaderboard:
		switch fn.Name {
		case FunctionNameTimeWindowCount,
			FunctionNameScoreForTimeWindowRank,
			FunctionNameScoreForTimeWindowRankElseSkip,
			FunctionNameTimeWindowRankForScore:
			return keyExpressionEquals(idx.RootExpression, fn.Operand)
		default:
			return false
		}
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
	// For BITMAP_VALUE indexes: delegate to bitmap-specific evaluation.
	if bm, ok := maintainer.(*bitmapValueIndexMaintainer); ok {
		return evaluateBitmapValueAggregate(ctx, bm, scanRange, isolationLevel)
	}

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
	if rm, ok := maintainer.(*rankIndexMaintainer); ok {
		return evaluateRankAggregate(fn, rm, scanRange)
	}

	// For TIME_WINDOW_LEADERBOARD aggregate functions.
	if lm, ok := maintainer.(*timeWindowLeaderboardIndexMaintainer); ok {
		return lm.EvaluateTimeWindowAggregate(fn, scanRange.Low)
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
	totalSize := fn.Operand.ColumnSize()
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
// reducing them. Used for COUNT, SUM, MIN_EVER_LONG, MAX_EVER_LONG, MAX_EVER_VERSION indexes.
// The maintainer must implement indexAggregator (identity + reducer live on the
// mutation type, not on string-name dispatch). This ensures adding a new atomic
// index type without implementing aggregation is a compile error, not a silent bug.
// Matches Java's AtomicMutationIndexMaintainer.evaluateAggregateFunction().
func evaluateAtomicAggregate(
	ctx context.Context,
	fn *IndexAggregateFunction,
	maintainer IndexMaintainer,
	scanRange TupleRange,
	isolationLevel IsolationLevel,
) (tuple.Tuple, error) {
	agg, ok := maintainer.(indexAggregator)
	if !ok {
		return nil, fmt.Errorf("index maintainer for %q does not support aggregation", fn.Name)
	}

	props := ScanProperties{
		ExecuteProperties: ExecuteProperties{
			IsolationLevel: isolationLevel,
		},
	}

	entries, err := AsList(ctx, maintainer.Scan(scanRange, nil, props))
	if err != nil {
		return nil, fmt.Errorf("evaluate %s aggregate: %w", fn.Name, err)
	}

	result := agg.aggregateIdentity()
	for _, e := range entries {
		result = agg.aggregate(result, e.Value)
	}
	return result, nil
}

// isGroupPrefix checks if the function operand is compatible with the index root.
// The grouped (aggregated) part must match structurally. The grouping (GROUP BY)
// part of the operand must be a structural prefix of the index's grouping part.
// Matches Java's IndexFunctionHelper.isGroupPrefix() which uses KeyExpression.equals()
// and isPrefixKey() for structural comparison (NOT field names).
func isGroupPrefix(operand KeyExpression, indexRoot KeyExpression) bool {
	// Fast path: full structural equality.
	if keyExpressionEquals(operand, indexRoot) {
		return true
	}
	// Compare grouped (aggregated) portions structurally.
	operandGrouped := getGroupedExprs(operand)
	indexGrouped := getGroupedExprs(indexRoot)
	if len(operandGrouped) != len(indexGrouped) {
		return false
	}
	for i := range operandGrouped {
		if !keyExpressionEquals(operandGrouped[i], indexGrouped[i]) {
			return false
		}
	}
	// Compare grouping (GROUP BY) portions: operand must be a prefix.
	operandGrouping := getGroupingExprs(operand)
	indexGrouping := getGroupingExprs(indexRoot)
	if len(operandGrouping) > len(indexGrouping) {
		return false
	}
	for i := range operandGrouping {
		if !keyExpressionEquals(operandGrouping[i], indexGrouping[i]) {
			return false
		}
	}
	return true
}

// isUngroupedPrefixOf checks if the operand's ungrouped (aggregated) part
// is a structural prefix of the index root expression. Used for VALUE index MIN/MAX.
// Matches Java's ValueIndexMaintainer.canEvaluateAggregateFunction().
func isUngroupedPrefixOf(operand KeyExpression, indexRoot KeyExpression) bool {
	operandExprs := normalizeKeyForPositions(operand)
	indexExprs := normalizeKeyForPositions(indexRoot)

	if len(operandExprs) > len(indexExprs) {
		return false
	}
	for i := range operandExprs {
		if !keyExpressionEquals(operandExprs[i], indexExprs[i]) {
			return false
		}
	}
	return true
}

// getGroupingExprs returns the per-column key expressions of the grouping (GROUP BY)
// part. Uses normalizeKeyForPositions for structural decomposition.
func getGroupingExprs(expr KeyExpression) []KeyExpression {
	if g, ok := expr.(*GroupingKeyExpression); ok {
		all := normalizeKeyForPositions(g.wholeKey)
		groupingCount := g.GetGroupingCount()
		if groupingCount <= len(all) {
			return all[:groupingCount]
		}
		return all
	}
	// Non-grouped expression: all columns are grouping
	return normalizeKeyForPositions(expr)
}

// getGroupedExprs returns the per-column key expressions of the grouped (aggregated)
// part. Uses normalizeKeyForPositions for structural decomposition.
func getGroupedExprs(expr KeyExpression) []KeyExpression {
	if g, ok := expr.(*GroupingKeyExpression); ok {
		all := normalizeKeyForPositions(g.wholeKey)
		groupingCount := g.GetGroupingCount()
		if groupingCount <= len(all) {
			return all[groupingCount:]
		}
		return nil
	}
	// Non-grouped expression: no grouped columns (everything is grouping)
	return nil
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
		return keyExpressionEquals(fn.Operand, idx.RootExpression)
	case FunctionNameCount:
		// COUNT on a unique RANK index where the operand covers only grouping columns.
		if !idx.IsUnique() {
			return false
		}
		groupingCount := 0
		if g, ok := idx.RootExpression.(*GroupingKeyExpression); ok {
			groupingCount = g.GetGroupingCount()
		}
		return fn.Operand.ColumnSize() == groupingCount &&
			isGroupPrefix(fn.Operand, idx.RootExpression)
	case FunctionNameScoreForRank, FunctionNameScoreForRankElseSkip, FunctionNameRankForScore:
		return keyExpressionEquals(fn.Operand, idx.RootExpression)
	default:
		return false
	}
}

// evaluateRankAggregate evaluates a RANK aggregate function using the ranked set.
// The scanRange must be an "equals" range (all Low == High values).
// Matches Java's RankIndexMaintainer.evaluateAggregateFunction().
func evaluateRankAggregate(
	fn *IndexAggregateFunction,
	rm *rankIndexMaintainer,
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
	rankedSet := newRankedSet(rankSubspace, rm.rankedSetConfig)

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

	// Prefetch sparse upper skip-list levels for Rank/GetNth calls below.
	rankedSet.PreloadForLookup(rm.tx)

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
		scoreTuple, err := fastUnpack(scoreBytes)
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
