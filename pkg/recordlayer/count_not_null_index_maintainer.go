package recordlayer

import (
	"fmt"

	"github.com/apple/foundationdb/bindings/go/src/fdb"
	"github.com/apple/foundationdb/bindings/go/src/fdb/subspace"
	"github.com/apple/foundationdb/bindings/go/src/fdb/tuple"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// countNotNullIndexMaintainer handles COUNT_NOT_NULL index maintenance using FDB atomic ADD.
// Like COUNT, but skips entries where the index key contains a null (nil) element.
// Key format: [indexSubspace].pack(groupingTuple)
// Value format: little-endian int64 count
// Matches Java's AtomicMutationIndexMaintainer with COUNT_NOT_NULL mutation.
type countNotNullIndexMaintainer struct {
	index         *Index
	indexSubspace subspace.Subspace
	tx            fdb.Transaction
	store         indexStoreContext
}

func newCountNotNullIndexMaintainer(index *Index, indexSubspace subspace.Subspace, tx fdb.Transaction, store indexStoreContext) *countNotNullIndexMaintainer {
	return &countNotNullIndexMaintainer{
		index:         index,
		indexSubspace: indexSubspace,
		tx:            tx,
		store:         store,
	}
}

// Update handles insert (old=nil), delete (new=nil), or update (both non-nil).
// For inserts: atomically adds +1 for each non-null grouping key entry.
// For deletes: atomically adds -1 for each non-null grouping key entry.
// For updates: decrements old non-null keys, increments new non-null keys.
// Entries with nil key elements are skipped entirely.
// Matches Java's AtomicMutationIndexMaintainer.updateIndexKeys() with COUNT_NOT_NULL.
func (m *countNotNullIndexMaintainer) Update(oldRecord, newRecord *FDBStoredRecord[proto.Message]) error {
	var oldKeys, newKeys []tuple.Tuple

	if oldRecord != nil {
		entries, err := m.evaluateGroupingKeys(oldRecord)
		if err != nil {
			return fmt.Errorf("evaluate count_not_null index %q for old record: %w", m.index.Name, err)
		}
		oldKeys = entries
	}

	if newRecord != nil {
		entries, err := m.evaluateGroupingKeys(newRecord)
		if err != nil {
			return fmt.Errorf("evaluate count_not_null index %q for new record: %w", m.index.Name, err)
		}
		newKeys = entries
	}

	// Skip common grouping keys on update — matches Java's skipUpdateForUnchangedKeys() = true.
	if oldKeys != nil && newKeys != nil {
		oldKeys, newKeys = removeCommonGroupingKeys(oldKeys, newKeys)
	}

	clearWhenZero := m.index.IsClearWhenZero()

	for _, key := range oldKeys {
		fdbKey := m.indexSubspace.Pack(key)
		m.tx.Add(fdb.Key(fdbKey), littleEndianInt64MinusOne)
		if clearWhenZero {
			m.tx.CompareAndClear(fdb.Key(fdbKey), littleEndianInt64Zero)
		}
	}

	for _, key := range newKeys {
		fdbKey := m.indexSubspace.Pack(key)
		if newRecord != nil {
			if err := checkKeyValueSizes(m.index, newRecord.PrimaryKey, fdbKey, littleEndianInt64One); err != nil {
				return err
			}
		}
		m.tx.Add(fdb.Key(fdbKey), littleEndianInt64One)
	}

	return nil
}

// UpdateWhileWriteOnly checks the index build range set before updating.
// COUNT_NOT_NULL is non-idempotent — blindly updating would cause double-counting.
// Matches Java's StandardIndexMaintainer.updateWriteOnlyByRecords().
func (m *countNotNullIndexMaintainer) UpdateWhileWriteOnly(oldRecord, newRecord *FDBStoredRecord[proto.Message]) error {
	return updateWhileWriteOnlyNonIdempotent(oldRecord, newRecord, m.index, m.store, "COUNT_NOT_NULL", m.Update)
}

// DeleteWhere clears all COUNT_NOT_NULL index entries whose key starts with the given prefix.
func (m *countNotNullIndexMaintainer) DeleteWhere(prefix tuple.Tuple) error {
	return deleteWhereRange(m.tx, m.indexSubspace, prefix)
}

// Scan scans COUNT_NOT_NULL index entries within the given tuple range.
// Reuses countKVCursor — identical wire format (little-endian int64 values).
func (m *countNotNullIndexMaintainer) Scan(scanRange TupleRange, continuation []byte, scanProperties ScanProperties) RecordCursor[*IndexEntry] {
	return newCountIndexCursor(m.index, m.indexSubspace, m.tx, scanRange, continuation, scanProperties)
}

// evaluateGroupingKeys extracts the grouping key tuple(s) from a record,
// filtering out any tuples where the GROUPED (trailing) columns contain null values.
// Java's AtomicMutationIndexMaintainer.updateIndexKeys() splits each evaluated entry
// into groupKey and groupedValue, then passes ONLY groupedValue to getMutationParam().
// COUNT_NOT_NULL's getMutationParam() calls keyContainsNonUniqueNull() on the grouped
// portion only — NOT the grouping (leading) columns.
func (m *countNotNullIndexMaintainer) evaluateGroupingKeys(record *FDBStoredRecord[proto.Message]) ([]tuple.Tuple, error) {
	if m.index.Predicate != nil && !m.index.Predicate(record.Record) {
		return nil, nil
	}

	tuples, err := m.index.RootExpression.Evaluate(record, record.Record)
	if err != nil {
		return nil, err
	}

	groupingCount := indexGroupingCount(m.index.RootExpression)
	totalColumns := keyExpressionColumnSize(m.index.RootExpression)
	groupedCount := totalColumns - groupingCount

	result := make([]tuple.Tuple, 0, len(tuples))
	for _, values := range tuples {
		// Check only the grouped (trailing) columns for null.
		// Grouping columns can be null — we still count the entry.
		hasNull := false
		for i := groupingCount; i < len(values) && i < totalColumns; i++ {
			if values[i] == nil {
				hasNull = true
				break
			}
		}
		// Also skip if grouped portion is entirely missing (truncated tuple).
		if hasNull || (groupedCount > 0 && len(values) <= groupingCount) {
			continue
		}

		// Extract only the grouping (leading) columns as the key.
		groupKey := make(tuple.Tuple, groupingCount)
		for j := 0; j < groupingCount && j < len(values); j++ {
			groupKey[j] = values[j]
		}
		result = append(result, groupKey)
	}
	return result, nil
}

// keyExpressionHasNullField checks if evaluating a key expression against a message
// would involve any unset (null) proto fields. This is used by COUNT_NOT_NULL to skip
// entries where the key contains NullStandin.NULL.
// Matches Java's IndexEntry.keyContainsNonUniqueNull().
func keyExpressionHasNullField(msg proto.Message, expr KeyExpression) bool {
	if msg == nil {
		return true
	}
	switch e := expr.(type) {
	case *FieldKeyExpression:
		m := msg.ProtoReflect()
		fd := m.Descriptor().Fields().ByName(protoreflect.Name(e.fieldName))
		if fd == nil {
			return true // Field not found in schema — treat as null
		}
		// For proto2 optional fields, check if explicitly set
		if fd.HasPresence() && !m.Has(fd) {
			return true
		}
		return false
	case *CompositeKeyExpression:
		for _, child := range e.expressions {
			if keyExpressionHasNullField(msg, child) {
				return true
			}
		}
		return false
	case *NestingKeyExpression:
		// Navigate into the nested message field, then check child expression.
		// Matches Java's NestingKeyExpression null handling.
		m := msg.ProtoReflect()
		fd := m.Descriptor().Fields().ByName(protoreflect.Name(e.parentField))
		if fd == nil {
			return true // Field not in schema — treat as null
		}
		if fd.HasPresence() && !m.Has(fd) {
			return true // Parent message field not set
		}
		if fd.Kind() == protoreflect.MessageKind {
			nestedMsg := m.Get(fd).Message().Interface()
			return keyExpressionHasNullField(nestedMsg, e.child)
		}
		return false
	case *GroupingKeyExpression:
		return keyExpressionHasNullField(msg, e.wholeKey)
	case *EmptyKeyExpression:
		return false
	default:
		return false
	}
}


var _ IndexMaintainer = (*countNotNullIndexMaintainer)(nil)
