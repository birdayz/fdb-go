package recordlayer

import (
	"encoding/binary"
	"fmt"
	"unsafe"

	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb/subspace"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb/tuple"
	"google.golang.org/protobuf/proto"
)

// atomicMutationIndexMaintainer is the unified maintainer for all atomic index types.
// Matches Java's AtomicMutationIndexMaintainer — one class handles COUNT, SUM,
// MIN_EVER_LONG, MAX_EVER_LONG, MIN_EVER_TUPLE, MAX_EVER_TUPLE, COUNT_NOT_NULL,
// and COUNT_UPDATES via the atomicMutation strategy interface.
type atomicMutationIndexMaintainer struct {
	index         *Index
	indexSubspace subspace.Subspace
	tx            fdb.Transaction
	store         indexStoreContext
	mutation      atomicMutation
}

func newAtomicMutationIndexMaintainer(
	index *Index,
	indexSubspace subspace.Subspace,
	tx fdb.Transaction,
	store indexStoreContext,
	mutation atomicMutation,
) *atomicMutationIndexMaintainer {
	return &atomicMutationIndexMaintainer{
		index:         index,
		indexSubspace: indexSubspace,
		tx:            tx,
		store:         store,
		mutation:      mutation,
	}
}

// Update handles insert (old=nil), delete (new=nil), or update (both non-nil).
// Delegates evaluation, common-entry filtering, and mutation application to the
// atomicMutation strategy. Matches Java's AtomicMutationIndexMaintainer.updateIndexKeys().
func (m *atomicMutationIndexMaintainer) Update(oldRecord, newRecord *FDBStoredRecord[proto.Message]) error {
	// _EVER and COUNT_UPDATES: deletes are no-ops.
	if m.mutation.deleteIsNoOp() && newRecord == nil {
		return nil
	}

	// Insert-only fast path: evaluate + apply inline, no intermediate slices.
	// Saves 2 allocs per call ([]atomicMutationEntry + make(tuple.Tuple)).
	if oldRecord == nil && newRecord != nil {
		if ok, err := m.updateInsertOnly(newRecord); ok || err != nil {
			return err
		}
	}

	var oldEntries, newEntries []atomicMutationEntry

	// Evaluate old record entries (skip if delete is no-op).
	if oldRecord != nil && !m.mutation.deleteIsNoOp() {
		entries, err := m.mutation.evaluateEntries(oldRecord)
		if err != nil {
			return fmt.Errorf("evaluate %s index %q for old record: %w", m.index.Type, m.index.Name, err)
		}
		oldEntries = entries
	}

	// Evaluate new record entries.
	if newRecord != nil {
		entries, err := m.mutation.evaluateEntries(newRecord)
		if err != nil {
			return fmt.Errorf("evaluate %s index %q for new record: %w", m.index.Type, m.index.Name, err)
		}
		newEntries = entries
	}

	// Remove common entries — skipped for idempotent types and COUNT_UPDATES.
	if oldEntries != nil && newEntries != nil {
		oldEntries, newEntries = m.mutation.removeCommon(oldEntries, newEntries)
	}

	// Apply removal mutations for old entries.
	for _, e := range oldEntries {
		fdbKey := fdb.Key(m.indexSubspace.Pack(e.groupKey))
		if err := m.mutation.applyMutation(m.tx, fdbKey, e, true); err != nil {
			return err
		}
	}

	// Apply insert mutations for new entries (with size checks).
	for _, e := range newEntries {
		fdbKey := fdb.Key(m.indexSubspace.Pack(e.groupKey))
		if newRecord != nil {
			if err := checkKeyValueSizes(m.index, newRecord.PrimaryKey, fdbKey, e.param); err != nil {
				return err
			}
		}
		if err := m.mutation.applyMutation(m.tx, fdbKey, e, false); err != nil {
			return err
		}
	}

	return nil
}

// updateInsertOnly is the insert-only fast path for atomic index maintenance.
// Evaluates the expression and applies the mutation inline without allocating
// []atomicMutationEntry or make(tuple.Tuple). Returns (true, nil) on success,
// (false, nil) to fall through to the standard path.
func (m *atomicMutationIndexMaintainer) updateInsertOnly(newRecord *FDBStoredRecord[proto.Message]) (bool, error) {
	if m.index.Predicate != nil && !m.index.Predicate(newRecord.Record) {
		return true, nil // predicate skipped
	}

	// Try zero-alloc path: evaluate grouping key fields directly via ScalarEvaluator,
	// bypassing CompositeKeyExpression.EvaluateFlat's make([]any, 0, N).
	if gke, ok := m.index.RootExpression.(*GroupingKeyExpression); ok {
		if ok, err := m.updateInsertOnlyGrouped(gke, newRecord); ok || err != nil {
			return ok, err
		}
	}

	// Standard path: use EvaluateFlat (allocates []any for composite expressions).
	fe, ok := m.index.RootExpression.(FlatEvaluator)
	if !ok {
		return false, nil
	}

	values, err := fe.EvaluateFlat(newRecord, newRecord.Record)
	if err != nil {
		return false, nil
	}

	gc := indexGroupingCount(m.index.RootExpression)
	n := gc
	if n > len(values) {
		n = len(values)
	}
	groupKey := *(*tuple.Tuple)(unsafe.Pointer(&values))
	groupKey = groupKey[:n]
	var fdbKey fdb.Key
	if s, ok := m.store.(*FDBRecordStore); ok && s.batchKeyBuf != nil {
		fdbKey = fdb.Key(groupKey.PackWithPrefixInto(s.batchKeyBuf, m.indexSubspace.Bytes()))
	} else {
		fdbKey = fdb.Key(m.indexSubspace.Pack(groupKey))
	}

	if err := checkKeyValueSizes(m.index, newRecord.PrimaryKey, fdbKey, nil); err != nil {
		return true, err
	}

	var entry atomicMutationEntry
	entry.groupKey = groupKey

	switch m.mutation.(type) {
	case *countMutation, *countUpdatesMutation:
	case *sumMutation:
		if gc >= len(values) || values[gc] == nil {
			return true, nil
		}
		sumValue, err := toInt64(values[gc])
		if err != nil {
			return true, fmt.Errorf("sum index %q: value at column %d: %w", m.index.Name, gc, err)
		}
		var param [8]byte
		binary.LittleEndian.PutUint64(param[:], uint64(sumValue))
		entry.param = param[:]
	default:
		return false, nil
	}

	if err := m.mutation.applyMutation(m.tx, fdbKey, entry, false); err != nil {
		return true, err
	}
	return true, nil
}

// updateInsertOnlyGrouped handles the insert-only fast path for GroupingKeyExpression
// roots. Evaluates grouping key fields directly via ScalarEvaluator, bypassing
// CompositeKeyExpression.EvaluateFlat's make([]any) allocation.
// Returns (true, nil) on success, (false, nil) to fall through.
func (m *atomicMutationIndexMaintainer) updateInsertOnlyGrouped(
	gke *GroupingKeyExpression,
	newRecord *FDBStoredRecord[proto.Message],
) (bool, error) {
	gc := gke.GetGroupingCount()

	// Ungrouped (gc=0): all records share empty group key. No evaluation needed.
	// Only handles COUNT variants (no grouped value required). SUM with gc=0
	// falls through to the standard EvaluateFlat path.
	if gc == 0 {
		switch m.mutation.(type) {
		case *countMutation, *countUpdatesMutation:
			var fdbKey fdb.Key
			if s, ok := m.store.(*FDBRecordStore); ok && s.batchKeyBuf != nil {
				fdbKey = fdb.Key(tuple.Tuple{}.PackWithPrefixInto(s.batchKeyBuf, m.indexSubspace.Bytes()))
			} else {
				fdbKey = fdb.Key(m.indexSubspace.Pack(tuple.Tuple{}))
			}
			return m.applyInsertMutation(fdbKey, nil, gc, newRecord, gke)
		}
		return false, nil // SUM/other with gc=0: fall through
	}

	// Single-field grouping (gc=1): try ScalarEvaluator on first child.
	if gc == 1 {
		comp, ok := gke.wholeKey.(*CompositeKeyExpression)
		if !ok {
			return false, nil
		}
		if len(comp.expressions) == 0 {
			return false, nil
		}
		// Try Int64Evaluator for zero-alloc group key packing.
		var fdbKey fdb.Key
		if ie, ok := comp.expressions[0].(Int64Evaluator); ok {
			groupInt64, valid, err := ie.EvaluateInt64(newRecord, newRecord.Record)
			if err != nil {
				return false, nil
			}
			if !valid {
				return false, nil
			}
			if s, ok := m.store.(*FDBRecordStore); ok && s.batchKeyBuf != nil {
				fdbKey = fdb.Key(tuple.PackInt64Into(s.batchKeyBuf, m.indexSubspace.Bytes(), groupInt64))
			} else {
				fdbKey = fdb.Key(tuple.Pack1WithPrefix(m.indexSubspace.Bytes(), groupInt64))
			}
		} else if se, ok := comp.expressions[0].(ScalarEvaluator); ok {
			groupVal, err := se.EvaluateScalar(newRecord, newRecord.Record)
			if err != nil {
				return false, nil
			}
			if s, ok := m.store.(*FDBRecordStore); ok && s.batchKeyBuf != nil {
				fdbKey = fdb.Key(tuple.Pack1Into(s.batchKeyBuf, m.indexSubspace.Bytes(), tuple.TupleElement(groupVal)))
			} else {
				fdbKey = fdb.Key(tuple.Pack1WithPrefix(m.indexSubspace.Bytes(), tuple.TupleElement(groupVal)))
			}
		} else {
			return false, nil
		}

		// For SUM: extract the sum value and apply inline to avoid int64→any boxing.
		if _, isSumMut := m.mutation.(*sumMutation); isSumMut && len(comp.expressions) > 1 {
			var sumInt64 int64
			if ie, ok := comp.expressions[1].(Int64Evaluator); ok {
				val, valid, err := ie.EvaluateInt64(newRecord, newRecord.Record)
				if err != nil || !valid {
					return false, nil
				}
				sumInt64 = val
			} else if se2, ok := comp.expressions[1].(ScalarEvaluator); ok {
				val, err := se2.EvaluateScalar(newRecord, newRecord.Record)
				if err != nil || val == nil {
					return false, nil
				}
				sumInt64, err = toInt64(val)
				if err != nil {
					return true, fmt.Errorf("sum index %q: %w", m.index.Name, err)
				}
			} else {
				return false, nil
			}
			if err := checkKeyValueSizes(m.index, newRecord.PrimaryKey, fdbKey, nil); err != nil {
				return true, err
			}
			// Inline SUM mutation with stack-allocated param buffer.
			var sumParam [8]byte
			binary.LittleEndian.PutUint64(sumParam[:], uint64(sumInt64))
			m.tx.AddBytes(fdbKey, sumParam[:])
			return true, nil
		}

		return m.applyInsertMutation(fdbKey, nil, gc, newRecord, gke)
	}

	// Multi-field grouping (gc > 1): try DirectPacker on the grouping fields.
	comp, ok := gke.wholeKey.(*CompositeKeyExpression)
	if !ok || len(comp.expressions) < gc {
		return false, nil
	}
	// Use shared batch packer if available to avoid pool churn.
	var pk *tuple.Packer
	var ownedPk bool
	if s, ok2 := m.store.(*FDBRecordStore); ok2 && s.batchPacker != nil {
		pk = s.batchPacker
	} else {
		pk = tuple.GetPacker()
		ownedPk = true
	}
	pk.Reset()
	allDirect := true
	for i := 0; i < gc; i++ {
		if dp, ok2 := comp.expressions[i].(DirectPacker); ok2 {
			if !dp.PackDirect(pk, newRecord, newRecord.Record) {
				allDirect = false
				break
			}
		} else {
			allDirect = false
			break
		}
	}
	if !allDirect {
		if ownedPk {
			tuple.PutPacker(pk)
		}
		return false, nil
	}
	var fdbKey fdb.Key
	if s, ok2 := m.store.(*FDBRecordStore); ok2 && s.batchKeyBuf != nil {
		fdbKey = fdb.Key(pk.AppendInto(s.batchKeyBuf, m.indexSubspace.Bytes()))
	} else {
		var buf []byte
		fdbKey = fdb.Key(pk.AppendInto(&buf, m.indexSubspace.Bytes()))
	}
	if ownedPk {
		tuple.PutPacker(pk)
	}

	// Extract SUM value directly if this is a SUM mutation.
	var sumSource any
	if _, isSumMut := m.mutation.(*sumMutation); isSumMut && len(comp.expressions) > gc {
		sumExpr := comp.expressions[gc]
		if ie, ok2 := sumExpr.(Int64Evaluator); ok2 {
			val, valid, err := ie.EvaluateInt64(newRecord, newRecord.Record)
			if err != nil || !valid {
				return false, nil
			}
			if err := checkKeyValueSizes(m.index, newRecord.PrimaryKey, fdbKey, nil); err != nil {
				return true, err
			}
			var param [8]byte
			binary.LittleEndian.PutUint64(param[:], uint64(val))
			m.tx.AddBytes(fdbKey, param[:])
			return true, nil
		}
		if se, ok2 := sumExpr.(ScalarEvaluator); ok2 {
			val, err := se.EvaluateScalar(newRecord, newRecord.Record)
			if err != nil {
				return false, nil
			}
			sumSource = val
		} else {
			return false, nil
		}
	}

	return m.applyInsertMutation(fdbKey, sumSource, gc, newRecord, gke)
}

// applyInsertMutation applies the mutation for insert-only, given a pre-packed FDB key.
// sumSource is the raw value for SUM mutations (nil for COUNT).
func (m *atomicMutationIndexMaintainer) applyInsertMutation(
	fdbKey fdb.Key,
	sumSource any,
	gc int,
	newRecord *FDBStoredRecord[proto.Message],
	gke *GroupingKeyExpression,
) (bool, error) {
	if err := checkKeyValueSizes(m.index, newRecord.PrimaryKey, fdbKey, nil); err != nil {
		return true, err
	}

	var entry atomicMutationEntry
	switch m.mutation.(type) {
	case *countMutation, *countUpdatesMutation:
		// param ignored by applyMutation
	case *sumMutation:
		if sumSource == nil {
			return true, nil
		}
		sumValue, err := toInt64(sumSource)
		if err != nil {
			return true, fmt.Errorf("sum index %q: %w", m.index.Name, err)
		}
		var param [8]byte
		binary.LittleEndian.PutUint64(param[:], uint64(sumValue))
		entry.param = param[:]
	default:
		return false, nil
	}

	if err := m.mutation.applyMutation(m.tx, fdbKey, entry, false); err != nil {
		return true, err
	}
	return true, nil
}

// UpdateWhileWriteOnly updates the index during WRITE_ONLY state.
// For idempotent indexes (_EVER), pass-through to Update().
// For non-idempotent indexes (COUNT, SUM), checks the index build range first.
func (m *atomicMutationIndexMaintainer) UpdateWhileWriteOnly(oldRecord, newRecord *FDBStoredRecord[proto.Message]) error {
	if m.mutation.isIdempotent() {
		return m.Update(oldRecord, newRecord)
	}
	// Non-idempotent: short-circuit deletes for deleteIsNoOp types (COUNT_UPDATES).
	if m.mutation.deleteIsNoOp() && newRecord == nil {
		return nil
	}
	return updateWhileWriteOnlyNonIdempotent(oldRecord, newRecord, m.index, m.store, m.index.Type, m.Update)
}

// DeleteWhere clears all index entries whose key starts with the given prefix.
func (m *atomicMutationIndexMaintainer) DeleteWhere(prefix tuple.Tuple) error {
	return deleteWhereRange(m.tx, m.indexSubspace, prefix)
}

// Scan scans index entries within the given tuple range.
// Uses countKVCursor (int64 values) or tuple variant based on mutation type.
func (m *atomicMutationIndexMaintainer) Scan(scanRange TupleRange, continuation []byte, scanProperties ScanProperties) RecordCursor[*IndexEntry] {
	if m.mutation.tupleValues() {
		return newTupleValueIndexCursor(m.index, m.indexSubspace, m.tx, scanRange, continuation, scanProperties)
	}
	return newCountIndexCursor(m.index, m.indexSubspace, m.tx, scanRange, continuation, scanProperties)
}

// indexAggregator is implemented by index maintainers that support aggregate evaluation.
// Used by evaluateAtomicAggregate to get the reducer from the maintainer itself,
// eliminating the string-name dispatch. Matches Java's pattern where getIdentity()
// and getAggregator() live on the AtomicMutation enum.
type indexAggregator interface {
	aggregateIdentity() tuple.Tuple
	aggregate(accum, entry tuple.Tuple) tuple.Tuple
}

func (m *atomicMutationIndexMaintainer) aggregateIdentity() tuple.Tuple {
	return m.mutation.aggregateIdentity()
}

func (m *atomicMutationIndexMaintainer) aggregate(accum, entry tuple.Tuple) tuple.Tuple {
	return m.mutation.aggregate(accum, entry)
}

var (
	_ IndexMaintainer = (*atomicMutationIndexMaintainer)(nil)
	_ indexAggregator = (*atomicMutationIndexMaintainer)(nil)
)
