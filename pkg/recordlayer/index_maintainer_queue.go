package recordlayer

import (
	"fmt"

	"fdb.dev/gen"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"
)

func (m *standardIndexMaintainer) IsPendingWriteQueueAllowed() bool { return false }

func (m *standardIndexMaintainer) SerializePendingWriteQueue(_, _ *FDBStoredRecord[proto.Message]) (*anypb.Any, error) {
	return nil, &UnsupportedOperationError{Message: m.index.Name + " does not support the pending write queue"}
}

func (m *standardIndexMaintainer) UpdateFromQueue(_ *anypb.Any) error {
	return &UnsupportedOperationError{Message: m.index.Name + " does not support the pending write queue"}
}

func (m *vectorIndexMaintainer) IsPendingWriteQueueAllowed() bool { return true }

func (m *vectorIndexMaintainer) SerializePendingWriteQueue(oldRecord, newRecord *FDBStoredRecord[proto.Message]) (*anypb.Any, error) {
	entries := &gen.OldAndNewIndexEntries{}
	for i, record := range []*FDBStoredRecord[proto.Message]{oldRecord, newRecord} {
		if record == nil {
			continue
		}
		evaluated, err := m.evaluateIndex(record)
		if err != nil {
			return nil, err
		}
		if evaluated == nil {
			continue
		}
		if len(evaluated) != 1 {
			return nil, &RecordCoreError{Message: "expected exactly one vector index entry", IndexName: m.index.Name}
		}
		entry := evaluated[0]
		packed := &gen.IndexEntry{Key: entry.key.Pack(), Value: entry.value.Pack(), PrimaryKey: record.PrimaryKey.Pack()}
		if i == 0 {
			entries.OldEntries = append(entries.OldEntries, packed)
		} else {
			entries.NewEntries = append(entries.NewEntries, packed)
		}
	}
	return anypb.New(entries)
}

func (m *vectorIndexMaintainer) UpdateFromQueue(data *anypb.Any) error {
	entries := &gen.OldAndNewIndexEntries{}
	if err := unmarshalPendingQueueAny(data, entries); err != nil {
		return &RecordCoreError{Message: "failed to parse vector index pending write queue entry data", Cause: err}
	}
	for i, list := range [][]*gen.IndexEntry{entries.GetOldEntries(), entries.GetNewEntries()} {
		for _, packed := range list {
			key, err := tuple.Unpack(packed.GetKey())
			if err != nil {
				return err
			}
			value, err := tuple.Unpack(packed.GetValue())
			if err != nil {
				return err
			}
			primaryKey, err := tuple.Unpack(packed.GetPrimaryKey())
			if err != nil {
				return err
			}
			if err := m.applyIndexEntry(indexEntry{key: key, value: value, primaryKey: primaryKey}, i == 0); err != nil {
				return err
			}
		}
	}
	return nil
}

// applyIndexEntry is shared by ordinary writes and replay. The serialized primary
// key is complete; trimming and graph locking happen only when applying it.
func (m *vectorIndexMaintainer) applyIndexEntry(entry indexEntry, remove bool) error {
	prefix, vector, decodeErr := m.splitPrefixAndVector(entry)
	if !remove && decodeErr != nil {
		return fmt.Errorf("vector index %q: decode vector for new record: %w", m.index.Name, decodeErr)
	}
	if decodeErr == nil && vector == nil {
		return nil
	}
	trimmed, err := m.index.TrimPrimaryKey(entry.primaryKey)
	if err != nil {
		action := "insert"
		if remove {
			action = "delete"
		}
		return fmt.Errorf("trim primary key for vector index %q %s: %w", m.index.Name, action, err)
	}
	return m.withPrefixWriteLock(prefix, func(graph *hnswGraph) error {
		if remove {
			return graph.Delete(m.tx, trimmed)
		}
		return graph.Insert(m.tx, trimmed, vector)
	})
}

func (m *slidingWindowIndexMaintainer) IsPendingWriteQueueAllowed() bool {
	return m.delegate.IsPendingWriteQueueAllowed()
}

func (key slidingWindowEntryKey) pack() []byte {
	flattened := append(tuple.Tuple(nil), key.partition...)
	flattened = append(flattened, key.windowValue...)
	return append(flattened, key.primaryKey...).Pack()
}

func (m *slidingWindowIndexMaintainer) queuedEntryKey(packed []byte) (slidingWindowEntryKey, error) {
	key, err := tuple.Unpack(packed)
	if err != nil {
		return slidingWindowEntryKey{}, err
	}
	end := m.partitionKeyColumnSize + m.windowKeyColumnSize
	if len(key) < end {
		return slidingWindowEntryKey{}, &RecordCoreError{Message: "sliding window pending write queue entry key is too short", IndexName: m.index.Name}
	}
	return slidingWindowEntryKey{partition: key[:m.partitionKeyColumnSize], windowValue: key[m.partitionKeyColumnSize:end], primaryKey: key[end:]}, nil
}

func (m *slidingWindowIndexMaintainer) SerializePendingWriteQueue(oldRecord, newRecord *FDBStoredRecord[proto.Message]) (*anypb.Any, error) {
	entry := &gen.SlidingWindowQueueEntry{}
	if oldRecord != nil && m.shouldMaintain(oldRecord) {
		key, err := m.entryKeyOf(oldRecord)
		if err != nil {
			return nil, err
		}
		entry.OldEntryKey = key.pack()
		entry.DelegatedDelete, err = m.delegate.SerializePendingWriteQueue(oldRecord, nil)
		if err != nil {
			return nil, err
		}
	}
	if newRecord != nil && m.shouldMaintain(newRecord) {
		key, err := m.entryKeyOf(newRecord)
		if err != nil {
			return nil, err
		}
		entry.NewEntryKey = key.pack()
		entry.DelegatedInsert, err = m.delegate.SerializePendingWriteQueue(nil, newRecord)
		if err != nil {
			return nil, err
		}
	}
	return anypb.New(entry)
}

func (m *slidingWindowIndexMaintainer) UpdateFromQueue(data *anypb.Any) error {
	entry := &gen.SlidingWindowQueueEntry{}
	if err := unmarshalPendingQueueAny(data, entry); err != nil {
		return &RecordCoreError{Message: "failed to parse sliding window pending write queue entry data", Cause: err}
	}
	var oldKey, newKey slidingWindowEntryKey
	var err error
	if entry.OldEntryKey != nil {
		oldKey, err = m.queuedEntryKey(entry.OldEntryKey)
		if err != nil {
			return err
		}
		if entry.DelegatedDelete == nil {
			return &RecordCoreError{Message: "old record key without delegate delete", IndexName: m.index.Name}
		}
	}
	if entry.NewEntryKey != nil {
		newKey, err = m.queuedEntryKey(entry.NewEntryKey)
		if err != nil {
			return err
		}
		if entry.DelegatedInsert == nil {
			return &RecordCoreError{Message: "new record key without delegate insert", IndexName: m.index.Name}
		}
	}
	lockKey := string(m.swSubspace.Bytes())
	m.store.AcquireWriteLock(lockKey)
	defer m.store.ReleaseWriteLock(lockKey)
	if entry.OldEntryKey != nil {
		if err := m.handleDelete(oldKey, func() error { return m.delegate.UpdateFromQueue(entry.DelegatedDelete) }); err != nil {
			return err
		}
	}
	if entry.NewEntryKey != nil {
		return m.handleInsert(newKey, func() error { return m.delegate.UpdateFromQueue(entry.DelegatedInsert) })
	}
	return nil
}

func (m *atomicMutationIndexMaintainer) IsPendingWriteQueueAllowed() bool { return false }
func (m *atomicMutationIndexMaintainer) SerializePendingWriteQueue(_, _ *FDBStoredRecord[proto.Message]) (*anypb.Any, error) {
	return nil, &UnsupportedOperationError{Message: m.index.Name + " does not support the pending write queue"}
}

func (m *atomicMutationIndexMaintainer) UpdateFromQueue(_ *anypb.Any) error {
	return &UnsupportedOperationError{Message: m.index.Name + " does not support the pending write queue"}
}

func (m *bitmapValueIndexMaintainer) IsPendingWriteQueueAllowed() bool { return false }
func (m *bitmapValueIndexMaintainer) SerializePendingWriteQueue(_, _ *FDBStoredRecord[proto.Message]) (*anypb.Any, error) {
	return nil, &UnsupportedOperationError{Message: m.index.Name + " does not support the pending write queue"}
}

func (m *bitmapValueIndexMaintainer) UpdateFromQueue(_ *anypb.Any) error {
	return &UnsupportedOperationError{Message: m.index.Name + " does not support the pending write queue"}
}

func (m *maxEverVersionIndexMaintainer) IsPendingWriteQueueAllowed() bool { return false }
func (m *maxEverVersionIndexMaintainer) SerializePendingWriteQueue(_, _ *FDBStoredRecord[proto.Message]) (*anypb.Any, error) {
	return nil, &UnsupportedOperationError{Message: m.index.Name + " does not support the pending write queue"}
}

func (m *maxEverVersionIndexMaintainer) UpdateFromQueue(_ *anypb.Any) error {
	return &UnsupportedOperationError{Message: m.index.Name + " does not support the pending write queue"}
}

func (m *textIndexMaintainer) IsPendingWriteQueueAllowed() bool { return false }
func (m *textIndexMaintainer) SerializePendingWriteQueue(_, _ *FDBStoredRecord[proto.Message]) (*anypb.Any, error) {
	return nil, &UnsupportedOperationError{Message: m.index.Name + " does not support the pending write queue"}
}

func (m *textIndexMaintainer) UpdateFromQueue(_ *anypb.Any) error {
	return &UnsupportedOperationError{Message: m.index.Name + " does not support the pending write queue"}
}

func (m *versionIndexMaintainer) IsPendingWriteQueueAllowed() bool { return false }
func (m *versionIndexMaintainer) SerializePendingWriteQueue(_, _ *FDBStoredRecord[proto.Message]) (*anypb.Any, error) {
	return nil, &UnsupportedOperationError{Message: m.index.Name + " does not support the pending write queue"}
}

func (m *versionIndexMaintainer) UpdateFromQueue(_ *anypb.Any) error {
	return &UnsupportedOperationError{Message: m.index.Name + " does not support the pending write queue"}
}
