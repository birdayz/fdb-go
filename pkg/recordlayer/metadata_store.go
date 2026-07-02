package recordlayer

import (
	"fmt"

	"fdb.dev/gen"
	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"google.golang.org/protobuf/proto"
)

// FDBMetaDataStore stores and retrieves RecordMetaData from FDB.
// The metadata is stored as a serialized protobuf at a well-known key,
// enabling runtime schema evolution without application redeployment.
// Matches Java's FDBMetaDataStore.
type FDBMetaDataStore struct {
	subspace           subspace.Subspace
	evolutionValidator *MetaDataEvolutionValidator
}

// currentKey is the key for the current metadata version.
// Matches Java's FDBMetaDataStore.CURRENT_KEY = Tuple.from((Object)null).
var currentKey = tuple.Tuple{nil}

// NewFDBMetaDataStore creates a metadata store at the given subspace. The
// evolution validator defaults to the strictest configuration, matching
// Java's `evolutionValidator = MetaDataEvolutionValidator.getDefaultInstance()`.
func NewFDBMetaDataStore(ss subspace.Subspace) *FDBMetaDataStore {
	return &FDBMetaDataStore{
		subspace:           ss,
		evolutionValidator: DefaultMetaDataEvolutionValidator(),
	}
}

// SetEvolutionValidator replaces the validator SaveRecordMetaData runs
// against the currently stored metadata.
// Matches Java's FDBMetaDataStore.setEvolutionValidator().
func (s *FDBMetaDataStore) SetEvolutionValidator(v *MetaDataEvolutionValidator) {
	s.evolutionValidator = v
}

// MetaDataVersionMustIncreaseError is returned by SaveRecordMetaData when
// the new metadata's version is not strictly greater than the stored one.
// This check is unconditional — it does not consult the evolution
// validator's allowNoVersionChange knob, exactly like Java, where
// saveAndSetCurrent throws before the validator ever runs.
// Matches Java's MetaDataException("meta-data version must increase")
// with log keys OLD / NEW.
type MetaDataVersionMustIncreaseError struct {
	OldVersion int32
	NewVersion int32
}

func (e *MetaDataVersionMustIncreaseError) Error() string {
	return fmt.Sprintf("meta-data version must increase (old: %d, new: %d)", e.OldVersion, e.NewVersion)
}

// SaveRecordMetaData saves a MetaData proto to FDB, with the full
// validation Java runs in the same transaction — this is NOT a raw
// persist. In order (matching Java's FDBMetaDataStore.saveAndSetCurrent):
//
//  1. the new proto must build into a RecordMetaData;
//  2. if metadata is already stored: the new version must be strictly
//     greater (MetaDataVersionMustIncreaseError otherwise), the evolution
//     validator must pass old → new, and the old serialized bytes are
//     archived at HISTORY_KEY_PREFIX + oldVersion;
//  3. the new metadata is written at CURRENT_KEY.
//
// Because all of this happens inside the caller's transaction, FDB
// conflict detection serializes concurrent evolvers — two racing saves
// against the same old version cannot both commit.
//
// Uses SplitHelper for wire compatibility with Java's FDBMetaDataStore,
// which stores metadata with split support (unsplit suffix 0).
func (s *FDBMetaDataStore) SaveRecordMetaData(tx fdb.WritableTransaction, metaDataProto *gen.MetaData) error {
	// Java: buildMetaData(metaDataProto, true) — the new metadata must
	// build before anything is written.
	newMeta, err := RecordMetaDataFromProto(metaDataProto)
	if err != nil {
		return fmt.Errorf("new metadata does not build: %w", err)
	}

	serialized, err := proto.Marshal(metaDataProto)
	if err != nil {
		return fmt.Errorf("marshal metadata: %w", err)
	}

	// Load existing metadata to validate the evolution and archive it as
	// history. Capture sizeInfo to clear stale split chunks when
	// overwriting.
	var existingSize sizeInfo
	existing, err := loadWithSplit(tx, s.subspace, currentKey, true, false, &existingSize)
	if err != nil {
		return fmt.Errorf("load existing metadata: %w", err)
	}
	if len(existing) > 0 {
		var oldProto gen.MetaData
		if err := proto.Unmarshal(existing, &oldProto); err != nil {
			// Java's parseMetaDataProto throws here — corrupt current
			// metadata must never be silently overwritten.
			return fmt.Errorf("parse current metadata: %w", err)
		}
		oldVersion := oldProto.GetVersion()
		if metaDataProto.GetVersion() <= oldVersion {
			return &MetaDataVersionMustIncreaseError{
				OldVersion: oldVersion,
				NewVersion: metaDataProto.GetVersion(),
			}
		}
		oldMeta, err := RecordMetaDataFromProto(&oldProto)
		if err != nil {
			return fmt.Errorf("current metadata does not build: %w", err)
		}
		if err := s.evolutionValidator.Validate(oldMeta, newMeta); err != nil {
			return err
		}
		historyKey := tuple.Tuple{"H", int64(oldVersion)}
		if err := saveWithSplit(tx, s.subspace, historyKey, existing, true, false, nil, &sizeInfo{}); err != nil {
			return fmt.Errorf("archive metadata v%d: %w", oldVersion, err)
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
