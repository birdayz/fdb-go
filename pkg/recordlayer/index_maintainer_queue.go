package recordlayer

import (
	"fdb.dev/gen"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"
)

func (m *StandardIndexMaintainer) IsPendingWriteQueueAllowed() bool { return false }

func (m *StandardIndexMaintainer) SerializePendingWriteQueue(_, _ *FDBStoredRecord[proto.Message]) (*anypb.Any, error) {
	return nil, &UnsupportedOperationError{Message: m.index.Name + " does not support the pending write queue"}
}

func (m *StandardIndexMaintainer) UpdateFromQueue(_ *anypb.Any) error {
	return &UnsupportedOperationError{Message: m.index.Name + " does not support the pending write queue"}
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
	maintainOld, err := m.shouldMaintain(oldRecord)
	if err != nil {
		return nil, err
	}
	maintainNew, err := m.shouldMaintain(newRecord)
	if err != nil {
		return nil, err
	}
	if maintainOld {
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
	if maintainNew {
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
	if err := UnmarshalPendingQueueAny(data, entry); err != nil {
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
