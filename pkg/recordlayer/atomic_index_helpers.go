package recordlayer

import (
	"fmt"

	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"google.golang.org/protobuf/proto"
)

// indexGroupingCount returns the number of grouping columns in an index expression.
// For GroupingKeyExpression, returns the explicit grouping count.
// For other expressions, all columns are grouping columns.
// Used by all atomic index maintainers (COUNT, SUM, COUNT_NOT_NULL, COUNT_UPDATES,
// MIN/MAX_EVER_LONG, MIN/MAX_EVER_TUPLE).
func indexGroupingCount(expr KeyExpression) int {
	if g, ok := expr.(*GroupingKeyExpression); ok {
		return g.GetGroupingCount()
	}
	return expr.ColumnSize()
}

// evaluateGroupingKeys extracts the grouping key tuple(s) from a record.
// For a GroupingKeyExpression, takes only the leading grouping columns.
// For other expressions, uses all columns as the grouping key.
// maintained is indexValuesFor's verdict on the record (the index predicate
// and the store's maintenance filter).
// Used by COUNT, COUNT_NOT_NULL, and COUNT_UPDATES maintainers.
func evaluateGroupingKeys(store indexStoreContext, index *Index, record *FDBStoredRecord[proto.Message], maintained IndexValues) ([]tuple.Tuple, error) {
	if maintained == IndexValuesNone {
		return nil, nil
	}

	groupingCount := indexGroupingCount(index.RootExpression)

	// Fast path: use EvaluateFlat to avoid [][]any alloc. It evaluates no
	// entry list, so it runs only when every entry is maintained.
	// Falls through on error (e.g. fan-out repeated fields).
	if fe, ok := index.RootExpression.(FlatEvaluator); ok && maintained == IndexValuesAll {
		values, err := fe.EvaluateFlat(record, record.Record)
		if err == nil {
			// Convert []any to tuple.Tuple (same underlying type)
			groupKey := make(tuple.Tuple, groupingCount)
			for j := 0; j < groupingCount && j < len(values); j++ {
				groupKey[j] = tuple.TupleElement(values[j])
			}
			return []tuple.Tuple{groupKey}, nil
		}
		// Fall through to standard Evaluate
	}

	tuples, err := maintainedKeyTuples(store, index, record, maintained)
	if err != nil {
		return nil, err
	}

	// Single tuple fast path
	if len(tuples) == 1 {
		values := tuples[0]
		groupKey := make(tuple.Tuple, groupingCount)
		for j := 0; j < groupingCount && j < len(values); j++ {
			groupKey[j] = values[j]
		}
		return []tuple.Tuple{groupKey}, nil
	}

	result := make([]tuple.Tuple, 0, len(tuples))
	for _, values := range tuples {
		groupKey := make(tuple.Tuple, groupingCount)
		for j := 0; j < groupingCount && j < len(values); j++ {
			groupKey[j] = values[j]
		}
		result = append(result, groupKey)
	}
	return result, nil
}

// updateWhileWriteOnlyNonIdempotent is Java's
// StandardIndexMaintainer.updateWhileWriteOnly for a non-idempotent index
// (StandardIndexMaintainer.java:255-328): a record is applied to an index under
// construction only where the build has already covered it, and what the build
// covers is the stamp's method's. With no stamp (the build has not started, or
// a by-records build that wrote none) or a by-records stamp, the range set
// holds primary keys (updateWriteOnlyByRecords). A BY_INDEX build's range set
// holds its source index's entry keys, so each record's key in that index
// decides (updateWriteOnlyByIndex). Used by the atomic indexes that are not
// idempotent, and by RANK and TIME_WINDOW_LEADERBOARD counting duplicates.
func updateWhileWriteOnlyNonIdempotent(
	oldRecord, newRecord *FDBStoredRecord[proto.Message],
	index *Index,
	store indexStoreContext,
	indexTypeName string,
	updateFunc func(*FDBStoredRecord[proto.Message], *FDBStoredRecord[proto.Message]) error,
) error {
	if oldRecord == nil && newRecord == nil {
		return nil
	}
	if store == nil {
		return updateFunc(oldRecord, newRecord)
	}
	inRange := func(key tuple.Tuple) (bool, error) {
		in, err := store.isKeyInIndexBuildRange(index, key)
		if err != nil {
			return false, fmt.Errorf("check index build range for %s index %q: %w", indexTypeName, index.Name, err)
		}
		return in, nil
	}
	source, err := store.writeOnlyBuildSource(index)
	if err != nil {
		return err
	}
	if source == nil {
		// updateWriteOnlyByRecords (:283-289).
		var primaryKey tuple.Tuple
		if oldRecord != nil {
			primaryKey = oldRecord.PrimaryKey
		} else {
			primaryKey = newRecord.PrimaryKey
		}
		in, err := inRange(primaryKey)
		if err != nil || !in {
			return err
		}
		return updateFunc(oldRecord, newRecord)
	}

	// updateWriteOnlyByIndex (:291-328).
	oldKey, err := store.sourceIndexEntryKey(source, oldRecord)
	if err != nil {
		return err
	}
	newKey, err := store.sourceIndexEntryKey(source, newRecord)
	if err != nil {
		return err
	}
	if oldKey != nil && newKey != nil {
		if tuplesEqual(oldKey, newKey) {
			in, err := inRange(oldKey)
			if err != nil || !in {
				return err
			}
			return updateFunc(oldRecord, newRecord)
		}
		// The two keys are checked before either update, as Java checks
		// them concurrently; an update does not move the range set.
		oldIn, err := inRange(oldKey)
		if err != nil {
			return err
		}
		newIn, err := inRange(newKey)
		if err != nil {
			return err
		}
		if oldIn {
			if err := updateFunc(oldRecord, nil); err != nil {
				return err
			}
		}
		if newIn {
			return updateFunc(nil, newRecord)
		}
		return nil
	}
	entryKey := oldKey
	if entryKey == nil {
		entryKey = newKey
	}
	if entryKey == nil {
		// Both records are excluded from the source index by its predicate or
		// the store's maintenance filter, so from this index too.
		return nil
	}
	in, err := inRange(entryKey)
	if err != nil || !in {
		return err
	}
	return updateFunc(oldRecord, newRecord)
}

// evaluateGroupingKeysNotNull extracts the grouping key tuple(s) from a record,
// filtering out any tuples where the GROUPED (trailing) columns contain a
// NullStandin.NULL.
// Used by COUNT_NOT_NULL maintainer.
//
// The GROUPED suffix, and only it. Java splits the evaluated entry before the
// null test — AtomicMutationIndexMaintainer.updateIndexKeys builds
// groupKey = subTuple(key, 0, groupPrefixSize) and
// groupedValue = subKey(groupPrefixSize, keySize), and passes only groupedValue
// to getMutationParam (AtomicMutationIndexMaintainer.java:141-146), which is
// where keyContainsNonUniqueNull runs (AtomicMutation.java:165-171). A null in a
// GROUP BY column is therefore a group, not a reason to drop the row; testing
// the whole key instead undercounts every group whose key contains a null, which
// is silent and reads as a correct-looking smaller number. Pinned by
// "counts a row whose GROUPING column is null when the grouped column is not" —
// the only spec here with a non-empty grouping prefix, which is the sole
// dimension on which the two readings differ.
//
// Note it decides nullness by EVALUATING the key expression, not by walking the
// proto structurally: a structural walk cannot see fan-out (a repeated field
// with no elements yields zero tuples, not a null one) and cannot tell a
// grouping column from a grouped one.
func evaluateGroupingKeysNotNull(store indexStoreContext, index *Index, record *FDBStoredRecord[proto.Message], maintained IndexValues) ([]tuple.Tuple, error) {
	tuples, err := maintainedKeyTuples(store, index, record, maintained)
	if err != nil {
		return nil, err
	}

	groupingCount := indexGroupingCount(index.RootExpression)
	totalColumns := index.RootExpression.ColumnSize()
	groupedCount := totalColumns - groupingCount

	// Only a NullStandin.NULL drops the row, as keyContainsNonUniqueNull
	// decides it: a null from a NULL_UNIQUE or NOT_NULL field, or a function's
	// plain null, is counted.
	var standins []bool
	result := make([]tuple.Tuple, 0, len(tuples))
	for _, values := range tuples {
		hasNull := false
		for i := groupingCount; i < len(values) && i < totalColumns; i++ {
			if values[i] != nil {
				continue
			}
			if standins == nil {
				standins = nonUniqueNullColumns(index.RootExpression)
			}
			if i < len(standins) && standins[i] {
				hasNull = true
				break
			}
		}
		if hasNull || (groupedCount > 0 && len(values) <= groupingCount) {
			continue
		}

		groupKey := make(tuple.Tuple, groupingCount)
		for j := 0; j < groupingCount && j < len(values); j++ {
			groupKey[j] = values[j]
		}
		result = append(result, groupKey)
	}
	return result, nil
}

// toInt64 converts a numeric value (from proto field evaluation) to int64.
// Handles int64, int32, float64, float32 matching Java's Number.longValue().
func toInt64(v any) (int64, error) {
	switch n := v.(type) {
	case int64:
		return n, nil
	case int32:
		return int64(n), nil
	case int:
		return int64(n), nil
	case float64:
		return int64(n), nil
	case float32:
		return int64(n), nil
	default:
		return 0, fmt.Errorf("cannot convert %T to int64 for atomic index", v)
	}
}
