package recordlayer

import (
	"fmt"

	"google.golang.org/protobuf/proto"
)

type partitionPreparedIndexUpdate struct {
	index      *Index
	state      IndexState
	maintainer IndexMaintainer
	value      *StandardIndexMaintainer
	old, new   []EvaluatedIndexEntry
}

// VALUE callbacks run exactly once per image, before any removal. Retain their
// entries for application; checkUniqueness itself is not a pure preflight for a
// WRITE_ONLY index because it writes paired violation annotations.
func (store *FDBRecordStore) preparePartitionUpdateIndexes(oldRecord, newRecord *FDBStoredRecord[proto.Message]) ([]partitionPreparedIndexUpdate, error) {
	if err := store.ensureStoreStateLoadedErr(); err != nil {
		return nil, err
	}
	if _, err := store.context.Transaction().GetReadVersion().Get(); err != nil {
		return nil, err
	}
	var prepared []partitionPreparedIndexUpdate
	for _, index := range store.metaData.GetIndexesForRecordType(newRecord.RecordType.Name) {
		state, err := store.readIndexState(index.Name)
		if err != nil {
			return nil, err
		}
		if state.IsDisabled() {
			prepared = append(prepared, partitionPreparedIndexUpdate{index: index, state: state})
			continue
		}
		if state.IsWriteOnlyWithQueue() {
			return nil, &PartitionMetadataError{RecordType: newRecord.RecordType.Name, IndexName: index.Name, Message: "partition update does not support pending index queues"}
		}
		maintainer, err := store.getIndexMaintainer(index)
		if err != nil {
			return nil, err
		}
		update := partitionPreparedIndexUpdate{index: index, state: state, maintainer: maintainer}
		if value, ok := maintainer.(*StandardIndexMaintainer); ok {
			update.value = value
			update.old, err = value.FilteredIndexEntries(oldRecord)
			if err != nil {
				return nil, err
			}
			update.new, err = value.FilteredIndexEntries(newRecord)
			if err != nil {
				return nil, err
			}
			update.old, update.new, err = RemoveCommonEntries(index, update.old, update.new)
			if err != nil {
				return nil, err
			}
			for _, entry := range update.new {
				key, err := indexEntryKey(index, entry.key, entry.primaryKey)
				if err != nil {
					return nil, err
				}
				if err := checkKeyValueSizes(index, entry.primaryKey, value.indexSubspace.Pack(key), entry.value.Pack()); err != nil {
					return nil, err
				}
				if !index.IsUnique() || keyContainsNonUniqueNull(index.RootExpression, entry.key) {
					continue
				}
				conflict, err := value.uniquenessConflict(entry, update.old)
				if err != nil {
					return nil, err
				}
				if conflict != nil && !state.IsWriteOnly() {
					return nil, &RecordIndexUniquenessViolationError{IndexName: index.Name, IndexKey: entry.key, PrimaryKey: entry.primaryKey, ExistingKey: conflict}
				}
			}
		}
		prepared = append(prepared, update)
	}
	return prepared, nil
}

// Scheduling follows updateOneIndex: disabled/queue handling is decided before
// effects, READABLE vs WRITE_ONLY calls retain their normal policy and session
// tracking. Changed-PK TEXT uses separate legs so its tokenizer-version key is
// deleted at OLD and written at NEW, even when the tokenizer version is equal.
func (store *FDBRecordStore) applyPartitionUpdateIndexes(prepared []partitionPreparedIndexUpdate, oldRecord, newRecord *FDBStoredRecord[proto.Message], changed bool) error {
	for _, update := range prepared {
		text, isText := update.maintainer.(*textIndexMaintainer)
		var textUpdate partitionPreparedTextUpdate
		if isText {
			var err error
			textUpdate, err = text.preparePartitionUpdate(oldRecord, newRecord, changed)
			if err != nil {
				return err
			}
		}
		err := func() error {
			store.stateMu.RLock()
			defer store.stateMu.RUnlock()
			store.indexStateView.maintenance.RLock()
			defer store.indexStateView.maintenance.RUnlock()
			state, err := store.indexStateView.read(store.context.Transaction(), update.index.Name)
			if err != nil {
				return err
			}
			if state != update.state {
				return &RecordCoreError{Message: fmt.Sprintf("index %q state changed during partition update", update.index.Name)}
			}
			if state.IsDisabled() {
				return nil
			}
			if update.value != nil {
				err = update.value.applyEvaluatedEntries(update.old, update.new)
			} else if isText {
				err = text.applyPartitionUpdate(oldRecord, newRecord, changed, textUpdate)
			} else if state.IsWriteOnlyNoQueue() {
				err = update.maintainer.UpdateWhileWriteOnly(oldRecord, newRecord)
			} else {
				err = update.maintainer.Update(oldRecord, newRecord)
			}
			if err != nil {
				return err
			}
			if state.IsWriteOnlyNoQueue() {
				store.context.addToSessionSet(WriteOnlyIndexesUpdated, update.index.Name)
			} else {
				store.context.addToSessionSet(ReadableIndexesUpdated, update.index.Name)
			}
			return nil
		}()
		if err != nil {
			return err
		}
	}
	return nil
}

type partitionPreparedTextUpdate struct {
	old, new         []preparedTextIndexKey
	replaceTokenizer bool
}

// Unlike an ordinary TEXT update, a move must permit reentrant writers from
// filters/tokenizers. Run them before taking index/state maintenance locks;
// apply only their retained keys and positions under the TEXT bunch lock.
func (m *textIndexMaintainer) preparePartitionUpdate(oldRecord, newRecord *FDBStoredRecord[proto.Message], changed bool) (partitionPreparedTextUpdate, error) {
	var prepared partitionPreparedTextUpdate
	oldEntries, oldRaw, err := m.filteredIndexEntries(oldRecord)
	if err != nil {
		return prepared, err
	}
	newEntries, newRaw, err := m.filteredIndexEntries(newRecord)
	if err != nil {
		return prepared, err
	}
	oldVersion, err := m.getRecordTokenizerVersion(oldRecord.PrimaryKey)
	if err != nil {
		return prepared, err
	}
	prepared.replaceTokenizer = changed || oldVersion != m.tokenizerVersion
	if !prepared.replaceTokenizer {
		oldEntries, newEntries, err = RemoveCommonEntries(m.index, oldEntries, newEntries)
		if err != nil {
			return prepared, err
		}
		oldRaw, newRaw = indexEntriesToRaw(oldEntries), indexEntriesToRaw(newEntries)
	}
	prepare := func(record *FDBStoredRecord[proto.Message], entries [][]any, version int) ([]preparedTextIndexKey, error) {
		keys := make([]preparedTextIndexKey, 0, len(entries))
		for _, entry := range entries {
			key, err := m.prepareOneKey(record, entry, textFieldPosition(m.index.RootExpression), version, nil)
			if err != nil {
				return nil, err
			}
			if len(key.positions) != 0 {
				// A custom tokenizer may reuse its result map/slices on its next
				// call. Retain our own image before invoking any other callback.
				positions := make(map[string][]int, len(key.positions))
				for token, values := range key.positions {
					positions[token] = append([]int(nil), values...)
				}
				key.positions = positions
				keys = append(keys, key)
			}
		}
		return keys, nil
	}
	prepared.old, err = prepare(oldRecord, oldRaw, oldVersion)
	if err != nil {
		return prepared, err
	}
	prepared.new, err = prepare(newRecord, newRaw, m.tokenizerVersion)
	return prepared, err
}

func (m *textIndexMaintainer) applyPartitionUpdate(oldRecord, newRecord *FDBStoredRecord[proto.Message], changed bool, prepared partitionPreparedTextUpdate) error {
	lockKey := string(m.indexSubspace.Bytes())
	m.store.AcquireWriteLock(lockKey)
	defer m.store.ReleaseWriteLock(lockKey)
	for i := range prepared.old {
		if err := m.applyPreparedOneKey(true, &prepared.old[i]); err != nil {
			return err
		}
	}
	if changed {
		m.clearRecordTokenizerVersion(oldRecord.PrimaryKey)
	}
	if prepared.replaceTokenizer {
		m.writeRecordTokenizerVersion(newRecord.PrimaryKey)
	}
	for i := range prepared.new {
		if err := m.applyPreparedOneKey(false, &prepared.new[i]); err != nil {
			return err
		}
	}
	return nil
}
