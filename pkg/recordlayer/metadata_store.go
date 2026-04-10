package recordlayer

import (
	"fmt"

	"github.com/birdayz/fdb-record-layer-go/gen"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb/subspace"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb/tuple"
	"google.golang.org/protobuf/proto"
)

// FDBMetaDataStore stores and retrieves RecordMetaData from FDB.
// The metadata is stored as a serialized protobuf at a well-known key,
// enabling runtime schema evolution without application redeployment.
// Matches Java's FDBMetaDataStore.
type FDBMetaDataStore struct {
	subspace subspace.Subspace
}

// currentKey is the key for the current metadata version.
// Matches Java's FDBMetaDataStore.CURRENT_KEY = Tuple.from((Object)null).
var currentKey = tuple.Tuple{nil}

// historyKeyPrefix is the prefix for historical metadata versions.
// Matches Java's FDBMetaDataStore.HISTORY_KEY_PREFIX = Tuple.from("H").
var historyKeyPrefix = tuple.Tuple{"H"}

// NewFDBMetaDataStore creates a metadata store at the given subspace.
func NewFDBMetaDataStore(ss subspace.Subspace) *FDBMetaDataStore {
	return &FDBMetaDataStore{subspace: ss}
}

// SaveRecordMetaData saves a MetaData proto to FDB.
// The current version is stored at CURRENT_KEY, and the previous version
// (if any) is archived at HISTORY_KEY_PREFIX + oldVersion.
// Matches Java's FDBMetaDataStore.saveRecordMetaData().
func (s *FDBMetaDataStore) SaveRecordMetaData(tx fdb.Transaction, metaDataProto *gen.MetaData) error {
	serialized, err := proto.Marshal(metaDataProto)
	if err != nil {
		return fmt.Errorf("marshal metadata: %w", err)
	}

	key := s.subspace.Pack(currentKey)

	// Load existing metadata to archive as history.
	existing, _ := tx.Get(fdb.Key(key)).Get()
	if len(existing) > 0 {
		var oldProto gen.MetaData
		if err := proto.Unmarshal(existing, &oldProto); err == nil {
			oldVersion := int64(oldProto.GetVersion())
			historyKey := s.subspace.Pack(tuple.Tuple{"H", oldVersion})
			tx.Set(fdb.Key(historyKey), existing)
		}
	}

	// Save current.
	tx.Set(fdb.Key(key), serialized)
	return nil
}

// LoadRecordMetaDataProto loads the current MetaData proto from FDB.
// Returns nil if no metadata has been stored.
// Matches Java's FDBMetaDataStore.loadRecordMetaData().
func (s *FDBMetaDataStore) LoadRecordMetaDataProto(tx fdb.Transaction) (*gen.MetaData, error) {
	key := s.subspace.Pack(currentKey)
	future := tx.Get(fdb.Key(key))
	data, err := future.Get()
	if err != nil {
		return nil, fmt.Errorf("load metadata: %w", err)
	}
	if len(data) == 0 {
		return nil, nil
	}

	var metaDataProto gen.MetaData
	if err := proto.Unmarshal(data, &metaDataProto); err != nil {
		return nil, fmt.Errorf("unmarshal metadata: %w", err)
	}
	return &metaDataProto, nil
}

// LoadRecordMetaDataProtoAtVersion loads a historical version of the metadata.
// Returns nil if the version doesn't exist.
func (s *FDBMetaDataStore) LoadRecordMetaDataProtoAtVersion(tx fdb.Transaction, version int32) (*gen.MetaData, error) {
	historyKey := s.subspace.Pack(tuple.Tuple{"H", int64(version)})
	future := tx.Get(fdb.Key(historyKey))
	data, err := future.Get()
	if err != nil {
		return nil, fmt.Errorf("load metadata v%d: %w", version, err)
	}
	if len(data) == 0 {
		return nil, nil
	}

	var metaDataProto gen.MetaData
	if err := proto.Unmarshal(data, &metaDataProto); err != nil {
		return nil, fmt.Errorf("unmarshal metadata v%d: %w", version, err)
	}
	return &metaDataProto, nil
}
