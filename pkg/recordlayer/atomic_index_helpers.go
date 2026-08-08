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
// Checks the index predicate first (sparse/filtered indexes).
// Used by COUNT, COUNT_NOT_NULL, and COUNT_UPDATES maintainers.
func evaluateGroupingKeys(index *Index, record *FDBStoredRecord[proto.Message]) ([]tuple.Tuple, error) {
	if index.Predicate != nil && !index.Predicate(record.Record) {
		return nil, nil
	}

	groupingCount := indexGroupingCount(index.RootExpression)

	// Fast path: use EvaluateFlat to avoid [][]any alloc.
	// Falls through on error (e.g. fan-out repeated fields).
	if fe, ok := index.RootExpression.(FlatEvaluator); ok {
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

	tuples, err := index.RootExpression.Evaluate(record, record.Record)
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

// updateWhileWriteOnlyNonIdempotent implements the common UpdateWhileWriteOnly
// pattern for non-idempotent atomic indexes (COUNT, SUM, COUNT_NOT_NULL, COUNT_UPDATES).
// Checks if the record's primary key is in the already-built range before delegating
// to the actual Update function.
// Matches Java's StandardIndexMaintainer.updateWriteOnlyByRecords().
func updateWhileWriteOnlyNonIdempotent(
	oldRecord, newRecord *FDBStoredRecord[proto.Message],
	index *Index,
	store indexStoreContext,
	indexTypeName string,
	updateFunc func(*FDBStoredRecord[proto.Message], *FDBStoredRecord[proto.Message]) error,
) error {
	var primaryKey tuple.Tuple
	if oldRecord != nil {
		primaryKey = oldRecord.PrimaryKey
	} else if newRecord != nil {
		primaryKey = newRecord.PrimaryKey
	} else {
		return nil
	}

	if store == nil {
		return updateFunc(oldRecord, newRecord)
	}

	inRange, err := store.isKeyInIndexBuildRange(index, primaryKey)
	if err != nil {
		return fmt.Errorf("check index build range for %s index %q: %w", indexTypeName, index.Name, err)
	}

	if !inRange {
		return nil
	}

	return updateFunc(oldRecord, newRecord)
}

// evaluateGroupingKeysNotNull extracts the grouping key tuple(s) from a record,
// filtering out any tuples where the GROUPED (trailing) columns contain null values.
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
func evaluateGroupingKeysNotNull(index *Index, record *FDBStoredRecord[proto.Message]) ([]tuple.Tuple, error) {
	if index.Predicate != nil && !index.Predicate(record.Record) {
		return nil, nil
	}

	tuples, err := index.RootExpression.Evaluate(record, record.Record)
	if err != nil {
		return nil, err
	}

	groupingCount := indexGroupingCount(index.RootExpression)
	totalColumns := index.RootExpression.ColumnSize()
	groupedCount := totalColumns - groupingCount

	result := make([]tuple.Tuple, 0, len(tuples))
	for _, values := range tuples {
		hasNull := false
		for i := groupingCount; i < len(values) && i < totalColumns; i++ {
			if values[i] == nil {
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
