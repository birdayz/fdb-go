package recordlayer

import (
	"fmt"

	"fdb.dev/gen"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"
)

func (m *vectorIndexMaintainer) IsPendingWriteQueueAllowed() bool { return true }

func (m *vectorIndexMaintainer) SerializePendingWriteQueue(oldRecord, newRecord *FDBStoredRecord[proto.Message]) (*anypb.Any, error) {
	entries := &gen.OldAndNewIndexEntries{}
	for i, record := range []*FDBStoredRecord[proto.Message]{oldRecord, newRecord} {
		if record == nil {
			continue
		}
		evaluated, err := m.filteredIndexEntries(record)
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
			// Refuse at enqueue an insert replay would refuse, rather than
			// queue an entry that can never be applied.
			if err := m.insertAdmission(); err != nil {
				return nil, m.refuseCapability(err)
			}
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
	if m.engine == VectorEngineGuardiann {
		return m.applyGuardiannEntry(prefix, trimmed, gVector{data: vector, typ: vectorTypeOfEntry(entry)}, remove)
	}
	return m.withPrefixWriteLock(prefix, func(graph *hnswGraph) error {
		if remove {
			return graph.Delete(m.tx, trimmed)
		}
		return graph.insertTyped(m.tx, trimmed, vector, vectorTypeOfEntry(entry))
	})
}
