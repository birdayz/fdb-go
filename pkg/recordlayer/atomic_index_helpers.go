package recordlayer

import (
	"fmt"

	"github.com/apple/foundationdb/bindings/go/src/fdb/tuple"
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
	return keyExpressionColumnSize(expr)
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

	tuples, err := index.RootExpression.Evaluate(record, record.Record)
	if err != nil {
		return nil, err
	}

	groupingCount := indexGroupingCount(index.RootExpression)
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
