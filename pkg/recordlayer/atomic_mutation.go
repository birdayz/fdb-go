package recordlayer

import (
	"encoding/binary"
	"fmt"
	"math"

	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"google.golang.org/protobuf/proto"
)

// atomicMutation defines the per-type behavior for atomic index maintainers.
// Matches Java's AtomicMutation interface — each variant parameterizes the
// unified atomicMutationIndexMaintainer with type-specific logic.
type atomicMutation interface {
	// evaluateEntries extracts mutation entries from a record.
	evaluateEntries(record *FDBStoredRecord[proto.Message]) ([]atomicMutationEntry, error)

	// removeCommon filters out common entries between old and new.
	removeCommon(old, new []atomicMutationEntry) ([]atomicMutationEntry, []atomicMutationEntry)

	// applyMutation applies the FDB mutation for a single entry.
	// remove=true for removal (old entries), false for insert (new entries).
	applyMutation(tx fdb.WritableTransaction, fdbKey fdb.Key, entry atomicMutationEntry, remove bool) error

	// isIdempotent returns whether this index type is idempotent under retry.
	isIdempotent() bool

	// deleteIsNoOp returns whether delete operations should be no-ops (_EVER semantics).
	deleteIsNoOp() bool

	// tupleValues returns whether scan cursor should decode values as tuple-packed bytes.
	tupleValues() bool

	// aggregateIdentity returns the initial value for reducing multiple index entries.
	// Matches Java's AtomicMutation.getIdentity().
	aggregateIdentity() tuple.Tuple

	// aggregate combines a running aggregate with a single scanned entry.
	// Matches Java's AtomicMutation.getAggregator().
	aggregate(accum, entry tuple.Tuple) tuple.Tuple
}

// atomicMutationEntry holds a grouping key and pre-computed mutation parameter bytes.
type atomicMutationEntry struct {
	groupKey tuple.Tuple
	param    []byte // FDB mutation parameter bytes (insert direction)
}

// --- COUNT mutation ---

type countMutation struct {
	index *Index
}

func (m *countMutation) evaluateEntries(record *FDBStoredRecord[proto.Message]) ([]atomicMutationEntry, error) {
	// Fast path: inline evaluation to skip evaluateGroupingKeys's []tuple.Tuple wrapper.
	if m.index.Predicate == nil || m.index.Predicate(record.Record) {
		if fe, ok := m.index.RootExpression.(FlatEvaluator); ok {
			values, err := fe.EvaluateFlat(record, record.Record)
			if err == nil {
				gc := indexGroupingCount(m.index.RootExpression)
				groupKey := make(tuple.Tuple, gc)
				for j := 0; j < gc && j < len(values); j++ {
					groupKey[j] = tuple.TupleElement(values[j])
				}
				return []atomicMutationEntry{{groupKey: groupKey, param: littleEndianInt64One}}, nil
			}
		}
	}
	keys, err := evaluateGroupingKeys(m.index, record)
	if err != nil {
		return nil, err
	}
	entries := make([]atomicMutationEntry, len(keys))
	for i, k := range keys {
		entries[i] = atomicMutationEntry{groupKey: k, param: littleEndianInt64One}
	}
	return entries, nil
}

func (m *countMutation) removeCommon(old, new []atomicMutationEntry) ([]atomicMutationEntry, []atomicMutationEntry) {
	return removeCommonAtomicByKey(old, new)
}

func (m *countMutation) applyMutation(tx fdb.WritableTransaction, fdbKey fdb.Key, entry atomicMutationEntry, remove bool) error {
	if remove {
		tx.AddBytes(fdbKey, littleEndianInt64MinusOne)
		if m.index.IsClearWhenZero() {
			tx.CompareAndClearBytes(fdbKey, littleEndianInt64Zero)
		}
	} else {
		tx.AddBytes(fdbKey, littleEndianInt64One)
	}
	return nil
}

func (m *countMutation) isIdempotent() bool             { return false }
func (m *countMutation) deleteIsNoOp() bool             { return false }
func (m *countMutation) tupleValues() bool              { return false }
func (m *countMutation) aggregateIdentity() tuple.Tuple { return tuple.Tuple{int64(0)} }
func (m *countMutation) aggregate(accum, entry tuple.Tuple) tuple.Tuple {
	return addAggregate(accum, entry)
}

// --- COUNT_NOT_NULL mutation ---

type countNotNullMutation struct {
	index *Index
}

func (m *countNotNullMutation) evaluateEntries(record *FDBStoredRecord[proto.Message]) ([]atomicMutationEntry, error) {
	keys, err := evaluateGroupingKeysNotNull(m.index, record)
	if err != nil {
		return nil, err
	}
	entries := make([]atomicMutationEntry, len(keys))
	for i, k := range keys {
		entries[i] = atomicMutationEntry{groupKey: k, param: littleEndianInt64One}
	}
	return entries, nil
}

func (m *countNotNullMutation) removeCommon(old, new []atomicMutationEntry) ([]atomicMutationEntry, []atomicMutationEntry) {
	return removeCommonAtomicByKey(old, new)
}

func (m *countNotNullMutation) applyMutation(tx fdb.WritableTransaction, fdbKey fdb.Key, entry atomicMutationEntry, remove bool) error {
	if remove {
		tx.AddBytes(fdbKey, littleEndianInt64MinusOne)
		if m.index.IsClearWhenZero() {
			tx.CompareAndClearBytes(fdbKey, littleEndianInt64Zero)
		}
	} else {
		tx.AddBytes(fdbKey, littleEndianInt64One)
	}
	return nil
}

func (m *countNotNullMutation) isIdempotent() bool             { return false }
func (m *countNotNullMutation) deleteIsNoOp() bool             { return false }
func (m *countNotNullMutation) tupleValues() bool              { return false }
func (m *countNotNullMutation) aggregateIdentity() tuple.Tuple { return tuple.Tuple{int64(0)} }
func (m *countNotNullMutation) aggregate(accum, entry tuple.Tuple) tuple.Tuple {
	return addAggregate(accum, entry)
}

// --- COUNT_UPDATES mutation ---

type countUpdatesMutation struct {
	index *Index
}

func (m *countUpdatesMutation) evaluateEntries(record *FDBStoredRecord[proto.Message]) ([]atomicMutationEntry, error) {
	keys, err := evaluateGroupingKeys(m.index, record)
	if err != nil {
		return nil, err
	}
	entries := make([]atomicMutationEntry, len(keys))
	for i, k := range keys {
		entries[i] = atomicMutationEntry{groupKey: k, param: littleEndianInt64One}
	}
	return entries, nil
}

func (m *countUpdatesMutation) removeCommon(old, new []atomicMutationEntry) ([]atomicMutationEntry, []atomicMutationEntry) {
	// COUNT_UPDATES: skipUpdateForUnchangedKeys = false — never skip.
	return old, new
}

func (m *countUpdatesMutation) applyMutation(tx fdb.WritableTransaction, fdbKey fdb.Key, _ atomicMutationEntry, remove bool) error {
	if remove {
		// COUNT_UPDATES: getMutationParam returns null for remove — no-op.
		return nil
	}
	tx.AddBytes(fdbKey, littleEndianInt64One)
	return nil
}

func (m *countUpdatesMutation) isIdempotent() bool             { return false }
func (m *countUpdatesMutation) deleteIsNoOp() bool             { return true }
func (m *countUpdatesMutation) tupleValues() bool              { return false }
func (m *countUpdatesMutation) aggregateIdentity() tuple.Tuple { return tuple.Tuple{int64(0)} }
func (m *countUpdatesMutation) aggregate(accum, entry tuple.Tuple) tuple.Tuple {
	return addAggregate(accum, entry)
}

// --- SUM mutation ---

type sumMutation struct {
	index *Index
}

func (m *sumMutation) evaluateEntries(record *FDBStoredRecord[proto.Message]) ([]atomicMutationEntry, error) {
	if m.index.Predicate != nil && !m.index.Predicate(record.Record) {
		return nil, nil
	}

	// Fast path: use EvaluateFlat to avoid [][]any alloc.
	// Falls through on error (e.g. fan-out repeated fields).
	if fe, ok := m.index.RootExpression.(FlatEvaluator); ok {
		values, err := fe.EvaluateFlat(record, record.Record)
		if err == nil {
			groupingCount := indexGroupingCount(m.index.RootExpression)
			if groupingCount >= len(values) {
				return nil, nil
			}
			groupKey := make(tuple.Tuple, groupingCount)
			for j := 0; j < groupingCount; j++ {
				groupKey[j] = values[j]
			}
			rawValue := values[groupingCount]
			if rawValue == nil {
				return nil, nil
			}
			sumValue, err := toInt64(rawValue)
			if err != nil {
				return nil, fmt.Errorf("sum index %q: value at column %d: %w", m.index.Name, groupingCount, err)
			}
			var param [8]byte
			binary.LittleEndian.PutUint64(param[:], uint64(sumValue))
			return []atomicMutationEntry{{groupKey: groupKey, param: param[:]}}, nil
		}
		// Fall through to standard Evaluate
	}

	tuples, err := m.index.RootExpression.Evaluate(record, record.Record)
	if err != nil {
		return nil, err
	}

	groupingCount := indexGroupingCount(m.index.RootExpression)
	var result []atomicMutationEntry
	for _, values := range tuples {
		groupKey := make(tuple.Tuple, groupingCount)
		for j := 0; j < groupingCount && j < len(values); j++ {
			groupKey[j] = values[j]
		}

		if groupingCount >= len(values) {
			continue
		}
		rawValue := values[groupingCount]
		if rawValue == nil {
			continue
		}

		sumValue, err := toInt64(rawValue)
		if err != nil {
			return nil, fmt.Errorf("sum index %q: value at column %d: %w", m.index.Name, groupingCount, err)
		}

		result = append(result, atomicMutationEntry{
			groupKey: groupKey,
			param:    encodeRecordCount(sumValue),
		})
	}
	return result, nil
}

func (m *sumMutation) removeCommon(old, new []atomicMutationEntry) ([]atomicMutationEntry, []atomicMutationEntry) {
	return removeCommonAtomicByKeyAndValue(old, new)
}

func (m *sumMutation) applyMutation(tx fdb.WritableTransaction, fdbKey fdb.Key, entry atomicMutationEntry, remove bool) error {
	if remove {
		val := int64(binary.LittleEndian.Uint64(entry.param))
		if val == math.MinInt64 {
			return fmt.Errorf("sum index %q overflow: cannot negate math.MinInt64", m.index.Name)
		}
		tx.AddBytes(fdbKey, encodeRecordCount(-val))
		if m.index.IsClearWhenZero() {
			tx.CompareAndClearBytes(fdbKey, littleEndianInt64Zero)
		}
	} else {
		tx.AddBytes(fdbKey, entry.param)
	}
	return nil
}

func (m *sumMutation) isIdempotent() bool             { return false }
func (m *sumMutation) deleteIsNoOp() bool             { return false }
func (m *sumMutation) tupleValues() bool              { return false }
func (m *sumMutation) aggregateIdentity() tuple.Tuple { return tuple.Tuple{int64(0)} }
func (m *sumMutation) aggregate(accum, entry tuple.Tuple) tuple.Tuple {
	return addAggregate(accum, entry)
}

// --- MIN_EVER_LONG / MAX_EVER_LONG mutation ---

type minMaxEverLongMutation struct {
	index *Index
	isMax bool
}

func (m *minMaxEverLongMutation) evaluateEntries(record *FDBStoredRecord[proto.Message]) ([]atomicMutationEntry, error) {
	if m.index.Predicate != nil && !m.index.Predicate(record.Record) {
		return nil, nil
	}

	tuples, err := m.index.RootExpression.Evaluate(record, record.Record)
	if err != nil {
		return nil, err
	}

	groupingCount := indexGroupingCount(m.index.RootExpression)
	var result []atomicMutationEntry
	for _, values := range tuples {
		groupKey := make(tuple.Tuple, groupingCount)
		for j := 0; j < groupingCount && j < len(values); j++ {
			groupKey[j] = values[j]
		}

		if groupingCount >= len(values) {
			continue
		}
		rawValue := values[groupingCount]
		if rawValue == nil {
			continue
		}

		val, err := toInt64(rawValue)
		if err != nil {
			return nil, fmt.Errorf("%s index %q: value at column %d: %w", m.index.Type, m.index.Name, groupingCount, err)
		}

		if val < 0 {
			return nil, fmt.Errorf("%s index %q: negative value %d not allowed for LONG variant", m.index.Type, m.index.Name, val)
		}

		result = append(result, atomicMutationEntry{
			groupKey: groupKey,
			param:    encodeRecordCount(val),
		})
	}
	return result, nil
}

func (m *minMaxEverLongMutation) removeCommon(old, new []atomicMutationEntry) ([]atomicMutationEntry, []atomicMutationEntry) {
	return old, new // Idempotent — no need to skip common entries.
}

func (m *minMaxEverLongMutation) applyMutation(tx fdb.WritableTransaction, fdbKey fdb.Key, entry atomicMutationEntry, remove bool) error {
	if remove {
		return nil // _EVER: deletes are no-ops.
	}
	if m.isMax {
		tx.MaxBytes(fdbKey, entry.param)
	} else {
		tx.MinBytes(fdbKey, entry.param)
	}
	return nil
}

func (m *minMaxEverLongMutation) isIdempotent() bool             { return true }
func (m *minMaxEverLongMutation) deleteIsNoOp() bool             { return true }
func (m *minMaxEverLongMutation) tupleValues() bool              { return false }
func (m *minMaxEverLongMutation) aggregateIdentity() tuple.Tuple { return nil }
func (m *minMaxEverLongMutation) aggregate(accum, entry tuple.Tuple) tuple.Tuple {
	if m.isMax {
		return maxAggregate(accum, entry)
	}
	return minAggregate(accum, entry)
}

// --- MIN_EVER_TUPLE / MAX_EVER_TUPLE mutation ---

type minMaxEverTupleMutation struct {
	index *Index
	isMax bool
}

func (m *minMaxEverTupleMutation) evaluateEntries(record *FDBStoredRecord[proto.Message]) ([]atomicMutationEntry, error) {
	if m.index.Predicate != nil && !m.index.Predicate(record.Record) {
		return nil, nil
	}

	tuples, err := m.index.RootExpression.Evaluate(record, record.Record)
	if err != nil {
		return nil, err
	}

	groupingCount := indexGroupingCount(m.index.RootExpression)
	var result []atomicMutationEntry
	for _, values := range tuples {
		groupKey := make(tuple.Tuple, groupingCount)
		for j := 0; j < groupingCount && j < len(values); j++ {
			groupKey[j] = values[j]
		}

		if groupingCount >= len(values) {
			continue
		}

		valueTuple := make(tuple.Tuple, len(values)-groupingCount)
		hasNull := false
		for j := groupingCount; j < len(values); j++ {
			if values[j] == nil {
				hasNull = true
				break
			}
			valueTuple[j-groupingCount] = values[j]
		}
		if hasNull {
			continue
		}

		result = append(result, atomicMutationEntry{
			groupKey: groupKey,
			param:    valueTuple.Pack(),
		})
	}
	return result, nil
}

func (m *minMaxEverTupleMutation) removeCommon(old, new []atomicMutationEntry) ([]atomicMutationEntry, []atomicMutationEntry) {
	return old, new // Idempotent — no need to skip common entries.
}

func (m *minMaxEverTupleMutation) applyMutation(tx fdb.WritableTransaction, fdbKey fdb.Key, entry atomicMutationEntry, remove bool) error {
	if remove {
		return nil // _EVER: deletes are no-ops.
	}
	if m.isMax {
		tx.ByteMax(fdbKey, entry.param)
	} else {
		tx.ByteMin(fdbKey, entry.param)
	}
	return nil
}

func (m *minMaxEverTupleMutation) isIdempotent() bool             { return true }
func (m *minMaxEverTupleMutation) deleteIsNoOp() bool             { return true }
func (m *minMaxEverTupleMutation) tupleValues() bool              { return true }
func (m *minMaxEverTupleMutation) aggregateIdentity() tuple.Tuple { return nil }
func (m *minMaxEverTupleMutation) aggregate(accum, entry tuple.Tuple) tuple.Tuple {
	if m.isMax {
		return maxAggregate(accum, entry)
	}
	return minAggregate(accum, entry)
}

// --- Shared removeCommon helpers ---

// removeCommonAtomicByKey removes entries with matching groupKey (ignoring param).
// Used by COUNT, COUNT_NOT_NULL.
func removeCommonAtomicByKey(old, new []atomicMutationEntry) ([]atomicMutationEntry, []atomicMutationEntry) {
	newSet := make(map[string]struct{}, len(new))
	for _, e := range new {
		newSet[string(e.groupKey.Pack())] = struct{}{}
	}

	common := make(map[string]struct{})
	var filteredOld []atomicMutationEntry
	for _, e := range old {
		p := string(e.groupKey.Pack())
		if _, ok := newSet[p]; ok {
			common[p] = struct{}{}
		} else {
			filteredOld = append(filteredOld, e)
		}
	}

	var filteredNew []atomicMutationEntry
	for _, e := range new {
		if _, ok := common[string(e.groupKey.Pack())]; !ok {
			filteredNew = append(filteredNew, e)
		}
	}

	return filteredOld, filteredNew
}

// removeCommonAtomicByKeyAndValue removes entries with matching groupKey AND param.
// Used by SUM (where the value matters for commonality check).
func removeCommonAtomicByKeyAndValue(old, new []atomicMutationEntry) ([]atomicMutationEntry, []atomicMutationEntry) {
	type entryKey struct {
		groupKey string
		param    string
	}

	newSet := make(map[entryKey]int, len(new))
	for _, e := range new {
		k := entryKey{groupKey: string(e.groupKey.Pack()), param: string(e.param)}
		newSet[k]++
	}

	common := make(map[entryKey]int)
	var filteredOld []atomicMutationEntry
	for _, e := range old {
		k := entryKey{groupKey: string(e.groupKey.Pack()), param: string(e.param)}
		if newSet[k] > common[k] {
			common[k]++
		} else {
			filteredOld = append(filteredOld, e)
		}
	}

	var filteredNew []atomicMutationEntry
	for _, e := range new {
		k := entryKey{groupKey: string(e.groupKey.Pack()), param: string(e.param)}
		if common[k] > 0 {
			common[k]--
		} else {
			filteredNew = append(filteredNew, e)
		}
	}

	return filteredOld, filteredNew
}

// --- Shared aggregate reducers ---
// Matches Java's AtomicMutation.Standard.getAggregator() implementations.

// addAggregate sums two int64 tuple values. Used by COUNT, SUM, COUNT_NOT_NULL, COUNT_UPDATES.
func addAggregate(accum, entry tuple.Tuple) tuple.Tuple {
	a, ok := accum[0].(int64)
	if !ok {
		return accum
	}
	b := int64(0)
	if len(entry) > 0 {
		bv, ok := entry[0].(int64)
		if !ok {
			return accum
		}
		b = bv
	}
	return tuple.Tuple{a + b}
}

// maxAggregate keeps the larger tuple by byte ordering. Used by MAX_EVER_LONG, MAX_EVER_TUPLE.
func maxAggregate(accum, entry tuple.Tuple) tuple.Tuple {
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

// minAggregate keeps the smaller tuple by byte ordering. Used by MIN_EVER_LONG, MIN_EVER_TUPLE.
func minAggregate(accum, entry tuple.Tuple) tuple.Tuple {
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
