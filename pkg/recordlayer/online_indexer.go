package recordlayer

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/birdayz/fdb-record-layer-go/gen"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb/subspace"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb/tuple"
	"google.golang.org/protobuf/proto"
)

// TakeoverType represents allowed build method conversions when resuming an index build
// that was started by a different indexing method.
// Matches Java's OnlineIndexer.IndexingPolicy.TakeoverTypes.
type TakeoverType int

const (
	// TakeoverMultiTargetToSingle allows a single-target BY_RECORDS build to continue
	// a MULTI_TARGET_BY_RECORDS build.
	TakeoverMultiTargetToSingle TakeoverType = iota
	// TakeoverMutualToSingle allows a single-target BY_RECORDS build to continue
	// a MUTUAL_BY_RECORDS build.
	TakeoverMutualToSingle
	// TakeoverByRecordsToMutual allows a MUTUAL_BY_RECORDS build to continue
	// a BY_RECORDS or MULTI_TARGET_BY_RECORDS build.
	TakeoverByRecordsToMutual
)

// IndexingPolicy configures how the OnlineIndexer handles stamp conflicts,
// blocked stamps, and build method conversions.
// Matches Java's OnlineIndexer.IndexingPolicy.
type IndexingPolicy struct {
	// ForceStampOverwrite forces writing the stamp without conflict checks on
	// fresh (non-continued) builds, or allows overwriting on continued builds
	// when no records have been scanned. Used internally during rebuild.
	ForceStampOverwrite bool

	// AllowUnblock permits unblocking a blocked stamp during resume.
	AllowUnblock bool

	// AllowUnblockID restricts unblocking to stamps with a matching blockID.
	// Empty string means any blockID is accepted (when AllowUnblock is true).
	AllowUnblockID string

	// AllowedTakeovers is the set of allowed build method conversions.
	AllowedTakeovers map[TakeoverType]bool
}

// ShouldAllowUnblock returns true if the policy allows unblocking a stamp with the given blockID.
// Matches Java's IndexingPolicy.shouldAllowUnblock().
func (p *IndexingPolicy) ShouldAllowUnblock(stampBlockID string) bool {
	if p == nil || !p.AllowUnblock {
		return false
	}
	return p.AllowUnblockID == "" || p.AllowUnblockID == stampBlockID
}

// ShouldAllowTypeConversionContinue returns true if switching from savedStamp's method
// to newStamp's method is permitted by the policy.
// Matches Java's IndexingPolicy.shouldAllowTypeConversionContinue().
func (p *IndexingPolicy) ShouldAllowTypeConversionContinue(newStamp, savedStamp *gen.IndexBuildIndexingStamp) bool {
	if p == nil || len(p.AllowedTakeovers) == 0 {
		return false
	}
	newMethod := newStamp.GetMethod()
	oldMethod := savedStamp.GetMethod()

	if newMethod == gen.IndexBuildIndexingStamp_BY_RECORDS {
		if oldMethod == gen.IndexBuildIndexingStamp_MULTI_TARGET_BY_RECORDS {
			return p.AllowedTakeovers[TakeoverMultiTargetToSingle]
		}
		if oldMethod == gen.IndexBuildIndexingStamp_MUTUAL_BY_RECORDS {
			return p.AllowedTakeovers[TakeoverMutualToSingle]
		}
	}

	if newMethod == gen.IndexBuildIndexingStamp_MUTUAL_BY_RECORDS {
		if !p.AllowedTakeovers[TakeoverByRecordsToMutual] {
			return false
		}
		if oldMethod == gen.IndexBuildIndexingStamp_MULTI_TARGET_BY_RECORDS {
			// Allow if same set of targets or single target.
			if len(newStamp.GetTargetIndex()) == 1 {
				return true
			}
			newTargets := newStamp.GetTargetIndex()
			oldTargets := savedStamp.GetTargetIndex()
			if len(newTargets) != len(oldTargets) {
				return false
			}
			oldSet := make(map[string]bool, len(oldTargets))
			for _, t := range oldTargets {
				oldSet[t] = true
			}
			for _, t := range newTargets {
				if !oldSet[t] {
					return false
				}
			}
			return true
		}
		if oldMethod == gen.IndexBuildIndexingStamp_BY_RECORDS {
			return len(newStamp.GetTargetIndex()) == 1
		}
	}
	return false
}

// isTypeStampBlocked returns true if the stamp has an active block.
// Matches Java's IndexingBase.isTypeStampBlocked().
func isTypeStampBlocked(stamp *gen.IndexBuildIndexingStamp) bool {
	if !stamp.GetBlock() {
		return false
	}
	expiry := stamp.GetBlockExpireEpochMilliSeconds()
	if expiry == 0 {
		return true // permanent block
	}
	return expiry > uint64(time.Now().UnixMilli())
}

// areSimilarStamps returns true if two stamps differ only in block-related fields.
// Matches Java's IndexingBase.areSimilar().
func areSimilarStamps(a, b *gen.IndexBuildIndexingStamp) bool {
	if proto.Equal(a, b) {
		return true
	}
	return proto.Equal(blocklessStampOf(a), blocklessStampOf(b))
}

// blocklessStampOf returns a copy of the stamp with all block fields cleared.
// Matches Java's IndexingBase.blocklessStampOf().
func blocklessStampOf(stamp *gen.IndexBuildIndexingStamp) *gen.IndexBuildIndexingStamp {
	clone := proto.Clone(stamp).(*gen.IndexBuildIndexingStamp)
	clone.Block = nil
	clone.BlockID = nil
	clone.BlockExpireEpochMilliSeconds = nil
	return clone
}

// IndexBuildState reports the current state of an index build.
// Matches Java's com.apple.foundationdb.record.provider.foundationdb.IndexBuildState.
type IndexBuildState struct {
	State          IndexState
	RecordsScanned *int64 // nil if not WRITE_ONLY or not tracked
	RecordsInTotal *int64 // nil if no COUNT index available
}

// LoadIndexBuildState loads the build state for an index within an open store.
// Matches Java's IndexBuildState.loadIndexBuildStateAsync().
func LoadIndexBuildState(store *FDBRecordStore, index *Index) (*IndexBuildState, error) {
	state := store.GetIndexState(index.Name)
	if state != IndexStateWriteOnly {
		return &IndexBuildState{State: state}, nil
	}

	scanned, err := store.LoadBuildProgress(index)
	if err != nil {
		return nil, fmt.Errorf("load build state: %w", err)
	}

	result := &IndexBuildState{
		State:          state,
		RecordsScanned: &scanned,
	}

	// Try to get total record count (requires a COUNT index).
	total, err := store.GetRecordCount()
	if err == nil {
		result.RecordsInTotal = &total
	}
	// If no COUNT index, RecordsInTotal stays nil (matching Java).

	return result, nil
}

// TimeLimitExceededError is returned when the OnlineIndexer's overall time limit is exceeded.
// Matches Java's com.apple.foundationdb.record.provider.foundationdb.IndexingBase.TimeLimitException.
type TimeLimitExceededError struct {
	TimeLimit time.Duration
	Elapsed   time.Duration
}

func (e *TimeLimitExceededError) Error() string {
	return fmt.Sprintf("online indexer time limit exceeded: limit=%v, elapsed=%v", e.TimeLimit, e.Elapsed)
}

// OnlineIndexer builds indexes on existing data across multiple transactions.
// Each transaction processes a chunk of records, tracks progress via IndexingRangeSet,
// and the build resumes from where it left off if interrupted.
//
// Supports four modes:
//   - BY_RECORDS (default, single target): scans all records in primary key order
//   - BY_INDEX (single target): scans an existing readable VALUE index to find records
//   - MULTI_TARGET_BY_RECORDS: scans records once, builds multiple indexes simultaneously
//   - MUTUAL_BY_RECORDS: concurrent multi-process building with fragment-based work division
//
// Matches Java's OnlineIndexer with IndexingByRecords, IndexingByIndex,
// IndexingMultiTargetByRecords, and IndexingMutuallyByRecords.
type OnlineIndexer struct {
	db               *FDBDatabase
	metaData         *RecordMetaData
	targetIndexes    []*Index // target indexes to build (first = primary for range tracking)
	sourceIndex      *Index   // non-nil = BY_INDEX strategy (single target only)
	subspace         subspace.Subspace
	limit            int
	maxRetries       int               // max retries per range on transient failures (0 = no retries)
	recordsPerSecond int               // inter-transaction rate limit (0 = unlimited, default 10000)
	timeLimit        time.Duration     // overall build time limit (0 = unlimited)
	recordTypes      []string          // record types to index (empty = all types; not allowed with multi-target)
	policy           *IndexingPolicy   // stamp conflict resolution policy (nil = default behavior)
	throttle         *indexingThrottle // adaptive throttle (created at Build time)
	mutual           bool              // true = MUTUAL_BY_RECORDS (concurrent building)
	mutualBoundaries [][]byte          // pre-set fragment boundaries (nil = single fragment)
	leaseLengthMs    int64             // heartbeat lease duration in ms (default 30000)
}

// primaryIndex returns the first target index, which drives range tracking.
func (oi *OnlineIndexer) primaryIndex() *Index {
	return oi.targetIndexes[0]
}

// isMultiTarget returns true if building multiple indexes simultaneously.
func (oi *OnlineIndexer) isMultiTarget() bool {
	return len(oi.targetIndexes) > 1
}

// OnlineIndexerBuilder constructs an OnlineIndexer.
type OnlineIndexerBuilder struct {
	indexer    OnlineIndexer
	singleMode bool // true if SetIndex was used (mutually exclusive with AddTargetIndex)
}

// NewOnlineIndexerBuilder creates a new builder.
func NewOnlineIndexerBuilder() *OnlineIndexerBuilder {
	return &OnlineIndexerBuilder{
		indexer: OnlineIndexer{
			limit:            100,
			recordsPerSecond: 10000, // matches Java DEFAULT_RECORDS_PER_SECOND
		},
	}
}

// SetDatabase sets the FDB database.
func (b *OnlineIndexerBuilder) SetDatabase(db *FDBDatabase) *OnlineIndexerBuilder {
	b.indexer.db = db
	return b
}

// SetMetaData sets the record metadata.
func (b *OnlineIndexerBuilder) SetMetaData(md *RecordMetaData) *OnlineIndexerBuilder {
	b.indexer.metaData = md
	return b
}

// SetIndex sets a single target index to build. Mutually exclusive with
// AddTargetIndex/SetTargetIndexes.
func (b *OnlineIndexerBuilder) SetIndex(index *Index) *OnlineIndexerBuilder {
	b.indexer.targetIndexes = []*Index{index}
	b.singleMode = true
	return b
}

// AddTargetIndex adds a target index for multi-target building. Mutually
// exclusive with SetIndex. Matches Java's OnlineIndexer.Builder.addTargetIndex().
func (b *OnlineIndexerBuilder) AddTargetIndex(index *Index) *OnlineIndexerBuilder {
	b.indexer.targetIndexes = append(b.indexer.targetIndexes, index)
	return b
}

// SetTargetIndexes sets multiple target indexes for multi-target building.
// Mutually exclusive with SetIndex. Matches Java's OnlineIndexer.Builder.setTargetIndexes().
func (b *OnlineIndexerBuilder) SetTargetIndexes(indexes []*Index) *OnlineIndexerBuilder {
	b.indexer.targetIndexes = indexes
	return b
}

// SetSubspace sets the store subspace.
func (b *OnlineIndexerBuilder) SetSubspace(ss subspace.Subspace) *OnlineIndexerBuilder {
	b.indexer.subspace = ss
	return b
}

// SetLimit sets the maximum number of records to process per transaction.
func (b *OnlineIndexerBuilder) SetLimit(limit int) *OnlineIndexerBuilder {
	b.indexer.limit = limit
	return b
}

// SetRecordTypes sets which record types to index. If empty, indexes all types
// that have this index defined. Not allowed with multi-target building.
func (b *OnlineIndexerBuilder) SetRecordTypes(types ...string) *OnlineIndexerBuilder {
	b.indexer.recordTypes = types
	return b
}

// SetSourceIndex sets the source index for the BY_INDEX strategy. The source
// index must be a READABLE VALUE index whose root expression does not create
// duplicates. Both source and target must apply to exactly one record type,
// and the target's type must be a superset of the source's.
// Not allowed with multi-target building.
// Matches Java's OnlineIndexer.Builder.setSourceIndex().
func (b *OnlineIndexerBuilder) SetSourceIndex(index *Index) *OnlineIndexerBuilder {
	b.indexer.sourceIndex = index
	return b
}

// SetMaxRetries sets the maximum number of retries per range on transient FDB errors.
// When a range build fails with a transient error, the indexer retries with a halved limit.
// Default is 0 (no retries — errors propagate immediately).
// Matches Java's OnlineIndexer.Builder.setMaxRetries() (default 100 in Java).
func (b *OnlineIndexerBuilder) SetMaxRetries(maxRetries int) *OnlineIndexerBuilder {
	b.indexer.maxRetries = maxRetries
	return b
}

// SetTimeLimit sets the overall time limit for the entire build. If the build exceeds
// this duration, BuildIndex returns a TimeLimitExceededError after the current range
// completes. Zero means unlimited (default).
// Matches Java's OnlineIndexer.Builder.setTimeLimitMilliseconds().
func (b *OnlineIndexerBuilder) SetTimeLimit(d time.Duration) *OnlineIndexerBuilder {
	b.indexer.timeLimit = d
	return b
}

// SetRecordsPerSecond sets the inter-transaction rate limit. The indexer will
// sleep between transactions to maintain approximately this rate. Default is
// 10,000 records/second (matching Java). Set to 0 for unlimited.
// Matches Java's OnlineIndexer.Builder.setRecordsPerSecond().
func (b *OnlineIndexerBuilder) SetRecordsPerSecond(rps int) *OnlineIndexerBuilder {
	b.indexer.recordsPerSecond = rps
	return b
}

// SetPolicy sets the indexing policy for stamp conflict resolution.
// Matches Java's OnlineIndexer.Builder.setIndexingPolicy().
func (b *OnlineIndexerBuilder) SetPolicy(policy *IndexingPolicy) *OnlineIndexerBuilder {
	b.indexer.policy = policy
	return b
}

// SetMutualIndexing enables concurrent/mutual index building mode.
// Multiple indexer processes can run simultaneously, each building different
// fragments of the key space. Fragments are determined by FDB shard boundaries.
// Matches Java's OnlineIndexer.Builder.setMutualIndexing().
func (b *OnlineIndexerBuilder) SetMutualIndexing() *OnlineIndexerBuilder {
	b.indexer.mutual = true
	return b
}

// SetMutualIndexingBoundaries enables mutual mode with pre-set fragment boundaries.
// Each boundary is a packed tuple representing a primary key at which to split
// the key space. If nil/empty, the entire key space is treated as a single fragment.
// Matches Java's OnlineIndexer.Builder.setMutualIndexingBoundaries().
func (b *OnlineIndexerBuilder) SetMutualIndexingBoundaries(boundaries [][]byte) *OnlineIndexerBuilder {
	b.indexer.mutual = true
	b.indexer.mutualBoundaries = boundaries
	return b
}

// SetLeaseLengthMs sets the heartbeat lease duration in milliseconds. If a
// heartbeat is not updated within this duration, the process is presumed dead.
// Default is 30000 (30 seconds). Only relevant for non-mutual mode; in mutual
// mode heartbeats are written but not checked.
// Matches Java's OnlineIndexer.IndexingPolicy.setLeaseLengthMillis().
func (b *OnlineIndexerBuilder) SetLeaseLengthMs(ms int64) *OnlineIndexerBuilder {
	b.indexer.leaseLengthMs = ms
	return b
}

// Build creates the OnlineIndexer.
// Matches Java's OnlineIndexer.Builder.build(): validates, deduplicates, and sorts
// target indexes by name (the alphabetically-first index becomes the "primary"
// that drives range tracking).
func (b *OnlineIndexerBuilder) Build() (*OnlineIndexer, error) {
	if b.indexer.db == nil {
		return nil, fmt.Errorf("online indexer: database is required")
	}
	if b.indexer.metaData == nil {
		return nil, fmt.Errorf("online indexer: metadata is required")
	}
	if len(b.indexer.targetIndexes) == 0 {
		return nil, fmt.Errorf("online indexer: at least one target index is required")
	}
	if b.indexer.subspace == nil {
		return nil, fmt.Errorf("online indexer: subspace is required")
	}
	if b.indexer.limit <= 0 {
		b.indexer.limit = 100
	}

	// Deduplicate target indexes by name (matches Java's HashSet dedup).
	seen := make(map[string]bool, len(b.indexer.targetIndexes))
	deduped := make([]*Index, 0, len(b.indexer.targetIndexes))
	for _, idx := range b.indexer.targetIndexes {
		if !seen[idx.Name] {
			seen[idx.Name] = true
			deduped = append(deduped, idx)
		}
	}
	b.indexer.targetIndexes = deduped

	// Validate all target indexes exist in metadata (matches Java's OnlineIndexer.Builder).
	md := b.indexer.metaData
	for _, idx := range b.indexer.targetIndexes {
		if md.GetIndex(idx.Name) == nil {
			return nil, fmt.Errorf("online indexer: index %q not contained within specified metadata", idx.Name)
		}
	}

	// Sort target indexes by name so primary index selection is deterministic.
	// Matches Java's OnlineIndexer.Builder.validateIndexSetting():
	// targetIndexes.sort(Comparator.comparing(Index::getName))
	sort.Slice(b.indexer.targetIndexes, func(i, j int) bool {
		return b.indexer.targetIndexes[i].Name < b.indexer.targetIndexes[j].Name
	})

	isMulti := len(b.indexer.targetIndexes) > 1

	// Mutual exclusivity: SetIndex vs multi-target.
	if b.singleMode && isMulti {
		return nil, fmt.Errorf("online indexer: SetIndex may not be used when other target indexes are set")
	}

	// Multi-target restrictions (matches Java's IndexingCommon).
	if isMulti || b.indexer.mutual {
		if len(b.indexer.recordTypes) > 0 {
			return nil, fmt.Errorf("online indexer: preset record types not allowed with multi-target/mutual indexing")
		}
		if b.indexer.sourceIndex != nil {
			return nil, fmt.Errorf("online indexer: source index (BY_INDEX) not allowed with multi-target/mutual indexing")
		}
	}

	if b.indexer.sourceIndex != nil {
		if err := b.validateSourceIndex(); err != nil {
			return nil, err
		}
	}

	// Default lease length: 30 seconds.
	if b.indexer.leaseLengthMs == 0 {
		b.indexer.leaseLengthMs = 30_000
	}

	// Create adaptive throttle (matches Java's IndexingThrottle initialization)
	b.indexer.throttle = newIndexingThrottle(b.indexer.limit, b.indexer.maxRetries, b.indexer.recordsPerSecond)

	return &b.indexer, nil
}

// validateSourceIndex checks that the source index is valid for BY_INDEX building.
// Matches Java's IndexingByIndex.validateSourceAndTargetIndexes().
func (b *OnlineIndexerBuilder) validateSourceIndex() error {
	src := b.indexer.sourceIndex
	tgt := b.indexer.targetIndexes[0]
	md := b.indexer.metaData

	// Source must be a VALUE index.
	if src.Type != IndexTypeValue {
		return fmt.Errorf("online indexer: source index %q must be a VALUE index, got %q", src.Name, src.Type)
	}

	// Source root expression must not create duplicates.
	if createsDuplicates(src.RootExpression) {
		return fmt.Errorf("online indexer: source index %q root expression creates duplicates", src.Name)
	}

	// Both source and target must apply to exactly one record type.
	srcTypes := indexRecordTypes(md, src)
	tgtTypes := indexRecordTypes(md, tgt)

	if len(srcTypes) != 1 {
		return fmt.Errorf("online indexer: source index %q must apply to exactly 1 record type, got %d", src.Name, len(srcTypes))
	}
	if len(tgtTypes) != 1 {
		return fmt.Errorf("online indexer: target index %q must apply to exactly 1 record type, got %d", tgt.Name, len(tgtTypes))
	}

	// Target's record type must be a superset of source's.
	if srcTypes[0] != tgtTypes[0] {
		return fmt.Errorf("online indexer: target index type %q does not cover source index type %q", tgtTypes[0], srcTypes[0])
	}

	return nil
}

// indexRecordTypes returns the record type names that have this index defined.
// Returns all types for universal indexes.
func indexRecordTypes(md *RecordMetaData, idx *Index) []string {
	for _, uIdx := range md.GetUniversalIndexes() {
		if uIdx.Name == idx.Name {
			// Universal — applies to all types.
			var names []string
			for _, rt := range md.RecordTypes() {
				names = append(names, rt.Name)
			}
			return names
		}
	}
	var names []string
	for _, rt := range md.RecordTypes() {
		for _, rtIdx := range md.GetIndexesForRecordType(rt.Name) {
			if rtIdx.Name == idx.Name {
				names = append(names, rt.Name)
				break
			}
		}
	}
	return names
}

// BuildIndex runs the full index build: marks WRITE_ONLY, builds all records,
// then marks READABLE. Returns the number of records indexed.
// Matches Java's OnlineIndexer.buildIndex().
func (oi *OnlineIndexer) BuildIndex(ctx context.Context) (int64, error) {
	startTime := time.Now()

	// Step 1: Mark all target indexes as WRITE_ONLY.
	if err := oi.markWriteOnly(ctx); err != nil {
		return 0, fmt.Errorf("mark write-only: %w", err)
	}

	// Step 2: Build in chunks across multiple transactions.
	if oi.mutual {
		return oi.buildIndexMutual(ctx, startTime)
	}

	buildFn := oi.buildRange
	if oi.sourceIndex != nil {
		buildFn = oi.buildRangeByIndex
	}

	var totalRecords int64
	for {
		n, hasMore, err := oi.buildRangeWithRetries(ctx, buildFn)
		if err != nil {
			return totalRecords, fmt.Errorf("build range: %w", err)
		}
		totalRecords += n
		if !hasMore {
			break
		}

		// Check time limit after each range.
		if oi.timeLimit > 0 {
			elapsed := time.Since(startTime)
			if elapsed >= oi.timeLimit {
				return totalRecords, &TimeLimitExceededError{
					TimeLimit: oi.timeLimit,
					Elapsed:   elapsed,
				}
			}
		}
	}

	// Step 3: Mark all target indexes as READABLE.
	if err := oi.markReadable(ctx); err != nil {
		return totalRecords, fmt.Errorf("mark readable: %w", err)
	}

	return totalRecords, nil
}

// buildIndexMutual runs the mutual (concurrent) build path.
//
// LIMITATION: Multi-target mutual builds should only target idempotent indexes
// (VALUE, RANK, VERSION, etc.). Non-idempotent indexes (COUNT, SUM, COUNT_UPDATES)
// can double-count when two concurrent builders process the same fragment and one's
// InsertRange(requireEmpty) detects the contest after index entries are already written.
// Idempotent indexes are unaffected (SET is idempotent). Build non-idempotent indexes
// with a separate single-target indexer.
func (oi *OnlineIndexer) buildIndexMutual(ctx context.Context, startTime time.Time) (int64, error) {
	mutual, err := newMutualIndexBuilder(oi)
	if err != nil {
		return 0, fmt.Errorf("init mutual builder: %w", err)
	}
	defer mutual.cleanup(ctx)

	var totalRecords int64
	for {
		// Use buildRangeWithRetries for adaptive throttling + retry on transient
		// FDB errors, matching the BY_RECORDS path. Without this, mutual builds
		// had no limit adjustment on failure and no rate limiting.
		n, hasMore, err := oi.buildRangeWithRetries(ctx, func(ctx context.Context) (int64, bool, error) {
			return mutual.buildMutual(ctx)
		})
		if err != nil {
			return totalRecords, fmt.Errorf("mutual build range: %w", err)
		}
		totalRecords += n
		if !hasMore {
			break
		}

		// Check time limit.
		if oi.timeLimit > 0 {
			elapsed := time.Since(startTime)
			if elapsed >= oi.timeLimit {
				return totalRecords, &TimeLimitExceededError{
					TimeLimit: oi.timeLimit,
					Elapsed:   elapsed,
				}
			}
		}
	}

	// Mark all target indexes as READABLE.
	if err := oi.markReadable(ctx); err != nil {
		return totalRecords, fmt.Errorf("mark readable: %w", err)
	}

	return totalRecords, nil
}

// shouldLessenWork returns true if the error is a transient FDB error indicating
// the transaction did too much work (too large, timed out, conflicted, etc.).
// Only these errors warrant retrying with a smaller limit.
//
// Ports Java's IndexingThrottle.lessenWorkCodes (IndexingThrottle.java:64-70)
// 1:1, with codes from the FDB error table (flow/error_definitions.h):
// TIMED_OUT(1004), TRANSACTION_TOO_OLD(1007), NOT_COMMITTED(1020),
// TRANSACTION_TIMED_OUT(1031), COMMIT_READ_INCOMPLETE(2002),
// TRANSACTION_TOO_LARGE(2101). The previous list had the right error NAMES but
// wrong NUMBERS — 1028 is new_coordinators_timed_out, 1039 is
// cluster_version_changed, 2501 is not transaction_timed_out — and was missing
// 1004/2002/2101, so a transaction_too_large(2101) batch was not retried with a
// smaller limit (the bug surfaced once the client started enforcing the txn-size
// limit; see RFC-067).
func shouldLessenWork(err error) bool {
	var fdbErr fdb.Error
	if !errors.As(err, &fdbErr) {
		return false
	}
	switch fdbErr.Code {
	case 1004, // timed_out
		1007, // transaction_too_old
		1020, // not_committed (conflict)
		1031, // transaction_timed_out
		2002, // commit_read_incomplete
		2101: // transaction_too_large
		return true
	}
	return false
}

// buildRangeWithRetries wraps a buildRange call with retry logic.
// On transient FDB failures indicating the transaction was too large/slow,
// halves the limit and retries up to maxRetries times. Permanent errors
// are returned immediately without retry.
// Matches Java's IndexingThrottle.mayRetryAfterHandlingException().
func (oi *OnlineIndexer) buildRangeWithRetries(ctx context.Context, buildFn func(context.Context) (int64, bool, error)) (int64, bool, error) {
	if oi.throttle == nil || oi.maxRetries <= 0 {
		return buildFn(ctx)
	}

	for attempt := 0; ; attempt++ {
		// Apply adaptive limit from throttle
		oi.limit = oi.throttle.getLimit()

		// Rate limit between transactions
		oi.throttle.waitForRateLimit()

		n, hasMore, err := buildFn(ctx)
		if err == nil {
			oi.throttle.handleSuccess(int(n))
			return n, hasMore, nil
		}
		if !oi.throttle.mayRetryAfterHandlingException(err, attempt, int(n)) {
			return n, hasMore, err
		}
		// Throttle has decreased the limit — retry
	}
}

// markWriteOnly transitions all target indexes to WRITE_ONLY state.
//
// For single-target: matches Java's IndexingBase.handleIndexingState().
// For multi-target: matches Java's IndexingMultiTargetByRecords, where the
// primary index (first in list) drives resume detection and all indexes must
// be in a consistent state.
//
// Note: Java's OnlineIndexer opens stores with IndexMaintenanceFilter.NONE
// (no auto-rebuild), so it can skip READABLE indexes. Our openStore() uses
// plain Open() which auto-rebuilds via checkPossiblyRebuild, ensuring new
// indexes are properly detected and transitioned to WRITE_ONLY/DISABLED.
func (oi *OnlineIndexer) markWriteOnly(ctx context.Context) error {
	_, err := oi.db.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
		store, err := oi.openStore(rtx)
		if err != nil {
			return nil, err
		}

		newStamp := oi.buildIndexingStamp()
		primary := oi.primaryIndex()
		continuedBuild := store.IsIndexWriteOnly(primary.Name)

		if !continuedBuild {
			// Fresh start: clear all target indexes and mark WRITE_ONLY.
			// Java's enforceStampOverwrite() is unnecessary here because
			// clearIndexData removes the stamp, so setIndexingTypeOrThrow
			// will write unconditionally (savedStamp=nil, continuedBuild=false).
			for _, idx := range oi.targetIndexes {
				if _, err := store.ClearAndMarkIndexWriteOnly(idx.Name); err != nil {
					return nil, err
				}
			}
		}

		// For continued builds: validates stamp compatibility and resumes.
		// For fresh starts: writes the new stamp (no saved stamp after clear).
		return nil, oi.setIndexingTypeOrThrow(store, continuedBuild, newStamp)
	})
	return err
}

// setIndexingTypeOrThrow implements Java's IndexingBase.setIndexingTypeOrThrow().
// It validates whether the new stamp can be written, considering the saved stamp,
// block state, policy, and build method conversion rules.
func (oi *OnlineIndexer) setIndexingTypeOrThrow(store *FDBRecordStore, continuedBuild bool, newStamp *gen.IndexBuildIndexingStamp) error {
	for _, idx := range oi.targetIndexes {
		if err := oi.setIndexingTypeOrThrowForIndex(store, continuedBuild, idx, newStamp); err != nil {
			return err
		}
	}
	return nil
}

// setIndexingTypeOrThrowForIndex is the per-index stamp resolution logic.
// Matches Java's IndexingBase.setIndexingTypeOrThrow(store, continuedBuild, index, newStamp).
func (oi *OnlineIndexer) setIndexingTypeOrThrowForIndex(store *FDBRecordStore, continuedBuild bool, index *Index, newStamp *gen.IndexBuildIndexingStamp) error {
	policy := oi.policy

	// Step 1: forceStampOverwrite + fresh session = no questions asked.
	if policy != nil && policy.ForceStampOverwrite && !continuedBuild {
		return store.SaveIndexingTypeStamp(index, newStamp)
	}

	// Step 2: Load saved stamp.
	savedStamp, err := store.LoadIndexingTypeStamp(index)
	if err != nil {
		return err
	}

	// Step 3: No saved stamp.
	if savedStamp == nil {
		if continuedBuild && newStamp.GetMethod() != gen.IndexBuildIndexingStamp_BY_RECORDS {
			// Backward compatibility: maybe continuing an old BY_RECORDS session that
			// didn't write stamps. Check if any records have been scanned.
			// Matches Java's throwAsByRecordsUnlessNoRecordWasScanned().
			if !isWriteOnlyButNoRecordScanned(store, store.context, index) {
				// Records were scanned under old BY_RECORDS — reject non-BY_RECORDS takeover.
				fakeSavedStamp := &gen.IndexBuildIndexingStamp{
					Method: gen.IndexBuildIndexingStamp_BY_RECORDS.Enum(),
				}
				return oi.newPartlyBuiltError(fakeSavedStamp, newStamp, index,
					"this index was partly built by records (no stamp)")
			}
		}
		// Fresh session or BY_RECORDS or no records scanned: write stamp.
		return store.SaveIndexingTypeStamp(index, newStamp)
	}

	// Step 4: Exact match — nothing to do.
	if proto.Equal(savedStamp, newStamp) {
		return nil
	}

	// Step 5: Check if blocked.
	if isTypeStampBlocked(savedStamp) {
		if policy == nil || !policy.ShouldAllowUnblock(savedStamp.GetBlockID()) {
			return oi.newPartlyBuiltError(savedStamp, newStamp, index,
				"this index was partly built, and blocked")
		}
	}

	// Step 6: Similar stamps (differ only in block fields).
	if areSimilarStamps(newStamp, savedStamp) {
		return store.SaveIndexingTypeStamp(index, newStamp)
	}

	// Step 7: Type conversion allowed?
	if continuedBuild && policy != nil && policy.ShouldAllowTypeConversionContinue(newStamp, savedStamp) {
		return store.SaveIndexingTypeStamp(index, newStamp)
	}

	// Step 8: forceStampOverwrite + continued build — allow if no records scanned.
	if policy != nil && policy.ForceStampOverwrite {
		if isWriteOnlyButNoRecordScanned(store, store.context, index) {
			return store.SaveIndexingTypeStamp(index, newStamp)
		}
		return oi.newPartlyBuiltError(savedStamp, newStamp, index,
			"this index was partly built by another method")
	}

	// Step 9: Fall through — mismatch, not allowed.
	return oi.newPartlyBuiltError(savedStamp, newStamp, index,
		"this index was partly built by another method")
}

// isWriteOnlyButNoRecordScanned returns true if the index is WRITE_ONLY with a
// completely empty range set (no records have been scanned yet).
// Matches Java's IndexingBase.isWriteOnlyButNoRecordScanned().
func isWriteOnlyButNoRecordScanned(store *FDBRecordStore, rtx *FDBRecordContext, index *Index) bool {
	rangeSet := NewIndexingRangeSet(store.subspace, index)
	missing, err := rangeSet.FirstMissingRange(rtx.Transaction())
	if err != nil {
		return false
	}
	if missing == nil {
		return false // fully built — no missing ranges
	}
	// Empty if the first missing range spans the entire key space.
	return bytes.Equal(missing.Begin, rangeSetFirstKey) && bytes.Equal(missing.End, rangeSetFinalKey)
}

// newPartlyBuiltError creates a PartlyBuiltError with stamp descriptions.
func (oi *OnlineIndexer) newPartlyBuiltError(savedStamp, expectedStamp *gen.IndexBuildIndexingStamp, index *Index, msg string) *PartlyBuiltError {
	return &PartlyBuiltError{
		IndexName:     index.Name,
		SavedStamp:    stampToString(savedStamp),
		ExpectedStamp: stampToString(expectedStamp),
		Message:       msg,
	}
}

// stampToString returns a human-readable representation of a stamp.
func stampToString(stamp *gen.IndexBuildIndexingStamp) string {
	if stamp == nil {
		return "<nil>"
	}
	return stamp.GetMethod().String()
}

// markReadable transitions all target indexes to READABLE (or READABLE_UNIQUE_PENDING
// for unique indexes with violations) after the build completes.
//
// Each index is marked in its own transaction so one failure doesn't block others.
// Matches Java's IndexingBase.markIndexReadable() → forEachTargetIndex() where each
// index uses getRunner().runAsync() (separate transaction per index).
func (oi *OnlineIndexer) markReadable(ctx context.Context) error {
	var firstErr error
	for _, idx := range oi.targetIndexes {
		_, err := oi.db.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := oi.openStore(rtx)
			if err != nil {
				return nil, err
			}
			_, err = store.MarkIndexReadableOrUniquePending(idx.Name)
			return nil, err
		})
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// buildRange processes one chunk of records in a single transaction.
// Returns (recordsProcessed, hasMore, error).
//
// Uses Java's limit+1 look-ahead pattern (IndexingBase.scanPropertiesWithLimits):
// requests limit+1 records, indexes only the first limit, and uses the (limit+1)th
// record's PK as the exclusive range boundary. This prevents boundary records from
// being re-scanned in the next chunk — critical for non-idempotent indexes (COUNT, SUM).
//
// For multi-target: scans records once and updates all target index maintainers per
// record. Range tracking uses the primary index's RangeSet for reading, but inserts
// into all target indexes' RangeSets. Matches Java's IndexingMultiTargetByRecords.
func (oi *OnlineIndexer) buildRange(ctx context.Context) (int64, bool, error) {
	var recordsProcessed int64
	var hasMore bool

	_, err := oi.db.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
		// Reset on retry — previous attempt's values are stale.
		recordsProcessed = 0
		hasMore = false

		store, err := oi.openStore(rtx)
		if err != nil {
			return nil, err
		}

		// Primary index's RangeSet determines the build boundaries.
		primaryRangeSet := NewIndexingRangeSet(store.subspace, oi.primaryIndex())

		// Find first missing range.
		missing, err := primaryRangeSet.FirstMissingRange(rtx.Transaction())
		if err != nil {
			return nil, err
		}
		if missing == nil {
			hasMore = false
			return nil, nil
		}

		// Convert byte boundaries to TupleRange for record scanning.
		var rangeStart, rangeEnd tuple.Tuple
		if !bytes.Equal(missing.Begin, rangeSetFirstKey) {
			rangeStart, err = fastUnpack(missing.Begin)
			if err != nil {
				return nil, fmt.Errorf("unpack range start: %w", err)
			}
		}
		if !bytes.Equal(missing.End, rangeSetFinalKey) {
			rangeEnd, err = fastUnpack(missing.End)
			if err != nil {
				return nil, fmt.Errorf("unpack range end: %w", err)
			}
		}

		// Scan limit+1 records: process up to limit, use the extra as continuation.
		// Idempotent indexes use SNAPSHOT reads (no conflict tracking) for better
		// throughput. Non-idempotent indexes use SERIALIZABLE to prevent double-counting.
		// Matches Java's IndexingBase.scanPropertiesWithLimits().
		scanProps := ForwardScan()
		scanProps.ExecuteProperties.ReturnedRowLimit = saturatingAdd(oi.limit, 1)
		if oi.allTargetIndexesIdempotent() {
			scanProps.ExecuteProperties.IsolationLevel = IsolationLevelSnapshot
		}

		lowEp := EndpointTypeRangeInclusive
		highEp := EndpointTypeRangeExclusive
		if rangeStart == nil {
			lowEp = EndpointTypeTreeStart
		}
		if rangeEnd == nil {
			highEp = EndpointTypeTreeEnd
		}

		cursor := store.ScanRecordsInRange(rangeStart, rangeEnd, lowEp, highEp, nil, scanProps)

		var scannedCount int
		var extraPK tuple.Tuple

		for rec, iterErr := range Seq2(cursor, ctx) {
			if iterErr != nil {
				return nil, iterErr
			}

			scannedCount++

			if scannedCount > oi.limit {
				extraPK = rec.PrimaryKey
				break
			}

			// Update each target index that applies to this record.
			for _, idx := range oi.targetIndexes {
				if !oi.shouldIndexRecordForIndex(rec, idx) {
					continue
				}
				maintainer, mErr := store.getIndexMaintainer(idx)
				if mErr != nil {
					return nil, fmt.Errorf("index %q get maintainer: %w", idx.Name, mErr)
				}
				if err := maintainer.Update(nil, rec); err != nil {
					return nil, fmt.Errorf("index %q record pk=%v: %w", idx.Name, rec.PrimaryKey, err)
				}
			}
			// Count ALL scanned records, not just indexed ones.
			// Matches Java's IndexingBase.handleCursorResult() which increments
			// recordsScannedCounter for every record regardless of type filtering.
			recordsProcessed++
		}

		// Mark progress in ALL target indexes' RangeSets.
		var rangeBeginBytes, rangeEndBytes []byte
		if rangeStart != nil {
			rangeBeginBytes = rangeStart.Pack()
		}

		if extraPK != nil {
			rangeEndBytes = extraPK.Pack()
			hasMore = true
		} else {
			if rangeEnd != nil {
				rangeEndBytes = rangeEnd.Pack()
			}
			hasMore = !bytes.Equal(missing.End, rangeSetFinalKey)
		}

		for _, idx := range oi.targetIndexes {
			rangeSet := NewIndexingRangeSet(store.subspace, idx)
			if _, err := rangeSet.InsertRange(rtx.Transaction(), rangeBeginBytes, rangeEndBytes, true); err != nil {
				return nil, fmt.Errorf("mark range built for %q: %w", idx.Name, err)
			}
		}

		// Track progress: atomic ADD of records scanned per target index.
		// Matches Java's IndexingBase.tieredMergeAndCommit() progress tracking.
		if recordsProcessed > 0 {
			for _, idx := range oi.targetIndexes {
				store.AddBuildProgress(idx, recordsProcessed)
			}
		}

		return nil, nil
	})

	return recordsProcessed, hasMore, err
}

// buildIndexingStamp creates the IndexBuildIndexingStamp proto for this build.
//
// Single-target BY_RECORDS: method=BY_RECORDS.
// Single-target BY_INDEX: method=BY_INDEX with source index info.
// Multi-target: method=MULTI_TARGET_BY_RECORDS with sorted target index names.
//
// Matches Java's IndexingByRecords/IndexingByIndex/IndexingMultiTargetByRecords
// compileSingleTargetLegacyIndexingTypeStamp()/compileTargetIndexesLegacyIndexingTypeStamp().
func (oi *OnlineIndexer) buildIndexingStamp() *gen.IndexBuildIndexingStamp {
	if oi.sourceIndex != nil {
		return &gen.IndexBuildIndexingStamp{
			Method:                         gen.IndexBuildIndexingStamp_BY_INDEX.Enum(),
			SourceIndexSubspaceKey:         tuple.Tuple{oi.sourceIndex.SubspaceTupleKey()}.Pack(),
			SourceIndexLastModifiedVersion: proto.Int32(int32(oi.sourceIndex.LastModifiedVersion)),
		}
	}

	if len(oi.targetIndexes) == 1 && !oi.mutual {
		return &gen.IndexBuildIndexingStamp{
			Method: gen.IndexBuildIndexingStamp_BY_RECORDS.Enum(),
		}
	}

	// Multi-target or mutual: index names in stamp (already sorted by Build()).
	// Matches Java's compileTargetIndexesLegacyIndexingTypeStamp().
	names := make([]string, len(oi.targetIndexes))
	for i, idx := range oi.targetIndexes {
		names[i] = idx.Name
	}

	method := gen.IndexBuildIndexingStamp_MULTI_TARGET_BY_RECORDS
	if oi.mutual {
		method = gen.IndexBuildIndexingStamp_MUTUAL_BY_RECORDS
	}

	return &gen.IndexBuildIndexingStamp{
		Method:      method.Enum(),
		TargetIndex: names,
	}
}

// buildRangeByIndex processes one chunk via the BY_INDEX strategy: scans the
// source index to find records, then feeds them to the target index maintainer.
// Range tracking uses source index entry keys (not primary keys).
// Only valid for single-target builds.
// Matches Java's IndexingByIndex.buildRangeOnly().
func (oi *OnlineIndexer) buildRangeByIndex(ctx context.Context) (int64, bool, error) {
	var recordsProcessed int64
	var hasMore bool

	_, err := oi.db.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
		// Reset on retry — previous attempt's values are stale.
		recordsProcessed = 0
		hasMore = false

		store, err := oi.openStore(rtx)
		if err != nil {
			return nil, err
		}

		// Validate source index is still scannable.
		if !store.IsIndexScannable(oi.sourceIndex.Name) {
			return nil, fmt.Errorf("online indexer: source index %q is not scannable", oi.sourceIndex.Name)
		}

		// FormatVersion 10 check: non-idempotent indexes cannot be built from a source
		// index on stores with format version < CHECK_INDEX_BUILD_TYPE_DURING_UPDATE.
		// On older format versions, UpdateWhileWriteOnly uses primary key range set checks
		// which are incorrect for source-index-based builds.
		// Matches Java's IndexingByIndex.validateSourceAndTargetIndexes().
		if store.GetFormatVersion() < formatVersionCheckIndexBuildType {
			if !isIndexIdempotent(oi.primaryIndex()) {
				return nil, fmt.Errorf("online indexer: cannot build non-idempotent index %q from source index on format version %d (requires >= %d)",
					oi.primaryIndex().Name, store.GetFormatVersion(), formatVersionCheckIndexBuildType)
			}
		}

		rangeSet := NewIndexingRangeSet(store.subspace, oi.primaryIndex())

		// Find first missing range.
		missing, err := rangeSet.FirstMissingRange(rtx.Transaction())
		if err != nil {
			return nil, err
		}
		if missing == nil {
			hasMore = false
			return nil, nil
		}

		// Convert byte boundaries to TupleRange for source index scanning.
		var rangeStart, rangeEnd tuple.Tuple
		if !bytes.Equal(missing.Begin, rangeSetFirstKey) {
			rangeStart, err = fastUnpack(missing.Begin)
			if err != nil {
				return nil, fmt.Errorf("unpack range start: %w", err)
			}
		}
		if !bytes.Equal(missing.End, rangeSetFinalKey) {
			rangeEnd, err = fastUnpack(missing.End)
			if err != nil {
				return nil, fmt.Errorf("unpack range end: %w", err)
			}
		}

		scanRange := TupleRange{
			Low:          rangeStart,
			High:         rangeEnd,
			LowEndpoint:  EndpointTypeRangeInclusive,
			HighEndpoint: EndpointTypeRangeExclusive,
		}
		if rangeStart == nil {
			scanRange.LowEndpoint = EndpointTypeTreeStart
		}
		if rangeEnd == nil {
			scanRange.HighEndpoint = EndpointTypeTreeEnd
		}

		scanProps := ForwardScan()
		scanProps.ExecuteProperties.ReturnedRowLimit = saturatingAdd(oi.limit, 1)
		if oi.allTargetIndexesIdempotent() {
			scanProps.ExecuteProperties.IsolationLevel = IsolationLevelSnapshot
		}

		cursor := store.ScanIndexRecords(oi.sourceIndex.Name, scanRange, nil, scanProps)

		maintainer, mErr := store.getIndexMaintainer(oi.primaryIndex())
		if mErr != nil {
			return nil, mErr
		}
		var scannedCount int
		var extraKey tuple.Tuple

		for indexedRec, iterErr := range Seq2(cursor, ctx) {
			if iterErr != nil {
				return nil, iterErr
			}

			scannedCount++

			if scannedCount > oi.limit {
				extraKey = indexedRec.IndexEntry.Key
				break
			}

			rec := indexedRec.Record
			// Count ALL scanned records regardless of type filtering.
			// Matches Java's IndexingBase.handleCursorResult().
			recordsProcessed++

			if !oi.shouldIndexRecordForIndex(rec, oi.primaryIndex()) {
				continue
			}

			if err := maintainer.Update(nil, rec); err != nil {
				return nil, fmt.Errorf("index record pk=%v: %w", rec.PrimaryKey, err)
			}
		}

		var rangeBeginBytes, rangeEndBytes []byte
		if rangeStart != nil {
			rangeBeginBytes = rangeStart.Pack()
		}

		if extraKey != nil {
			rangeEndBytes = extraKey.Pack()
			hasMore = true
		} else {
			if rangeEnd != nil {
				rangeEndBytes = rangeEnd.Pack()
			}
			hasMore = !bytes.Equal(missing.End, rangeSetFinalKey)
		}

		_, err = rangeSet.InsertRange(rtx.Transaction(), rangeBeginBytes, rangeEndBytes, true)
		if err != nil {
			return nil, fmt.Errorf("mark range built: %w", err)
		}

		// Track progress: atomic ADD of records scanned.
		if recordsProcessed > 0 {
			store.AddBuildProgress(oi.primaryIndex(), recordsProcessed)
		}

		return nil, nil
	})

	return recordsProcessed, hasMore, err
}

// isIndexTypeIdempotent returns true if the given index type produces idempotent
// updates. Idempotent indexes can safely use SNAPSHOT reads during online builds
// because re-applying the same operation produces the same result.
// Matches Java's IndexMaintainer.isIdempotent().
func isIndexIdempotent(index *Index) bool {
	switch index.Type {
	case IndexTypeValue,
		IndexTypeMinEverLong, IndexTypeMaxEverLong,
		IndexTypeMinEverTuple, IndexTypeMaxEverTuple,
		IndexTypeMaxEverVersion, IndexTypeVersion,
		IndexTypePermutedMin, IndexTypePermutedMax,
		IndexTypeText:
		return true
	case IndexTypeRank:
		// RANK is idempotent only when !CountDuplicates.
		// Matches Java's RankIndexMaintainer.isIdempotent().
		return index.Options[IndexOptionRankCountDuplicates] != "true"
	case IndexTypeCount, IndexTypeCountNotNull, IndexTypeCountUpdates, IndexTypeSum:
		return false
	default:
		return false // conservative default
	}
}

// allTargetIndexesIdempotent returns true if all target indexes are idempotent.
// Matches Java's IndexMaintainer.isIdempotent() per index.
func (oi *OnlineIndexer) allTargetIndexesIdempotent() bool {
	for _, idx := range oi.targetIndexes {
		if !isIndexIdempotent(idx) {
			return false
		}
	}
	return true
}

// shouldIndexRecordForIndex checks if a record should be indexed by a specific
// index, based on record type matching.
func (oi *OnlineIndexer) shouldIndexRecordForIndex(rec *FDBStoredRecord[proto.Message], idx *Index) bool {
	if len(oi.recordTypes) > 0 {
		// Preset record types override — applies to all indexes in single-target.
		for _, t := range oi.recordTypes {
			if rec.RecordType.Name == t {
				return true
			}
		}
		return false
	}
	// Check if the record type has this index defined.
	for _, rtIdx := range oi.metaData.GetIndexesForRecordType(rec.RecordType.Name) {
		if rtIdx.Name == idx.Name {
			return true
		}
	}
	for _, uIdx := range oi.metaData.GetUniversalIndexes() {
		if uIdx.Name == idx.Name {
			return true
		}
	}
	return false
}

// openStore opens an FDBRecordStore for the current transaction.
func (oi *OnlineIndexer) openStore(rtx *FDBRecordContext) (*FDBRecordStore, error) {
	return NewStoreBuilder().
		SetContext(rtx).
		SetMetaDataProvider(oi.metaData).
		SetSubspace(oi.subspace).
		Open()
}

// BlockIndex sets the block flag on the indexing stamp for all target indexes.
// A blocked stamp prevents other indexers from continuing the build.
// blockID is an optional identifier for the block reason.
// ttl is the time-to-live for the block; zero means permanent.
// Matches Java's IndexingBase.performIndexingStampOperation(BLOCK, ...).
func (oi *OnlineIndexer) BlockIndex(ctx context.Context, blockID string, ttl time.Duration) error {
	_, err := oi.db.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
		store, err := oi.openStore(rtx)
		if err != nil {
			return nil, err
		}
		for _, idx := range oi.targetIndexes {
			stamp, err := store.LoadIndexingTypeStamp(idx)
			if err != nil {
				return nil, err
			}
			if stamp == nil {
				continue
			}
			stamp.Block = proto.Bool(true)
			if blockID != "" {
				stamp.BlockID = proto.String(blockID)
			} else {
				stamp.BlockID = proto.String("")
			}
			if ttl > 0 {
				stamp.BlockExpireEpochMilliSeconds = proto.Uint64(uint64(time.Now().Add(ttl).UnixMilli()))
			} else {
				stamp.BlockExpireEpochMilliSeconds = nil
			}
			if err := store.SaveIndexingTypeStamp(idx, stamp); err != nil {
				return nil, err
			}
		}
		return nil, nil
	})
	return err
}

// UnblockIndex clears the block flag on the indexing stamp for all target indexes.
// If blockID is non-empty, only unblocks stamps with a matching blockID.
// Matches Java's IndexingBase.performIndexingStampOperation(UNBLOCK, ...).
func (oi *OnlineIndexer) UnblockIndex(ctx context.Context, blockID string) error {
	_, err := oi.db.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
		store, err := oi.openStore(rtx)
		if err != nil {
			return nil, err
		}
		for _, idx := range oi.targetIndexes {
			stamp, err := store.LoadIndexingTypeStamp(idx)
			if err != nil {
				return nil, err
			}
			if stamp == nil || !stamp.GetBlock() {
				continue
			}
			if blockID != "" && stamp.GetBlockID() != blockID {
				continue // ID mismatch — don't unblock
			}
			stamp.Block = proto.Bool(false)
			if err := store.SaveIndexingTypeStamp(idx, stamp); err != nil {
				return nil, err
			}
		}
		return nil, nil
	})
	return err
}

// MarkReadableIfBuilt checks each target index and marks it READABLE if fully built.
// Returns true if all target indexes are now READABLE.
// This is an idempotent operation — safe to call repeatedly.
// Matches Java's IndexingBase.markReadableIfBuilt().
func (oi *OnlineIndexer) MarkReadableIfBuilt(ctx context.Context) (bool, error) {
	allReadable := true
	_, err := oi.db.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
		// Reset on retry — previous attempt's result is stale.
		allReadable = true
		store, err := oi.openStore(rtx)
		if err != nil {
			return nil, err
		}
		for _, idx := range oi.targetIndexes {
			if store.IsIndexReadable(idx.Name) {
				continue
			}
			rangeSet := NewIndexingRangeSet(store.subspace, idx)
			missing, err := rangeSet.FirstMissingRange(rtx.Transaction())
			if err != nil {
				return nil, err
			}
			if missing != nil {
				allReadable = false
				continue
			}
			// Index is fully built — mark readable.
			if _, err := store.MarkIndexReadable(idx.Name); err != nil {
				return nil, err
			}
		}
		return nil, nil
	})
	if err != nil {
		return false, err
	}
	return allReadable, nil
}

// QueryIndexingStamps returns the indexing stamps for all target indexes.
// Returns a map of index name → stamp. Nil stamps are returned as a NONE method stamp.
// Matches Java's IndexingBase.performIndexingStampOperation(QUERY, ...).
func (oi *OnlineIndexer) QueryIndexingStamps(ctx context.Context) (map[string]*gen.IndexBuildIndexingStamp, error) {
	stamps := make(map[string]*gen.IndexBuildIndexingStamp, len(oi.targetIndexes))
	_, err := oi.db.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
		store, err := oi.openStore(rtx)
		if err != nil {
			return nil, err
		}
		for _, idx := range oi.targetIndexes {
			stamp, err := store.LoadIndexingTypeStamp(idx)
			if err != nil {
				return nil, err
			}
			if stamp == nil {
				stamp = &gen.IndexBuildIndexingStamp{
					Method: gen.IndexBuildIndexingStamp_NONE.Enum(),
				}
			}
			stamps[idx.Name] = stamp
		}
		return nil, nil
	})
	if err != nil {
		return nil, err
	}
	return stamps, nil
}
