package recordlayer

import (
	"fmt"

	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"google.golang.org/protobuf/proto"
)

// Record function name constants matching Java's FunctionNames.
const (
	FunctionNameRank                   = "rank"
	FunctionNameTimeWindowRank         = "time_window_rank"
	FunctionNameTimeWindowRankAndEntry = "time_window_rank_and_entry"
)

// IndexRecordFunction specifies a function to evaluate on a record using an index.
// Matches Java's com.apple.foundationdb.record.metadata.IndexRecordFunction.
type IndexRecordFunction struct {
	Name    string        // Function name (e.g. "rank")
	Operand KeyExpression // The operand (typically a GroupingKeyExpression)
	Index   string        // Optional: explicit index name. Empty = auto-select.

	// TimeWindow specifies the leaderboard time window for TIME_WINDOW_RANK and
	// TIME_WINDOW_RANK_AND_ENTRY functions. Nil for plain RANK (uses all-time).
	// Matches Java's TimeWindowRecordFunction.timeWindow (TimeWindowForFunction).
	TimeWindow *TimeWindowForFunction
}

// TimeWindowForFunction specifies a leaderboard time window for record/aggregate functions.
// Matches Java's com.apple.foundationdb.record.provider.foundationdb.leaderboard.TimeWindowForFunction.
type TimeWindowForFunction struct {
	LeaderboardType      int
	LeaderboardTimestamp int64
}

// EvaluateRecordFunction evaluates an index record function for a specific record.
// For RANK indexes, this returns the rank of the record's score within its group.
// Returns nil if the record's value is not in the index (e.g. null field).
//
// Matches Java's FDBRecordStore.evaluateIndexRecordFunction().
func (store *FDBRecordStore) EvaluateRecordFunction(
	fn *IndexRecordFunction,
	record *FDBStoredRecord[proto.Message],
) (*int64, error) {
	index, err := store.findIndexForRecordFunction(fn, record)
	if err != nil {
		return nil, err
	}

	maintainer, err := store.getIndexMaintainer(index)
	if err != nil {
		return nil, err
	}
	return evaluateRecordFunction(fn, maintainer, record, index)
}

// findIndexForRecordFunction locates the best index that can evaluate the given
// record function. If fn.Index is set, uses that index directly. Otherwise,
// searches all READABLE indexes for the record's type.
// Matches Java's IndexFunctionHelper.indexMaintainerForRecordFunction().
func (store *FDBRecordStore) findIndexForRecordFunction(
	fn *IndexRecordFunction,
	record *FDBStoredRecord[proto.Message],
) (*Index, error) {
	if fn.Index != "" {
		idx := store.metaData.GetIndex(fn.Index)
		if idx == nil {
			return nil, fmt.Errorf("record function %q: %w", fn.Name, &IndexNotFoundError{IndexName: fn.Index})
		}
		if !store.IsIndexReadable(idx.Name) {
			return nil, fmt.Errorf("record function %q: %w", fn.Name, &IndexNotReadableError{IndexName: idx.Name, CurrentState: store.GetIndexState(idx.Name)})
		}
		return idx, nil
	}

	recordTypeName := record.RecordType.Name
	candidates := store.metaData.GetIndexesForRecordType(recordTypeName)

	var best *Index
	bestColSize := int(^uint(0) >> 1) // MaxInt

	for _, idx := range candidates {
		if !store.IsIndexReadable(idx.Name) {
			continue
		}
		if canEvaluateRecordFunction(fn, idx) {
			colSize := idx.RootExpression.ColumnSize()
			if colSize < bestColSize {
				best = idx
				bestColSize = colSize
			}
		}
	}

	if best == nil {
		return nil, fmt.Errorf("record function %q requires appropriate index on %s", fn.Name, recordTypeName)
	}
	return best, nil
}

// canEvaluateRecordFunction checks if an index can evaluate the given record function.
// Matches Java's RankIndexMaintainer.canEvaluateRecordFunction().
func canEvaluateRecordFunction(fn *IndexRecordFunction, idx *Index) bool {
	switch idx.Type {
	case IndexTypeRank:
		return fn.Name == FunctionNameRank &&
			keyExpressionEquals(idx.RootExpression, fn.Operand)
	case IndexTypeTimeWindowLeaderboard:
		return (fn.Name == FunctionNameRank ||
			fn.Name == FunctionNameTimeWindowRank ||
			fn.Name == FunctionNameTimeWindowRankAndEntry) &&
			keyExpressionEquals(idx.RootExpression, fn.Operand)
	default:
		return false
	}
}

// evaluateRecordFunction dispatches to the appropriate maintainer for evaluation.
func evaluateRecordFunction(
	fn *IndexRecordFunction,
	maintainer IndexMaintainer,
	record *FDBStoredRecord[proto.Message],
	index *Index,
) (*int64, error) {
	if rm, ok := maintainer.(*rankIndexMaintainer); ok {
		return rm.EvaluateRecordFunction(fn, record)
	}
	if lm, ok := maintainer.(*timeWindowLeaderboardIndexMaintainer); ok {
		return lm.EvaluateRecordFunction(fn, record)
	}
	return nil, fmt.Errorf("index %q (type %s) does not support record function %q", index.Name, index.Type, fn.Name)
}

// EvaluateRecordFunction evaluates a record function (e.g. "rank") for a specific record.
// For the "rank" function, returns the rank of the record's score in its group.
// Returns nil if the record's index key evaluates to null.
//
// Matches Java's RankIndexMaintainer.evaluateRecordFunction() → rank().
func (m *rankIndexMaintainer) EvaluateRecordFunction(
	fn *IndexRecordFunction,
	record *FDBStoredRecord[proto.Message],
) (*int64, error) {
	if fn.Name != FunctionNameRank {
		return nil, fmt.Errorf("RANK index does not support record function %q", fn.Name)
	}

	// Evaluate the index key expression against the record to get the score.
	// Matches Java's IndexFunctionHelper.recordFunctionIndexEntry() → evaluateSingleton().
	tuples, err := m.index.RootExpression.Evaluate(record, record.Record)
	if err != nil {
		return nil, err
	}
	if len(tuples) == 0 {
		return nil, nil
	}

	// Use the first (and typically only) evaluation result.
	indexKey := make(tuple.Tuple, len(tuples[0]))
	for i, v := range tuples[0] {
		indexKey[i] = v
	}

	// RankForScore already handles group prefix splitting.
	return m.RankForScore(indexKey, true)
}
