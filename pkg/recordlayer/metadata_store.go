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

// NewFDBMetaDataStore creates a metadata store at the given subspace.
func NewFDBMetaDataStore(ss subspace.Subspace) *FDBMetaDataStore {
	return &FDBMetaDataStore{subspace: ss}
}

// SaveRecordMetaData saves a MetaData proto to FDB.
// The current version is stored at CURRENT_KEY, and the previous version
// (if any) is archived at HISTORY_KEY_PREFIX + oldVersion.
// Uses SplitHelper for wire compatibility with Java's FDBMetaDataStore,
// which stores metadata with split support (unsplit suffix 0).
// Matches Java's FDBMetaDataStore.saveRecordMetaData().
func (s *FDBMetaDataStore) SaveRecordMetaData(tx fdb.WritableTransaction, metaDataProto *gen.MetaData) error {
	serialized, err := proto.Marshal(metaDataProto)
	if err != nil {
		return fmt.Errorf("marshal metadata: %w", err)
	}

	// Load existing metadata to archive as history.
	// Capture sizeInfo to clear stale split chunks when overwriting.
	var existingSize sizeInfo
	existing, err := loadWithSplit(tx, s.subspace, currentKey, true, false, &existingSize)
	if err != nil {
		return fmt.Errorf("load existing metadata: %w", err)
	}
	if len(existing) > 0 {
		var oldProto gen.MetaData
		if err := proto.Unmarshal(existing, &oldProto); err == nil {
			oldVersion := int64(oldProto.GetVersion())
			historyKey := tuple.Tuple{"H", oldVersion}
			if err := saveWithSplit(tx, s.subspace, historyKey, existing, true, false, nil, &sizeInfo{}); err != nil {
				return fmt.Errorf("archive metadata v%d: %w", oldVersion, err)
			}
		}
	}

	// Save current with split support (matching Java).
	// Pass existingSize so clearPreviousRecord removes stale split chunks.
	if err := saveWithSplit(tx, s.subspace, currentKey, serialized, true, false, &existingSize, &sizeInfo{}); err != nil {
		return fmt.Errorf("save metadata: %w", err)
	}
	return nil
}

// LoadRecordMetaDataProto loads the current MetaData proto from FDB.
// Returns nil if no metadata has been stored.
// Uses SplitHelper for wire compatibility with Java's FDBMetaDataStore.
// Matches Java's FDBMetaDataStore.loadRecordMetaData().
func (s *FDBMetaDataStore) LoadRecordMetaDataProto(tx fdb.WritableTransaction) (*gen.MetaData, error) {
	data, err := loadWithSplit(tx, s.subspace, currentKey, true, false, &sizeInfo{})
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

// Subspace returns the subspace this metadata store uses.
func (s *FDBMetaDataStore) Subspace() subspace.Subspace {
	return s.subspace
}

// LoadRecordMetaDataProtoAtVersion loads a historical version of the metadata.
// Returns nil if the version doesn't exist.
// Uses SplitHelper for wire compatibility with Java's FDBMetaDataStore.
func (s *FDBMetaDataStore) LoadRecordMetaDataProtoAtVersion(tx fdb.WritableTransaction, version int32) (*gen.MetaData, error) {
	historyKey := tuple.Tuple{"H", int64(version)}
	data, err := loadWithSplit(tx, s.subspace, historyKey, true, false, &sizeInfo{})
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
