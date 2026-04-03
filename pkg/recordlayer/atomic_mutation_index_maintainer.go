package recordlayer

import (
	"fmt"

	"github.com/apple/foundationdb/bindings/go/src/fdb"
	"github.com/apple/foundationdb/bindings/go/src/fdb/subspace"
	"github.com/apple/foundationdb/bindings/go/src/fdb/tuple"
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
