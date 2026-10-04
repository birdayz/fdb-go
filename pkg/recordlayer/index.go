package recordlayer

import (
	"fmt"
	"reflect"
	"slices"
	"sort"
	"strconv"
	"strings"

	gen "fdb.dev/gen"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
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
	// IndexTypeVectorSPFresh is the Go-only FDB-native vector index (RFC-094):
	// SPANN centroid + posting-list layout with two-level routing and SPFresh
	// incremental rebalancing. Java has no counterpart; deployments sharing
	// metadata with Java apps must keep this index out of shared metadata (a
	// Java app fails maintainer lookup for the unknown type). Records remain
	// fully Java-readable — the index writes only under its own subspace.
	IndexTypeVectorSPFresh = "vector_spfresh"

	// IndexTypeMinEver and IndexTypeMaxEver are the DEPRECATED bare spellings
	// that predate the _long/_tuple split. Java still accepts them and says
	// exactly why it cannot stop: "Deprecated for new usage, but this can't
	// really be removed because old meta-data might include it"
	// (IndexTypes.java:67-81). They are declared here for the same reason —
	// Java-authored metadata on a shared cluster may carry them, and this port
	// has to READ that metadata correctly.
	//
	// Java resolves each to its _LONG behaviour UNCONDITIONALLY:
	// getAtomicMutation returns MIN_EVER_LONG for `MIN_EVER_LONG || MIN_EVER` and
	// MAX_EVER_LONG for `MAX_EVER_LONG || MAX_EVER`
	// (AtomicMutationIndexMaintainer.java:100-106). There is no dependence on the
	// key type, and the doc comment on IndexTypes.MIN_EVER says the same thing
	// declaratively — "Compatible name for MIN_EVER_LONG".
	//
	// Never WRITE these. New metadata uses the _long or _tuple spelling; these
	// exist so old and Java-authored metadata resolves to the right maintainer
	// instead of falling off the end of a switch.
	IndexTypeMinEver = "min_ever"
	IndexTypeMaxEver = "max_ever"
)

// canonicalIndexType resolves a deprecated index-type alias to the type whose
// behaviour it names, and returns every other type unchanged.
//
// One function rather than two more `case` labels on each switch, because Java
// only ever answers this question ONCE. There, the type string picks a maintainer
// out of the factory registry and every later question — is it idempotent, does
// it validate grouping, can it serve this aggregate — is a method call on the
// maintainer that was already chosen. Go flattened that into several independent
// switches over `idx.Type`, so an alias handled at the dispatch and nowhere else
// would produce an index that builds with the right maintainer and is then
// mis-judged by the idempotency check, the grouping validator and the aggregate
// matcher. Routing every one of those switches through here keeps the alias in a
// single place, which is the property Java gets for free from its registry.
// CanonicalType is canonicalIndexType for callers outside this package — the
// chaos model and its verifier, which re-derive an index's behaviour from its
// type exactly as the five in-package switches do.
//
// They are the same hazard with a worse failure mode. A model that does not
// resolve the alias simply DROPS the index: it tracks no min/max for it, the
// verifier's own switch skips it too, and the pair agrees perfectly about an
// index neither one looked at. That is a safety net reporting green on the
// thing it was not checking, which is worse than a wrong answer.
func (idx *Index) CanonicalType() string {
	return canonicalIndexType(idx.Type)
}

func canonicalIndexType(indexType string) string {
	switch indexType {
	case IndexTypeMinEver:
		return IndexTypeMinEverLong
	case IndexTypeMaxEver:
		return IndexTypeMaxEverLong
	default:
		return indexType
	}
}

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
// CreatesDuplicates reports whether the index's root key expression fans out —
// a single record can produce multiple index entries (e.g. an index over a
// repeated/collection field). Ports Java's
// index.getRootExpression().createsDuplicates(); such an index does NOT produce
// distinct records (DistinctRecordsProperty). Returns false when the root
// expression is unset. FAILS CLOSED (conservatively duplicating) for an
// UNRECOGNIZED root type so the M4 DistinctRecords signal never mis-reports an
// unknown-fan-out index as distinct (which would elide a required DISTINCT).
func (i *Index) CreatesDuplicates() bool {
	if i.RootExpression == nil {
		return false
	}
	// Java builds a ValueIndexScanMatchCandidate for VALUE and VERSION indexes
	// and governs their scan-time distinctness by the root key expression's
	// fan-out (index.getRootExpression().createsDuplicates()). A VERSION index is
	// one entry per record (VersionKeyExpression.createsDuplicates()==false), so
	// it produces DISTINCT records — the earlier blanket non-VALUE fail-closed
	// over-reported it as duplicate-producing and lost a DISTINCT elision.
	// createsDuplicatesRec self-protects: an UNRECOGNIZED root returns the
	// `unrecognized=true` fail-closed default, so this can report distinct ONLY
	// when the root is provably non-fan-out.
	//
	// Every OTHER index type that can reach a value-scan candidate may emit
	// multiple entries per record — TEXT tokenizes to one entry per token;
	// multidimensional / time-window-leaderboard have their own scan shapes; a
	// RANK value-scan's distinctness is unverified — so FAIL CLOSED to
	// duplicate-producing for them (the safe direction: identical rows,
	// at most a redundant DISTINCT, never a dropped dedup). aggregate / vector /
	// atomic-mutation indexes are already excluded upstream from value candidates
	// (RANK/PERMUTED-min/max reach the aggregate candidate, not this path).
	switch i.Type {
	case IndexTypeValue, IndexTypeVersion:
		return createsDuplicatesRec(i.RootExpression, true)
	default:
		return true
	}
}

type Index struct {
	Name           string
	Type           string
	RootExpression KeyExpression
	subspaceKey    any
	// useExplicitSubspaceKey records that subspaceKey was CHOSEN rather than
	// defaulted from the name. Java carries the same bit
	// (Index.useExplicitSubspaceKey) and it is not bookkeeping: the builder's
	// counter-based assignment consults it to decide whether it may overwrite
	// the key, and a subspace key is the on-disk prefix of every entry the
	// index owns. Overwriting a chosen one orphans that data.
	//
	// Go could not previously express the distinction, because every
	// constructor seeds subspaceKey with the index name — so "unset" and
	// "deliberately set to the name" are the same value and only a separate
	// bit tells them apart.
	useExplicitSubspaceKey bool
	// subspaceKeyErr is SetSubspaceKey's refusal of a nil key, which Java
	// throws from the setter; every RecordMetaDataBuilder.Build the index was
	// handed to returns it unless a fault recorded earlier in program order
	// (subspaceKeyErrSeq) comes first.
	subspaceKeyErr    error
	subspaceKeyErrSeq uint64
	Options           map[string]string
	// optionOrder is the order keys were first set through SetOption. Java
	// holds an index's options in an ImmutableMap, whose iteration order is
	// insertion order, and Index.toProto writes them in that order
	// (Index.java:131, 661-663), so the stored option list is a function of
	// the call order. OptionKeys emits this order, then any key written into
	// Options directly, sorted, so the stored bytes never depend on Go's map
	// iteration.
	optionOrder         []string
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
	idx := &Index{
		Name:           name,
		Type:           IndexTypePermutedMax,
		RootExpression: rootExpression,
		subspaceKey:    name,
		Options:        make(map[string]string),
	}
	return idx.SetOption(IndexOptionPermutedSize, strconv.Itoa(permutedSize))
}

// NewPermutedMinIndex creates a PERMUTED_MIN index with the given name, root key expression,
// and permuted size. The permuted size specifies how many trailing grouping columns are
// permuted to after the value in the secondary subspace, enabling value-ordered scans.
// Matches Java's new Index(name, rootExpression, IndexTypes.PERMUTED_MIN) with PERMUTED_SIZE_OPTION.
func NewPermutedMinIndex(name string, rootExpression KeyExpression, permutedSize int) *Index {
	idx := &Index{
		Name:           name,
		Type:           IndexTypePermutedMin,
		RootExpression: rootExpression,
		subspaceKey:    name,
		Options:        make(map[string]string),
	}
	return idx.SetOption(IndexOptionPermutedSize, strconv.Itoa(permutedSize))
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
	idx := &Index{
		Name:           name,
		Type:           IndexTypeVector,
		RootExpression: rootExpression,
		subspaceKey:    name,
		Options:        make(map[string]string),
	}
	return idx.SetOption(IndexOptionVectorNumDimensions, fmt.Sprintf("%d", numDimensions))
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
// Matches Java's Index.setSubspaceKey() (Index.java:413-416), including its
// side effect of MARKING the key explicit — see useExplicitSubspaceKey — and
// its normalization of the key (tupleEquivalentValue), so an int32 key is
// stored, and packed, as an int64. Java refuses a nil key with
// RecordCoreArgumentException "Index subspace key cannot be null" and keeps
// the key it had; a typed nil (a nil *big.Int or *FDBRecordVersion) is that
// null too. A Go setter returns the Index for chaining, so the refusal is kept
// on the Index and returned by every RecordMetaDataBuilder.Build that includes
// the index, whether the set came before or after AddIndex. It is STICKY: Java
// throws at the refused call, so no later call of that program runs, and a
// later valid set here does not clear it, and a refused set leaves the index
// as it was (Java normalizes before it assigns or marks anything,
// Index.java:413-416), explicit mark included. Build returns the refusal in
// program order among the builder's own faults, also for an index removed or
// refused as a duplicate after the set.
//
// Divergence, Go-only: an index already inside a built RecordMetaData has no
// Build left to return the refusal. There the refused set records the error
// (SubspaceKeyError reports it) and changes nothing, where Java throws.
func (idx *Index) SetSubspaceKey(key any) *Index {
	normalized := tupleEquivalentValue(key)
	if normalized == nil {
		if idx.subspaceKeyErr == nil {
			idx.subspaceKeyErr = &RecordCoreArgumentError{Message: "Index subspace key cannot be null", IndexName: idx.Name, SubspaceKey: key, HasSubspaceKey: true}
			idx.subspaceKeyErrSeq = nextBuildFaultSeq()
		}
		return idx
	}
	if idx.subspaceKeyErr != nil {
		return idx
	}
	idx.subspaceKey = normalized
	idx.useExplicitSubspaceKey = true
	return idx
}

// SubspaceKeyError returns the refusal a SetSubspaceKey of a nil key recorded
// on this index, or nil. Build returns it for an index it builds; this is
// where the refusal of a set on an index of a built RecordMetaData is read.
func (idx *Index) SubspaceKeyError() error {
	return idx.subspaceKeyErr
}

// HasExplicitSubspaceKey reports whether the subspace key was chosen rather
// than defaulted from the index name.
// Matches Java's Index.hasExplicitSubspaceKey().
func (idx *Index) HasExplicitSubspaceKey() bool {
	return idx.useExplicitSubspaceKey
}

// IsUnique returns whether this index enforces a uniqueness constraint.
// Matches Java's Index.isUnique() which checks IndexOptions.UNIQUE_OPTION.
func (idx *Index) IsUnique() bool {
	return idx.GetBooleanOption(IndexOptionUnique, false)
}

// IsClearWhenZero returns whether this index should clear entries when values reach zero.
// When true, atomic ADD mutations are followed by CompareAndClear(zero) to remove
// stale zero-value entries. Applies to COUNT, COUNT_NOT_NULL, and SUM indexes.
// Matches Java's IndexOptions.CLEAR_WHEN_ZERO.
func (idx *Index) IsClearWhenZero() bool {
	return idx.GetBooleanOption(IndexOptionClearWhenZero, false)
}

// IsAtomicMutationIndex reports whether this index is maintained via FDB atomic
// mutations that store aggregated or running-extremum entries rather than
// per-record values. Such indexes (COUNT/SUM totals, MAX_EVER/MIN_EVER running
// extrema, BITMAP_VALUE bitsets) must never be offered as an ordinary VALUE-index
// scan source: their entries are not the underlying record values, so an ordered
// per-record scan (e.g. a StreamingAgg over the index) reads stale/mislaid data.
//
// Mirrors Java, where AtomicMutationIndexMaintainerFactory (count, count_not_null,
// count_updates, sum, max_ever_*, min_ever_*, max_ever_version) and
// BitmapValueIndexMaintainerFactory never call expandValueIndexMatchCandidate —
// these types only ever reach the aggregate expansion path (or none), and their
// maintainers reject a BY_VALUE scan at runtime. VALUE, VERSION, RANK, and the
// PERMUTED_MIN/MAX types are deliberately excluded here: they are genuinely
// value-scannable (their Java maintainers support BY_VALUE).
func (idx *Index) IsAtomicMutationIndex() bool {
	switch canonicalIndexType(idx.Type) {
	case IndexTypeCount, IndexTypeCountNotNull, IndexTypeCountUpdates,
		IndexTypeSum,
		IndexTypeMaxEverLong, IndexTypeMinEverLong,
		IndexTypeMaxEverTuple, IndexTypeMinEverTuple,
		IndexTypeMaxEverVersion,
		IndexTypeBitmapValue:
		return true
	default:
		return false
	}
}

// SetClearWhenZero enables or disables the clear-when-zero behavior.
// Matches Java's IndexOptions.CLEAR_WHEN_ZERO.
func (idx *Index) SetClearWhenZero(clear bool) *Index {
	if clear {
		idx.SetOption(IndexOptionClearWhenZero, "true")
	} else {
		delete(idx.Options, IndexOptionClearWhenZero)
	}
	return idx
}

// GetBooleanOption returns the boolean value of an index option.
// Returns the default value if the option is not set, and otherwise
// Boolean.valueOf's reading: "true" in any case is true, anything else false.
// Matches Java's Index.getBooleanOption(String, boolean).
func (idx *Index) GetBooleanOption(key string, defaultVal bool) bool {
	v, ok := idx.Options[key]
	if !ok {
		return defaultVal
	}
	return javaParseBoolean(v)
}

// SetPredicate sets a filter predicate for sparse/filtered indexes.
// Only records where the predicate returns true will have index entries.
// Note: programmatic Go predicates cannot be serialized to proto. Use
// SetPredicateProto for predicates that must survive metadata round-tripping.
func (idx *Index) SetPredicate(p IndexPredicate) *Index {
	idx.Predicate = p
	// A programmatic predicate has no serializable representation. Clear any
	// previously installed proto so replacing (or clearing) a deserialized
	// predicate cannot retain stale serialized semantics on the next metadata
	// round trip.
	idx.predicateProto = nil
	return idx
}

// SetPredicateProto sets a predicate from a proto message. This both stores
// the proto for round-tripping and builds an evaluator function.
func (idx *Index) SetPredicateProto(p *gen.Predicate) error {
	if p == nil {
		idx.predicateProto = nil
		idx.Predicate = nil
		return nil
	}
	// Take ownership before compiling. predicateFromProto may build closures
	// from the message, so retaining caller-owned mutable protobuf storage would
	// let a post-Set mutation desynchronize serialized semantics from the
	// already-published evaluator.
	owned := proto.Clone(p).(*gen.Predicate)
	fn, err := predicateFromProto(owned)
	if err != nil {
		// Compile before publishing either representation. A rejected proto must
		// leave the previously valid predicate intact, not create a half-updated
		// sparse index whose evaluator and serialization disagree.
		return fmt.Errorf("index %s: predicate: %w", idx.Name, err)
	}
	idx.predicateProto = owned
	idx.Predicate = fn
	return nil
}

// GetPredicateProto returns the proto representation of the predicate, if any.
// Returns nil for programmatic Go predicates set via SetPredicate(). The
// returned message is a defensive clone: callers cannot mutate the evaluator's
// serialized authority through this accessor.
func (idx *Index) GetPredicateProto() *gen.Predicate {
	if idx == nil || idx.predicateProto == nil {
		return nil
	}
	return proto.Clone(idx.predicateProto).(*gen.Predicate)
}

// HasPredicate reports whether this is a sparse/filtered index. Check both
// representations: programmatic Go predicates have only Predicate, while
// serialized metadata carries predicateProto (and normally its compiled
// evaluator too). Keeping one authority prevents planner adapters from
// accidentally admitting one representation but not the other.
func (idx *Index) HasPredicate() bool {
	return idx != nil && (idx.Predicate != nil || idx.predicateProto != nil)
}

// HasFilteringPredicate reports whether this index's predicate can actually
// reject a record — the question anything reasoning about index COMPLETENESS
// has to ask, as opposed to HasPredicate's question of whether a predicate is
// declared at all. A predicate that is a proved tautology rejects nothing, so
// the index holds an entry for every record and a scan over it is not lossy;
// Java draws the same line at ValueIndexExpansionVisitor.java:141, where a
// tautological index predicate is simply not attached to the match candidate
// and the index stays a full value index.
//
// Only the serialized representation can be reasoned about. A programmatic Go
// predicate is an opaque closure — `func(proto.Message) bool { return true }`
// is a tautology no one can prove — so it counts as filtering and fails closed.
func (idx *Index) HasFilteringPredicate() bool {
	return idx.HasPredicate() && !predicateProtoIsTautology(idx.predicateProto)
}

// equalsJava is Java's Index.equals (Index.java:695-711): name, type, root
// expression, subspace key, added and last-modified versions, primary-key
// component positions, options and predicate. OnlineIndexer's builder removes
// duplicate targets with it (a HashSet, OnlineIndexer.java:871-874), so two
// objects that differ in any of these are both kept, and the one that is not
// the metadata's own is then refused.
//
// The type is compared as spelled, as Java's type.equals does: the deprecated
// min_ever is not min_ever_long here, although both maintain the same index.
// Java compares subspace keys after normalizing them on assignment, so an
// Integer key equals a Long one and a byte[] key equals another with the same
// content; subspaceKeysEqual is that comparison. The root expressions are
// compared with keyExpressionEquals, Java's KeyExpression.equals.
//
// Java compares predicates with Objects.equals, and of the IndexPredicate
// classes only RowNumberWindowPredicate overrides equals
// (IndexPredicate.java:790-801); And, Or, Not, Constant and Value predicates
// are equal only to themselves. Index's copy constructor shares the predicate
// object, and Index(proto) builds a new one (Index.java:238). Go keeps the
// stored proto per Index and a shallow copy shares it, so a predicate is equal
// when both Indexes hold the same proto, or when both are row-number windows
// with the same ordering field, size, direction and partition fields. A Go
// predicate set with SetPredicate is a closure with no stored proto; Go cannot
// compare closures, not even for identity, so an Index carrying one equals
// only itself.
func (idx *Index) equalsJava(o *Index) bool {
	if idx == o {
		return true
	}
	if idx == nil || o == nil {
		return false
	}
	if idx.Name != o.Name || idx.Type != o.Type ||
		!keyExpressionsEqualNilSafe(idx.RootExpression, o.RootExpression) ||
		!subspaceKeysEqual(idx.subspaceKey, o.subspaceKey) ||
		idx.AddedVersion != o.AddedVersion || idx.LastModifiedVersion != o.LastModifiedVersion {
		return false
	}
	if (idx.primaryKeyComponentPositions == nil) != (o.primaryKeyComponentPositions == nil) ||
		!slices.Equal(idx.primaryKeyComponentPositions, o.primaryKeyComponentPositions) {
		return false
	}
	if len(idx.Options) != len(o.Options) {
		return false
	}
	for k, v := range idx.Options {
		if w, ok := o.Options[k]; !ok || w != v {
			return false
		}
	}
	switch {
	case idx.predicateProto != nil || o.predicateProto != nil:
		if idx.predicateProto == o.predicateProto {
			return true
		}
		return rowNumberWindowPredicatesEqual(rowNumberWindowOf(idx.predicateProto), rowNumberWindowOf(o.predicateProto))
	default:
		return idx.Predicate == nil && o.Predicate == nil
	}
}

// rowNumberWindowOf returns p's row-number window when Java's
// IndexPredicate.fromProto would build a RowNumberWindowPredicate from p: it
// takes the first of the and, or, constant, not, value and row-number fields
// that is set (IndexPredicate.java:105-121).
func rowNumberWindowOf(p *gen.Predicate) *gen.RowNumberWindowPredicate {
	if p == nil || p.AndPredicate != nil || p.OrPredicate != nil || p.ConstantPredicate != nil ||
		p.NotPredicate != nil || p.ValuePredicate != nil {
		return nil
	}
	return p.RowNumberWindowPredicate
}

// rowNumberWindowPredicatesEqual is RowNumberWindowPredicate.equals over the
// fields Java builds from the proto (IndexPredicate.java:657-673, :790-801).
// Either being nil means that side is not a row-number window.
func rowNumberWindowPredicatesEqual(a, b *gen.RowNumberWindowPredicate) bool {
	if a == nil || b == nil {
		return false
	}
	if !slices.Equal(a.GetOrderingField(), b.GetOrderingField()) || a.GetSize() != b.GetSize() ||
		a.GetDirection() != b.GetDirection() || len(a.GetPartitionFields()) != len(b.GetPartitionFields()) {
		return false
	}
	for i, path := range a.GetPartitionFields() {
		if !slices.Equal(path.GetField(), b.GetPartitionFields()[i].GetField()) {
			return false
		}
	}
	return true
}

// javaObjectsEqual is Objects.equals over two Go values that stand for Java
// objects: nil equals only nil, and otherwise the values must have the same
// dynamic type, a comparable one, and be ==.
func javaObjectsEqual(a, b any) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	// Value.Comparable checks the dynamic contents too (a comparable struct type
	// holding an interface field that holds a slice is not), so the == below
	// cannot panic.
	va := reflect.ValueOf(a)
	if va.Type() != reflect.TypeOf(b) || !va.Comparable() || !reflect.ValueOf(b).Comparable() {
		return false
	}
	return a == b
}

// PrimaryKeyComponentPositions returns the overlap mapping between index key and primary key.
// nil means no overlap was computed. Matches Java's Index.getPrimaryKeyComponentPositions().
func (idx *Index) PrimaryKeyComponentPositions() []int {
	return idx.primaryKeyComponentPositions
}

// HasPrimaryKeyComponentPositions returns true if the index has computed PK component positions.
//
// NOT SEMANTICALLY IDENTICAL to Java's Index.hasPrimaryKeyComponentPositions(),
// which is `!= null && IntStream.of(...).anyMatch(i -> i >= 0)` -- this drops
// the second conjunct, so the two disagree for a non-nil ALL-NEGATIVE array.
// They agree on everything Build produces, because buildPrimaryKeyComponentPositions
// returns nil rather than an all-negative array (in both engines), so the
// divergent state is unreachable from the builder in production. It IS
// constructible by hand, and metadata_evolution_validator_test.go does exactly
// that -- so "matches Java" is true of the reachable states and false of the
// predicate, which is why this says which.
func (idx *Index) HasPrimaryKeyComponentPositions() bool {
	return idx.primaryKeyComponentPositions != nil
}

// SetOption sets an index option. A key set for the first time takes the next
// position in the stored option order, as Java's RecordLayerIndex.Builder.setOption
// and Index's ImmutableMap do; setting it again changes only its value.
func (idx *Index) SetOption(key, value string) *Index {
	if idx.Options == nil {
		idx.Options = make(map[string]string)
	}
	if !slices.Contains(idx.optionOrder, key) {
		idx.optionOrder = append(idx.optionOrder, key)
	}
	idx.Options[key] = value
	return idx
}

// OptionKeys returns the option keys in stored order: the keys set through
// SetOption in the order they were first set (skipping any since deleted), then
// any key written into Options directly, sorted.
func (idx *Index) OptionKeys() []string {
	keys := make([]string, 0, len(idx.Options))
	for _, k := range idx.optionOrder {
		if _, ok := idx.Options[k]; ok {
			keys = append(keys, k)
		}
	}
	var rest []string
	for k := range idx.Options {
		if !slices.Contains(keys, k) {
			rest = append(rest, k)
		}
	}
	sort.Strings(rest)
	return append(keys, rest...)
}

// GetReplacedByIndexNames returns the names of indexes that replace this one.
// Options with keys starting with "replacedBy" (e.g. "replacedBy", "replacedBy_0")
// have their values as replacement index names, in option order.
// Matches Java's Index.getReplacedByIndexNames().
func (idx *Index) GetReplacedByIndexNames() []string {
	var names []string
	for _, k := range idx.OptionKeys() {
		if strings.HasPrefix(k, IndexOptionReplacedByPrefix) {
			names = append(names, idx.Options[k])
		}
	}
	return names
}

// SetUnique marks this index as enforcing uniqueness.
func (idx *Index) SetUnique() *Index {
	return idx.SetOption(IndexOptionUnique, "true")
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
