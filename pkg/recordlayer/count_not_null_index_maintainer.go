package recordlayer

import (
	"fmt"

	"github.com/apple/foundationdb/bindings/go/src/fdb"
	"github.com/apple/foundationdb/bindings/go/src/fdb/subspace"
	"github.com/apple/foundationdb/bindings/go/src/fdb/tuple"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// CountNotNullIndexMaintainer handles COUNT_NOT_NULL index maintenance using FDB atomic ADD.
// Like COUNT, but skips entries where the index key contains a null (nil) element.
// Key format: [indexSubspace].pack(groupingTuple)
// Value format: little-endian int64 count
// Matches Java's AtomicMutationIndexMaintainer with COUNT_NOT_NULL mutation.
type CountNotNullIndexMaintainer struct {
	index         *Index
	indexSubspace subspace.Subspace
	tx            fdb.Transaction
	store         indexStoreContext
}

func newCountNotNullIndexMaintainer(index *Index, indexSubspace subspace.Subspace, tx fdb.Transaction, store indexStoreContext) *CountNotNullIndexMaintainer {
	return &CountNotNullIndexMaintainer{
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
func (m *CountNotNullIndexMaintainer) Update(oldRecord, newRecord *FDBStoredRecord[proto.Message]) error {
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
func (m *CountNotNullIndexMaintainer) UpdateWhileWriteOnly(oldRecord, newRecord *FDBStoredRecord[proto.Message]) error {
	var primaryKey tuple.Tuple
	if oldRecord != nil {
		primaryKey = oldRecord.PrimaryKey
	} else if newRecord != nil {
		primaryKey = newRecord.PrimaryKey
	} else {
		return nil
	}

	if m.store == nil {
		return m.Update(oldRecord, newRecord)
	}

	inRange, err := m.store.isKeyInIndexBuildRange(m.index, primaryKey)
	if err != nil {
		return fmt.Errorf("check index build range for COUNT_NOT_NULL index %q: %w", m.index.Name, err)
	}

	if !inRange {
		return nil
	}

	return m.Update(oldRecord, newRecord)
}

// Scan scans COUNT_NOT_NULL index entries within the given tuple range.
// Reuses countKVCursor — identical wire format (little-endian int64 values).
func (m *CountNotNullIndexMaintainer) Scan(scanRange TupleRange, continuation []byte, scanProperties ScanProperties) RecordCursor[*IndexEntry] {
	return newCountIndexCursor(m.index, m.indexSubspace, m.tx, scanRange, continuation, scanProperties)
}

// evaluateGroupingKeys extracts the grouping key tuple(s) from a record,
// filtering out any tuples where the source fields contain null (unset) values.
// Matches Java's COUNT_NOT_NULL behavior: getMutationParam() returns null
// when entry.keyContainsNonUniqueNull() is true.
func (m *CountNotNullIndexMaintainer) evaluateGroupingKeys(record *FDBStoredRecord[proto.Message]) ([]tuple.Tuple, error) {
	if m.index.Predicate != nil && !m.index.Predicate(record.Record) {
		return nil, nil
	}

	// Check if the record has any null (unset) fields in the key expression.
	// If so, skip this record entirely — matching Java's keyContainsNonUniqueNull().
	if keyExpressionHasNullField(record.Record, m.index.RootExpression) {
		return nil, nil
	}

	tuples, err := m.index.RootExpression.Evaluate(record.Record)
	if err != nil {
		return nil, err
	}

	groupingCount := m.getGroupingCount()
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
	case *GroupingKeyExpression:
		return keyExpressionHasNullField(msg, e.wholeKey)
	case *EmptyKeyExpression:
		return false
	default:
		return false
	}
}

func (m *CountNotNullIndexMaintainer) getGroupingCount() int {
	if g, ok := m.index.RootExpression.(*GroupingKeyExpression); ok {
		return g.GetGroupingCount()
	}
	return keyExpressionColumnSize(m.index.RootExpression)
}

var _ IndexMaintainer = (*CountNotNullIndexMaintainer)(nil)
