package recordlayer

import (
	"errors"
	"fmt"

	"fdb.dev/gen"
	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
)

// indexingPendingWriteQueue uses the Java IndexingSubspaces layout. Queue
// capacity is a producer policy; replay and emptiness never trust the counter.
func (store *FDBRecordStore) indexingPendingWriteQueue(index *Index, maximum int64) *PendingWritesQueue[*gen.PendingWritesQueueEntry] {
	build := store.subspace.Sub(IndexBuildSpaceKey, index.SubspaceTupleKey())
	return NewPendingWritesQueue(build.Sub(int64(8)), build.Sub(int64(9)), maximum, &gen.PendingWritesQueueEntry{})
}

// isIndexPendingQueueEmpty includes entries not yet flushed at commit. A size
// counter cannot prove emptiness, and one handle's writes must block another
// handle's checked publication. The caller holds the shared maintenance gate.
func (store *FDBRecordStore) isIndexPendingQueueEmpty(index *Index) (bool, error) {
	queue := store.indexingPendingWriteQueue(index, 0)
	rangeKeys, err := fdb.PrefixRange(queue.entries.Bytes())
	if err != nil {
		return false, err
	}
	if store.context.HasVersionMutationsInRange(rangeKeys.Begin.FDBKey(), rangeKeys.End.FDBKey()) {
		return false, nil
	}
	return queue.IsQueueEmpty(store.context)
}

// replayPendingIndexWrite applies one committed entry before removing it in the
// same transaction. The caller owns commit/retry and continuation publication.
// Matches IndexingPendingWriteQueue.handleOneItem; builders call maintainers
// directly, so applying an entry must not route through store writer dispatch.
func (store *FDBRecordStore) replayPendingIndexWrite(index *Index, entry *PendingWritesQueueEntry[*gen.PendingWritesQueueEntry]) error {
	if entry == nil {
		return nil
	}
	payload := entry.Payload
	if payload == nil || payload.Operation == nil {
		return &RecordCoreStorageError{Message: "pending index write has no operation"}
	}
	maintainer, err := store.getIndexMaintainer(index)
	if err != nil {
		return err
	}
	switch payload.GetOperation() {
	case gen.PendingWritesQueueEntry_UPDATE:
		if err := maintainer.UpdateFromQueue(payload.GetData()); err != nil {
			return err
		}
	case gen.PendingWritesQueueEntry_DELETE_WHERE:
		var deletion gen.DeleteWhere
		if err := unmarshalPendingQueueAny(payload.GetData(), &deletion); err != nil {
			return &RecordCoreError{Message: "failed to parse pending write queue DELETE_WHERE entry data", Cause: err}
		}
		prefix, err := tuple.Unpack(deletion.GetPrefix())
		if err != nil {
			return err
		}
		if err := maintainer.DeleteWhere(prefix); err != nil {
			return err
		}
	default:
		return &RecordCoreStorageError{Message: "unknown pending index write operation"}
	}
	return store.indexingPendingWriteQueue(index, 0).ClearEntry(store.context, entry)
}

// PendingWriteQueueOptions corresponds to the context's Java pending queue
// properties. A nonpositive maximum disables the capacity check.
type PendingWriteQueueOptions struct {
	MaximumSize            int64
	DisableIndexOnOverflow bool
}

// SetPendingWriteQueueOptions replaces both context-scoped queue properties.
// Without an explicit value the Java defaults are 100000 entries and disable
// on overflow. The value is copied so concurrent callers cannot mutate it.
func (rc *FDBRecordContext) SetPendingWriteQueueOptions(options PendingWriteQueueOptions) {
	rc.pendingWriteQueueOptions.Store(&options)
}

func (rc *FDBRecordContext) PendingWriteQueueOptions() PendingWriteQueueOptions {
	if options := rc.pendingWriteQueueOptions.Load(); options != nil {
		return *options
	}
	return PendingWriteQueueOptions{MaximumSize: 100000, DisableIndexOnOverflow: true}
}

func pendingWriteCommitCheckPrefix(ss subspace.Subspace) string {
	return fmt.Sprintf("pendingIndexWrite:%x:", ss.Bytes())
}

func (store *FDBRecordStore) enqueuePendingIndexWrite(index *Index, entry *gen.PendingWritesQueueEntry) error {
	// Store dispatch already holds stateMu; do not recursively acquire its read
	// side, which deadlocks if a state transition is waiting for the write side.
	if err := store.requireFormatVersion("pending index writes", formatVersionPendingWrites); err != nil {
		return err
	}
	incarnation := store.storeHeader.GetIncarnation()
	options := store.context.PendingWriteQueueOptions()
	err := store.indexingPendingWriteQueue(index, options.MaximumSize).Enqueue(store.context, entry, incarnation)
	var overflow *PendingWritesQueueTooLargeError
	if !options.DisableIndexOnOverflow || !errors.As(err, &overflow) {
		return err
	}
	// Writer dispatch holds the shared state-read/maintenance gate. Disabling
	// acquires its write side, so defer it until commit rather than deadlocking.
	// Scope the name to the store as well as the index: contexts may share handles
	// for different stores with identically named indexes.
	name := pendingWriteCommitCheckPrefix(store.subspace) + "overflow:" + index.Name
	store.context.getOrCreateCommitCheck(name, func(string) CommitCheckFunc {
		return func() error {
			changed, err := store.MarkIndexDisabled(index.Name)
			if err == nil && changed {
				store.context.Timer().Increment(CountPendingWritesQueueOverflowDisabledIndex)
			}
			return err
		}
	})
	return nil
}

func (store *FDBRecordStore) validatePendingQueueIndex(index *Index) error {
	if err := store.requireFormatVersion("pending index writes", formatVersionPendingWrites); err != nil {
		return err
	}
	maintainer, err := store.getIndexMaintainer(index)
	if err != nil {
		return err
	}
	if !maintainer.IsPendingWriteQueueAllowed() || countVersionColumns(index.RootExpression) != 0 {
		return &RecordCoreError{Message: "index does not support queued writes", IndexName: index.Name}
	}
	return nil
}
