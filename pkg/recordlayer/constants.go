package recordlayer

// Subspace keys used by the Record Layer to organize data within FDB.
// These MUST match the Java implementation for compatibility.
// Verified against Java: FDBRecordStoreKeyspace.java (enum values 0-9)
const (
	// StoreInfoKey is the subspace key for store metadata
	StoreInfoKey = 0

	// RecordKey is the subspace key for storing records
	RecordKey = 1

	// IndexKey is the subspace key for storing indexes
	IndexKey = 2

	// IndexSecondarySpaceKey is the subspace key for secondary index data
	IndexSecondarySpaceKey = 3

	// RecordCountKey is the subspace key for record counts
	RecordCountKey = 4

	// IndexStateSpaceKey is the subspace key for index state
	IndexStateSpaceKey = 5

	// IndexRangeSpaceKey is the subspace key for index ranges
	IndexRangeSpaceKey = 6

	// IndexUniquenessViolationsKey is the subspace key for uniqueness violations
	IndexUniquenessViolationsKey = 7

	// RecordVersionKey is the subspace key for record versions
	RecordVersionKey = 8

	// IndexBuildSpaceKey is the subspace key for index building
	IndexBuildSpaceKey = 9
)

// Record key suffix constants matching Java's SplitHelper
const (
	// unsplitRecord is the suffix appended to unsplit record keys.
	// Matches Java's SplitHelper.UNSPLIT_RECORD = 0L.
	// For format version >= SAVE_UNSPLIT_WITH_SUFFIX (5), every record key
	// ends with this constant regardless of record type.
	unsplitRecord = int64(0)

	// startSplitRecord is the first suffix for split record chunks.
	// Matches Java's SplitHelper.START_SPLIT_RECORD = 1L.
	// Split records use suffixes 1, 2, 3, ... for consecutive chunks.
	startSplitRecord = int64(1)

	// recordVersionSuffix is the suffix for inline version keys.
	// Matches Java's SplitHelper.RECORD_VERSION = -1L.
	// For format version >= SAVE_VERSION_WITH_RECORD (6), versions are stored
	// adjacent to the record as recordsSubspace.pack(primaryKey, -1).
	recordVersionSuffix = int64(-1)

	// splitRecordSize is the maximum size of a single FDB value before splitting.
	// Matches Java's SplitHelper.SPLIT_RECORD_SIZE = 100_000.
	splitRecordSize = 100_000
)

// Other constants from Java implementation
const (
	// defaultPipelineSize is the default pipeline size for operations
	defaultPipelineSize = 10

	// maxRecordsForRebuild is the maximum records for rebuild operations
	maxRecordsForRebuild = 200

	// keySizeLimit is the maximum key size in bytes.
	// Matches Java's FDBRecordStore.KEY_SIZE_LIMIT.
	keySizeLimit = 10_000

	// valueSizeLimit is the maximum value size in bytes.
	// Matches Java's FDBRecordStore.VALUE_SIZE_LIMIT.
	valueSizeLimit = 100_000
)

// GetKeySizeLimit returns the maximum key size in bytes for index entries.
// Matches Java's FDBRecordStore.getKeySizeLimit().
func (store *FDBRecordStore) GetKeySizeLimit() int {
	return keySizeLimit
}

// GetValueSizeLimit returns the maximum value size in bytes for index entries.
// Matches Java's FDBRecordStore.getValueSizeLimit().
func (store *FDBRecordStore) GetValueSizeLimit() int {
	return valueSizeLimit
}
