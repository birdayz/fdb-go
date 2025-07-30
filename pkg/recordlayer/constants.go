package recordlayer

// Subspace keys used by the Record Layer to organize data within FDB.
// These MUST match the Java implementation for compatibility.
// TODO: Verify these values against Java source
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

// Other constants from Java implementation
const (
	// DefaultPipelineSize is the default pipeline size for operations
	DefaultPipelineSize = 10

	// MaxRecordsForRebuild is the maximum records for rebuild operations
	MaxRecordsForRebuild = 200

	// MaxParallelIndexRebuild is the maximum parallel index rebuilds
	MaxParallelIndexRebuild = 10

	// KeySizeLimit is the maximum key size in bytes
	KeySizeLimit = 10_000

	// ValueSizeLimit is the maximum value size in bytes
	ValueSizeLimit = 100_000

	// PreloadCacheSize is the default preload cache size
	PreloadCacheSize = 100
)