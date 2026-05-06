package recordlayer

import (
	"fmt"
	"strconv"
	"strings"

	gen "github.com/birdayz/fdb-record-layer-go/gen"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb/tuple"
	"google.golang.org/protobuf/proto"
)

// Index type constants matching Java's IndexTypes.
const (
	IndexTypeValue                 = "value"
	IndexTypeCount                 = "count"
	IndexTypeCountNotNull          = "count_not_null"
	IndexTypeCountUpdates          = "count_updates"
	IndexTypeSum                   = "sum"
	IndexTypeMaxEverLong           = "max_ever_long"
	IndexTypeMinEverLong           = "min_ever_long"
	IndexTypeMaxEverTuple          = "max_ever_tuple"
	IndexTypeMinEverTuple          = "min_ever_tuple"
	IndexTypeRank                  = "rank"
	IndexTypeVersion               = "version"
	IndexTypeMaxEverVersion        = "max_ever_version"
	IndexTypePermutedMin           = "permuted_min"
	IndexTypePermutedMax           = "permuted_max"
	IndexTypeBitmapValue           = "bitmap_value"
	IndexTypeText                  = "text"
	IndexTypeTimeWindowLeaderboard = "time_window_leaderboard"
	IndexTypeMultidimensional      = "multidimensional"
	IndexTypeVector                = "vector"
)

// Index option keys matching Java's IndexOptions.
const (
	IndexOptionUnique               = "unique"
	IndexOptionClearWhenZero        = "clearWhenZero"
	IndexOptionReplacedByPrefix     = "replacedBy"
	IndexOptionBitmapValueEntrySize = "bitmapValueEntrySize"

	// TEXT index options matching Java's IndexOptions.
	IndexOptionTextTokenizerName               = "textTokenizerName"
	IndexOptionTextTokenizerVersion            = "textTokenizerVersion"
	IndexOptionTextAddAggressiveConflictRanges = "textAddAggressiveConflictRanges"
	IndexOptionTextOmitPositions               = "textOmitPositions"

	// Runtime-only index option, always safe to change.
	// Matches Java's IndexOptions.ALLOWED_FOR_QUERY_OPTION.
	IndexOptionAllowedForQuery = "allowedForQuery"
)

// IndexPredicate is a function that determines whether a record should be indexed.
// Return true to include the record in the index, false to skip it.
// Matches Java's Index predicate concept for sparse/filtered indexes.
type IndexPredicate func(msg proto.Message) bool

// Index represents a secondary index definition.
// Matches Java's com.apple.foundationdb.record.metadata.Index.
type Index struct {
	Name                string
	Type                string
	RootExpression      KeyExpression
	subspaceKey         any
	Options             map[string]string
	AddedVersion        int
	LastModifiedVersion int

	// Predicate filters which records are included in this index.
	// If nil, all records are indexed. If set, only records where
	// Predicate returns true are indexed (sparse/filtered index).
	Predicate IndexPredicate

	// predicateProto stores the proto representation of the predicate
	// for round-tripping. Set when loading from proto (Java-defined predicates)
	// or when using SetPredicateProto(). Nil for programmatic Go predicates
	// set via SetPredicate().
	predicateProto *gen.Predicate

	// primaryKeyComponentPositions tracks overlap between index key and primary key.
	// Each element corresponds to a primary key component:
	//   >= 0: the component already appears at that position in the index key (deduplicated)
	//   < 0:  the component is NOT in the index key (appended to the entry)
	// nil means no overlap (all PK components are appended as-is).
	// Matches Java's Index.primaryKeyComponentPositions.
	primaryKeyComponentPositions []int
}

// NewIndex creates a VALUE index with the given name and root key expression.
// Matches Java's new Index(name, rootExpression) which defaults to IndexTypes.VALUE.
func NewIndex(name string, rootExpression KeyExpression) *Index {
	return &Index{
		Name:           name,
		Type:           IndexTypeValue,
		RootExpression: rootExpression,
		subspaceKey:    name,
		Options:        make(map[string]string),
	}
}

// NewCountIndex creates a COUNT index with the given name and root key expression.
// COUNT indexes use FDB atomic ADD to maintain counts per grouping key.
// Matches Java's new Index(name, rootExpression, IndexTypes.COUNT).
func NewCountIndex(name string, rootExpression KeyExpression) *Index {
	return &Index{
		Name:           name,
		Type:           IndexTypeCount,
		RootExpression: rootExpression,
		subspaceKey:    name,
		Options:        make(map[string]string),
	}
}

// NewSumIndex creates a SUM index with the given name and root key expression.
// SUM indexes use FDB atomic ADD to maintain running sums per grouping key.
// The expression must include at least one grouped (aggregated) column.
// Matches Java's new Index(name, rootExpression, IndexTypes.SUM).
func NewSumIndex(name string, rootExpression KeyExpression) *Index {
	return &Index{
		Name:           name,
		Type:           IndexTypeSum,
		RootExpression: rootExpression,
		subspaceKey:    name,
		Options:        make(map[string]string),
	}
}

// NewMaxEverLongIndex creates a MAX_EVER_LONG index with the given name and root key expression.
// MAX_EVER_LONG indexes use FDB atomic MAX to track the maximum value seen per grouping key.
// Values must be non-negative (unsigned comparison). Deletes are no-ops (_EVER = irreversible).
// Matches Java's new Index(name, rootExpression, IndexTypes.MAX_EVER_LONG).
func NewMaxEverLongIndex(name string, rootExpression KeyExpression) *Index {
	return &Index{
		Name:           name,
		Type:           IndexTypeMaxEverLong,
		RootExpression: rootExpression,
		subspaceKey:    name,
		Options:        make(map[string]string),
	}
}

// NewMinEverLongIndex creates a MIN_EVER_LONG index with the given name and root key expression.
// MIN_EVER_LONG indexes use FDB atomic MIN to track the minimum value seen per grouping key.
// Values must be non-negative (unsigned comparison). Deletes are no-ops (_EVER = irreversible).
// Matches Java's new Index(name, rootExpression, IndexTypes.MIN_EVER_LONG).
func NewMinEverLongIndex(name string, rootExpression KeyExpression) *Index {
	return &Index{
		Name:           name,
		Type:           IndexTypeMinEverLong,
		RootExpression: rootExpression,
		subspaceKey:    name,
		Options:        make(map[string]string),
	}
}

// NewMaxEverTupleIndex creates a MAX_EVER_TUPLE index with the given name and root key expression.
// MAX_EVER_TUPLE indexes use FDB atomic BYTE_MAX to track the maximum tuple-packed value per grouping key.
// Unlike MAX_EVER_LONG, accepts any tuple-encodable type and compares via byte ordering.
// Deletes are no-ops (_EVER = irreversible). Idempotent.
// Matches Java's new Index(name, rootExpression, IndexTypes.MAX_EVER_TUPLE).
func NewMaxEverTupleIndex(name string, rootExpression KeyExpression) *Index {
	return &Index{
		Name:           name,
		Type:           IndexTypeMaxEverTuple,
		RootExpression: rootExpression,
		subspaceKey:    name,
		Options:        make(map[string]string),
	}
}

// NewMinEverTupleIndex creates a MIN_EVER_TUPLE index with the given name and root key expression.
// MIN_EVER_TUPLE indexes use FDB atomic BYTE_MIN to track the minimum tuple-packed value per grouping key.
// Unlike MIN_EVER_LONG, accepts any tuple-encodable type and compares via byte ordering.
// Deletes are no-ops (_EVER = irreversible). Idempotent.
// Matches Java's new Index(name, rootExpression, IndexTypes.MIN_EVER_TUPLE).
func NewMinEverTupleIndex(name string, rootExpression KeyExpression) *Index {
	return &Index{
		Name:           name,
		Type:           IndexTypeMinEverTuple,
		RootExpression: rootExpression,
		subspaceKey:    name,
		Options:        make(map[string]string),
	}
}

// NewCountNotNullIndex creates a COUNT_NOT_NULL index with the given name and root key expression.
// Like COUNT, but skips entries where the key contains a null value (nil element).
// Matches Java's new Index(name, rootExpression, IndexTypes.COUNT_NOT_NULL).
func NewCountNotNullIndex(name string, rootExpression KeyExpression) *Index {
	return &Index{
		Name:           name,
		Type:           IndexTypeCountNotNull,
		RootExpression: rootExpression,
		subspaceKey:    name,
		Options:        make(map[string]string),
	}
}

// NewCountUpdatesIndex creates a COUNT_UPDATES index with the given name and root key expression.
// Like COUNT, but deletes are no-ops (count never decrements) and updates always re-count
// (skipUpdateForUnchangedKeys = false). Tracks total insert+update events.
// Matches Java's new Index(name, rootExpression, IndexTypes.COUNT_UPDATES).
func NewCountUpdatesIndex(name string, rootExpression KeyExpression) *Index {
	return &Index{
		Name:           name,
		Type:           IndexTypeCountUpdates,
		RootExpression: rootExpression,
		subspaceKey:    name,
		Options:        make(map[string]string),
	}
}

// NewRankIndex creates a RANK index with the given name and root key expression.
// RANK indexes maintain a B-tree (like VALUE) plus a skip-list ranked set per group
// for O(log n) rank/select queries.
// Matches Java's new Index(name, rootExpression, IndexTypes.RANK).
func NewRankIndex(name string, rootExpression KeyExpression) *Index {
	return &Index{
		Name:           name,
		Type:           IndexTypeRank,
		RootExpression: rootExpression,
		subspaceKey:    name,
		Options:        make(map[string]string),
	}
}

// NewVersionIndex creates a VERSION index with the given name and root key expression.
// VERSION indexes store the record's commit version in the index key for version-ordered queries.
// The root expression should include a VersionKeyExpression (typically via Concat with other fields).
// Matches Java's new Index(name, rootExpression, IndexTypes.VERSION).
func NewVersionIndex(name string, rootExpression KeyExpression) *Index {
	return &Index{
		Name:           name,
		Type:           IndexTypeVersion,
		RootExpression: rootExpression,
		subspaceKey:    name,
		Options:        make(map[string]string),
	}
}

// NewMaxEverVersionIndex creates a MAX_EVER_VERSION index that tracks the maximum
// version ever written per grouping key. The root expression must be a GroupingKeyExpression
// with exactly 1 VersionKeyExpression in the grouped (aggregated) portion.
// Matches Java's new Index(name, rootExpression, IndexTypes.MAX_EVER_VERSION).
func NewMaxEverVersionIndex(name string, rootExpression KeyExpression) *Index {
	return &Index{
		Name:           name,
		Type:           IndexTypeMaxEverVersion,
		RootExpression: rootExpression,
		subspaceKey:    name,
		Options:        make(map[string]string),
	}
}

// NewPermutedMaxIndex creates a PERMUTED_MAX index with the given name, root key expression,
// and permuted size. The permuted size specifies how many trailing grouping columns are
// permuted to after the value in the secondary subspace, enabling value-ordered scans.
// Matches Java's new Index(name, rootExpression, IndexTypes.PERMUTED_MAX) with PERMUTED_SIZE_OPTION.
func NewPermutedMaxIndex(name string, rootExpression KeyExpression, permutedSize int) *Index {
	return &Index{
		Name:           name,
		Type:           IndexTypePermutedMax,
		RootExpression: rootExpression,
		subspaceKey:    name,
		Options: map[string]string{
			IndexOptionPermutedSize: strconv.Itoa(permutedSize),
		},
	}
}

// NewPermutedMinIndex creates a PERMUTED_MIN index with the given name, root key expression,
// and permuted size. The permuted size specifies how many trailing grouping columns are
// permuted to after the value in the secondary subspace, enabling value-ordered scans.
// Matches Java's new Index(name, rootExpression, IndexTypes.PERMUTED_MIN) with PERMUTED_SIZE_OPTION.
func NewPermutedMinIndex(name string, rootExpression KeyExpression, permutedSize int) *Index {
	return &Index{
		Name:           name,
		Type:           IndexTypePermutedMin,
		RootExpression: rootExpression,
		subspaceKey:    name,
		Options: map[string]string{
			IndexOptionPermutedSize: strconv.Itoa(permutedSize),
		},
	}
}

// NewBitmapValueIndex creates a BITMAP_VALUE index with the given name and root key expression.
// BITMAP_VALUE indexes store one bit per record in fixed-size bitmaps, using atomic
// BIT_OR/BIT_AND operations for set/clear. The position field is the last grouped column.
// Matches Java's new Index(name, rootExpression, IndexTypes.BITMAP_VALUE).
func NewBitmapValueIndex(name string, rootExpression KeyExpression) *Index {
	return &Index{
		Name:           name,
		Type:           IndexTypeBitmapValue,
		RootExpression: rootExpression,
		subspaceKey:    name,
		Options:        make(map[string]string),
	}
}

// NewTextIndex creates a TEXT index with the given name and root key expression.
// TEXT indexes tokenize string fields and store per-token position lists in a BunchedMap.
// Matches Java's new Index(name, rootExpression, IndexTypes.TEXT).
func NewTextIndex(name string, rootExpression KeyExpression) *Index {
	return &Index{
		Name:           name,
		Type:           IndexTypeText,
		RootExpression: rootExpression,
		subspaceKey:    name,
		Options:        make(map[string]string),
	}
}

// NewTimeWindowLeaderboardIndex creates a TIME_WINDOW_LEADERBOARD index.
// TIME_WINDOW_LEADERBOARD indexes maintain multiple ranked sets, one per time window,
// enabling time-windowed leaderboard queries.
// Matches Java's new Index(name, rootExpression, IndexTypes.TIME_WINDOW_LEADERBOARD).
func NewTimeWindowLeaderboardIndex(name string, rootExpression KeyExpression) *Index {
	return &Index{
		Name:           name,
		Type:           IndexTypeTimeWindowLeaderboard,
		RootExpression: rootExpression,
		subspaceKey:    name,
		Options:        make(map[string]string),
	}
}

// NewVectorIndex creates a VECTOR index backed by an HNSW graph.
// The root expression identifies the vector field to index. numDimensions specifies
// the vector dimensionality. Supports EUCLIDEAN, COSINE, and INNER_PRODUCT metrics.
// Matches Java's new Index(name, rootExpression, IndexTypes.VECTOR).
func NewVectorIndex(name string, rootExpression KeyExpression, numDimensions int) *Index {
	return &Index{
		Name:           name,
		Type:           IndexTypeVector,
		RootExpression: rootExpression,
		subspaceKey:    name,
		Options: map[string]string{
			IndexOptionVectorNumDimensions: fmt.Sprintf("%d", numDimensions),
		},
	}
}

// NewMultidimensionalIndex creates a MULTIDIMENSIONAL index backed by a Hilbert R-tree.
// The root expression must be a DimensionsKeyExpression that specifies prefix, dimensions, and suffix.
// Matches Java's new Index(name, rootExpression, IndexTypes.MULTIDIMENSIONAL).
func NewMultidimensionalIndex(name string, rootExpression KeyExpression) *Index {
	return &Index{
		Name:           name,
		Type:           IndexTypeMultidimensional,
		RootExpression: rootExpression,
		subspaceKey:    name,
		Options:        make(map[string]string),
	}
}

// SubspaceTupleKey returns the key used to identify this index's subspace within
// the IndexKey (2) subspace. Defaults to the index name.
// Matches Java's Index.getSubspaceTupleKey().
func (idx *Index) SubspaceTupleKey() any {
	return idx.subspaceKey
}

// SetSubspaceKey overrides the default subspace key (index name).
// Matches Java's Index.setSubspaceKey().
func (idx *Index) SetSubspaceKey(key any) *Index {
	idx.subspaceKey = key
	return idx
}

// IsUnique returns whether this index enforces a uniqueness constraint.
// Matches Java's Index.isUnique() which checks IndexOptions.UNIQUE_OPTION.
func (idx *Index) IsUnique() bool {
	v, ok := idx.Options[IndexOptionUnique]
	return ok && v == "true"
}

// IsClearWhenZero returns whether this index should clear entries when values reach zero.
// When true, atomic ADD mutations are followed by CompareAndClear(zero) to remove
// stale zero-value entries. Applies to COUNT, COUNT_NOT_NULL, and SUM indexes.
// Matches Java's IndexOptions.CLEAR_WHEN_ZERO.
func (idx *Index) IsClearWhenZero() bool {
	v, ok := idx.Options[IndexOptionClearWhenZero]
	return ok && v == "true"
}

// SetClearWhenZero enables or disables the clear-when-zero behavior.
// Matches Java's IndexOptions.CLEAR_WHEN_ZERO.
func (idx *Index) SetClearWhenZero(clear bool) *Index {
	if clear {
		idx.Options[IndexOptionClearWhenZero] = "true"
	} else {
		delete(idx.Options, IndexOptionClearWhenZero)
	}
	return idx
}

// GetBooleanOption returns the boolean value of an index option.
// Returns the default value if the option is not set.
// Matches Java's Index.getBooleanOption(String, boolean).
func (idx *Index) GetBooleanOption(key string, defaultVal bool) bool {
	v, ok := idx.Options[key]
	if !ok {
		return defaultVal
	}
	return v == "true"
}

// SetPredicate sets a filter predicate for sparse/filtered indexes.
// Only records where the predicate returns true will have index entries.
// Note: programmatic Go predicates cannot be serialized to proto. Use
// SetPredicateProto for predicates that must survive metadata round-tripping.
func (idx *Index) SetPredicate(p IndexPredicate) *Index {
	idx.Predicate = p
	return idx
}

// SetPredicateProto sets a predicate from a proto message. This both stores
// the proto for round-tripping and builds an evaluator function.
func (idx *Index) SetPredicateProto(p *gen.Predicate) error {
	idx.predicateProto = p
	if p != nil {
		fn, err := predicateFromProto(p)
		if err != nil {
			return fmt.Errorf("index %s: predicate: %w", idx.Name, err)
		}
		idx.Predicate = fn
	} else {
		idx.Predicate = nil
	}
	return nil
}

// GetPredicateProto returns the proto representation of the predicate, if any.
// Returns nil for programmatic Go predicates set via SetPredicate().
func (idx *Index) GetPredicateProto() *gen.Predicate {
	return idx.predicateProto
}

// PrimaryKeyComponentPositions returns the overlap mapping between index key and primary key.
// nil means no overlap was computed. Matches Java's Index.getPrimaryKeyComponentPositions().
func (idx *Index) PrimaryKeyComponentPositions() []int {
	return idx.primaryKeyComponentPositions
}

// HasPrimaryKeyComponentPositions returns true if the index has computed PK component positions.
// Matches Java's Index.hasPrimaryKeyComponentPositions().
func (idx *Index) HasPrimaryKeyComponentPositions() bool {
	return idx.primaryKeyComponentPositions != nil
}

// GetReplacedByIndexNames returns the names of indexes that replace this one.
// Options with keys starting with "replacedBy" (e.g. "replacedBy", "replacedBy_0")
// have their values as replacement index names.
// Matches Java's Index.getReplacedByIndexNames().
func (idx *Index) GetReplacedByIndexNames() []string {
	var names []string
	for k, v := range idx.Options {
		if strings.HasPrefix(k, IndexOptionReplacedByPrefix) {
			names = append(names, v)
		}
	}
	return names
}

// SetUnique marks this index as enforcing uniqueness.
func (idx *Index) SetUnique() *Index {
	idx.Options[IndexOptionUnique] = "true"
	return idx
}

// indexEntryKey builds the FDB tuple for an index entry.
// Format: (indexedValues..., trimmedPrimaryKeyValues...).
// When the index has primaryKeyComponentPositions, PK components that already
// appear in the index key are omitted (deduplicated). This matches Java's
// FDBRecordStoreBase.indexEntryKey() which calls Index.TrimPrimaryKey().
func indexEntryKey(idx *Index, indexValues tuple.Tuple, primaryKey tuple.Tuple) (tuple.Tuple, error) {
	trimmed, err := idx.TrimPrimaryKey(primaryKey)
	if err != nil {
		return nil, err
	}
	// Fast path: no PK to append (fully deduplicated or empty PK)
	if len(trimmed) == 0 {
		return indexValues, nil
	}
	// Fast path: no index values (PK-only index)
	if len(indexValues) == 0 {
		return trimmed, nil
	}
	entry := make(tuple.Tuple, 0, len(indexValues)+len(trimmed))
	entry = append(entry, indexValues...)
	entry = append(entry, trimmed...)
	return entry, nil
}

// trimPrimaryKey removes PK components that already appear in the index key.
// Returns the remaining PK components that need to be appended to the index entry.
// Returns an error if primaryKeyComponentPositions references an index beyond the
// primary key length.
// Matches Java's Index.TrimPrimaryKey().
func (idx *Index) TrimPrimaryKey(primaryKey tuple.Tuple) (tuple.Tuple, error) {
	if idx.primaryKeyComponentPositions == nil {
		return primaryKey, nil
	}
	trimmed := make(tuple.Tuple, 0, len(primaryKey))
	for i, pos := range idx.primaryKeyComponentPositions {
		if i >= len(primaryKey) {
			return nil, fmt.Errorf("trimPrimaryKey: primaryKeyComponentPositions[%d] out of bounds for primary key of length %d (index %q)", i, len(primaryKey), idx.Name)
		}
		if pos < 0 {
			trimmed = append(trimmed, primaryKey[i])
		}
	}
	return trimmed, nil
}

// getEntryPrimaryKey reconstructs the full primary key from an index entry key.
// When primaryKeyComponentPositions is set, some PK components come from the
// index key portion and some from the appended portion.
// Returns an empty tuple if the entry key is truncated (fewer elements than expected).
// Matches Java's Index.getEntryPrimaryKey().
func (idx *Index) getEntryPrimaryKey(entryKey tuple.Tuple) tuple.Tuple {
	colSize := idx.RootExpression.ColumnSize()
	if idx.primaryKeyComponentPositions == nil {
		if colSize < len(entryKey) {
			return entryKey[colSize:]
		}
		return tuple.Tuple{}
	}

	// Validate minimum expected length: at least colSize elements for index values,
	// plus enough trailing elements for PK components not in the index key.
	expectedTrailing := 0
	for _, pos := range idx.primaryKeyComponentPositions {
		if pos < 0 {
			expectedTrailing++
		}
	}
	minLen := colSize + expectedTrailing
	if len(entryKey) < minLen {
		return tuple.Tuple{} // truncated entry: return empty PK rather than nil-filled garbage
	}

	pk := make(tuple.Tuple, len(idx.primaryKeyComponentPositions))
	after := colSize
	for i, pos := range idx.primaryKeyComponentPositions {
		if pos >= 0 && pos < len(entryKey) {
			pk[i] = entryKey[pos]
		} else if after < len(entryKey) {
			pk[i] = entryKey[after]
			after++
		}
	}
	return pk
}
