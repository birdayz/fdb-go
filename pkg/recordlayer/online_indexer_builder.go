// Portions derived from FoundationDB Record Layer (OnlineIndexer.java,
// IndexingBase.java, OnlineIndexOperationConfig.java, IndexingByIndex.java,
// and others),
// Copyright 2015-2018 Apple Inc. and the FoundationDB project authors
// Copyright 2015-2020 Apple Inc. and the FoundationDB project authors
// Copyright 2015-2024 Apple Inc. and the FoundationDB project authors
// Licensed under the Apache License, Version 2.0; translated to Go and modified.

package recordlayer

import (
	"fmt"
	"log/slog"
	"slices"
	"sort"
	"time"

	"fdb.dev/pkg/fdbgo/fdb/subspace"
)

// OnlineIndexerBuilder constructs an OnlineIndexer.
type OnlineIndexerBuilder struct {
	indexer OnlineIndexer
	// setIndexErr is the refusal Java's setIndex throws when targets are already
	// set (OnlineIndexer.java:668-671). Go's builder chains, so the refusal is
	// held here and Build returns it before anything else, where Java's would
	// have been thrown before build() was reached.
	setIndexErr error
}

// NewOnlineIndexerBuilder creates a new builder.
func NewOnlineIndexerBuilder() *OnlineIndexerBuilder {
	return &OnlineIndexerBuilder{
		indexer: OnlineIndexer{
			limit: 100,
			// Java OnlineIndexOperationConfig.Builder defaults maxRetries to
			// DEFAULT_MAX_RETRIES = 100 (OnlineIndexOperationConfig.java:52,251). A 0
			// default would leave every caller that never touches the builder without
			// the adaptive limit-halving, so one transaction_too_large escaping the
			// client's own retry loop aborts an otherwise-resumable build.
			maxRetries:          100,
			recordsPerSecond:    10000, // matches Java DEFAULT_RECORDS_PER_SECOND
			markReadableEnabled: true,  // Java buildIndex() defaults markReadable=true
			// Java DO_NOT_RE_INCREASE_LIMIT, DEFAULT_WRITE_LIMIT_BYTES and
			// DEFAULT_TRANSACTION_TIME_LIMIT (OnlineIndexOperationConfig.java:40-72).
			increaseLimitAfter:     doNotReIncreaseLimit,
			maxWriteLimitBytes:     900_000,
			transactionTimeLimitMs: 4_000,
			// Java DEFAULT_PROGRESS_LOG_INTERVAL = -1 → progress logging is OFF by
			// default; opt in via SetProgressLogIntervalMillis.
			progressLogIntervalMillis: -1,
		},
	}
}

// SetFormatVersion pins the format version the indexer opens its stores at,
// mirroring StoreBuilder.SetFormatVersion. Java gets this for free by reusing the
// caller's record-store builder (IndexingCommon.getRecordStoreBuilder).
func (b *OnlineIndexerBuilder) SetFormatVersion(version int32) *OnlineIndexerBuilder {
	b.indexer.formatVersion = &version
	return b
}

// SetIndexMaintenanceFilter sets the IndexMaintenanceFilter of the stores the
// indexer opens, mirroring StoreBuilder.SetIndexMaintenanceFilter; Java reads it
// off the caller's record-store builder (IndexingCommon.getRecordStoreBuilder).
func (b *OnlineIndexerBuilder) SetIndexMaintenanceFilter(filter IndexMaintenanceFilter) *OnlineIndexerBuilder {
	b.indexer.maintenanceFilter = filter
	return b
}

// SetAllowUniquePendingState opts in to leaving a unique index in
// READABLE_UNIQUE_PENDING when the build finds duplicates, instead of failing the
// build with a RecordIndexUniquenessViolationError.
//
// Java OnlineIndexer.IndexingPolicy.Builder.allowUniquePendingState(boolean)
// (OnlineIndexer.java:1354). Defaults FALSE here as it does there, and even when
// set it is honoured only on a store at format version >=
// READABLE_UNIQUE_PENDING — see OnlineIndexer.shouldAllowUniquePendingState.
//
// Java's javadoc carries the warning this option deserves: "This state might not be
// compatible with older code. Please verify that all instances are up-to-date before
// allowing it."
func (b *OnlineIndexerBuilder) SetAllowUniquePendingState(allow bool) *OnlineIndexerBuilder {
	b.indexer.allowUniquePendingState = allow
	return b
}

// SetDatabase sets the FDB database.
func (b *OnlineIndexerBuilder) SetDatabase(db *FDBDatabase) *OnlineIndexerBuilder {
	b.indexer.db = db
	return b
}

// SetMarkReadable controls whether BuildIndex marks the index readable when the build
// completes (default true). With false, the build populates the index but leaves it
// WRITE_ONLY with its build stamp and progress counter intact — matching Java's
// OnlineIndexer.buildIndex(boolean markReadable).
func (b *OnlineIndexerBuilder) SetMarkReadable(v bool) *OnlineIndexerBuilder {
	b.indexer.markReadableEnabled = v
	return b
}

// SetProgressLogIntervalMillis sets the throttle for the per-range
// "Indexer: Built Range" progress log. Matches Java
// OnlineIndexer.Builder.setProgressLogIntervalMillis():
//   - a negative value (the default, -1) disables progress logging;
//   - 0 logs after every range;
//   - a positive value logs at most once per that many milliseconds.
//
// Events go to the logger set via SetLogger (or slog.Default()) at INFO.
func (b *OnlineIndexerBuilder) SetProgressLogIntervalMillis(ms int64) *OnlineIndexerBuilder {
	b.indexer.progressLogIntervalMillis = ms
	return b
}

// SetLogger sets the slog.Logger that receives the indexer's operational
// progress events. A nil logger (the default) falls back to slog.Default() at
// log time, mirroring the client's WithLogger convention (RFC-097).
func (b *OnlineIndexerBuilder) SetLogger(logger *slog.Logger) *OnlineIndexerBuilder {
	b.indexer.logger = logger
	return b
}

// SetRecordStoreBuilder is Java's OnlineIndexOperationBaseBuilder.setRecordStoreBuilder:
// the indexer opens every store through a copy of builder's configuration
// (copyBuilder), in its own transaction (the builder's context is dropped), so
// the stores carry the caller's configuration, the serializer included. The
// builder's metadata, subspace, format version and maintenance filter become
// the indexer's (a later setter overrides them). When no database is set, the
// indexer takes the builder's, else that of the builder's context, as Java
// takes its runner from recordStoreBuilder.getContext()
// (OnlineIndexOperationBaseBuilder.java:124-128).
func (b *OnlineIndexerBuilder) SetRecordStoreBuilder(builder *StoreBuilder) *OnlineIndexerBuilder {
	t := builder.copyBuilder()
	t.context = nil
	b.indexer.storeTemplate = t
	if builder.metaData != nil {
		b.indexer.metaData = builder.metaData
	}
	if builder.subspace != nil {
		b.indexer.subspace = builder.subspace
	}
	if builder.formatVersion != nil {
		v := *builder.formatVersion
		b.indexer.formatVersion = &v
	}
	if builder.maintenanceFilter != nil {
		b.indexer.maintenanceFilter = builder.maintenanceFilter
	}
	if b.indexer.db == nil {
		switch {
		case builder.database != nil:
			b.indexer.db = builder.database
		case builder.context != nil:
			b.indexer.db = builder.context.GetDatabase()
		}
	}
	return b
}

// SetRecordStore is Java's setRecordStore (OnlineIndexOperationBaseBuilder.java:
// 138-143): the store as a template, SetRecordStoreBuilder(store.AsBuilder()),
// which carries the store's format version, and, when no database is set, the
// database of the store's context, as Java takes its runner from it.
func (b *OnlineIndexerBuilder) SetRecordStore(store *FDBRecordStore) *OnlineIndexerBuilder {
	b.SetRecordStoreBuilder(store.AsBuilder())
	if b.indexer.db == nil && store.context != nil {
		b.indexer.db = store.context.GetDatabase()
	}
	return b
}

// SetMetaData sets the record metadata.
func (b *OnlineIndexerBuilder) SetMetaData(md *RecordMetaData) *OnlineIndexerBuilder {
	b.indexer.metaData = md
	return b
}

// SetIndex adds the index to build, as Java's setIndex does
// (OnlineIndexer.java:668-676): it is refused when targets are already set
// ("setIndex may not be used when other target indexes are already set", an
// IndexingValidationError that Build returns), a nil index is skipped (Build
// then reports "index must be set"), and nothing stops AddTargetIndex after it.
func (b *OnlineIndexerBuilder) SetIndex(index *Index) *OnlineIndexerBuilder {
	if len(b.indexer.targetIndexes) > 0 {
		if b.setIndexErr == nil {
			b.setIndexErr = &IndexingValidationError{Message: "setIndex may not be used when other target indexes are already set"}
		}
		return b
	}
	if index != nil {
		b.indexer.targetIndexes = append(b.indexer.targetIndexes, index)
	}
	return b
}

// AddTargetIndex adds a target index for multi-target building. Matches Java's
// OnlineIndexer.Builder.addTargetIndex(), which checks nothing.
func (b *OnlineIndexerBuilder) AddTargetIndex(index *Index) *OnlineIndexerBuilder {
	b.indexer.targetIndexes = append(b.indexer.targetIndexes, index)
	return b
}

// SetTargetIndexes replaces the target list. Matches Java's
// OnlineIndexer.Builder.setTargetIndexes().
func (b *OnlineIndexerBuilder) SetTargetIndexes(indexes []*Index) *OnlineIndexerBuilder {
	// A copy, as Java's setTargetIndexes makes one (OnlineIndexer.java:698):
	// AddTargetIndex and SetIndex append to this slice, and Build reorders it,
	// neither of which may write into the caller's array.
	b.indexer.targetIndexes = slices.Clone(indexes)
	return b
}

// SetSubspace sets the store subspace.
func (b *OnlineIndexerBuilder) SetSubspace(ss subspace.Subspace) *OnlineIndexerBuilder {
	b.indexer.subspace = ss
	return b
}

// SetInitialLimit is Java's setInitialLimit: the per-transaction record limit
// a build starts at, at most the limit; 0 starts at the limit.
func (b *OnlineIndexerBuilder) SetInitialLimit(limit int) *OnlineIndexerBuilder {
	b.indexer.initialLimit = limit
	return b
}

// SetIncreaseLimitAfter is Java's setIncreaseLimitAfter: the consecutive
// successful transactions after which a lowered limit re-increases toward the
// limit.
func (b *OnlineIndexerBuilder) SetIncreaseLimitAfter(n int) *OnlineIndexerBuilder {
	b.indexer.increaseLimitAfter = n
	return b
}

// SetMaxWriteLimitBytes is Java's setMaxWriteLimitBytes: a range commits once
// its transaction's approximate size passes this many bytes (default 900,000;
// 0 or less disables the check).
func (b *OnlineIndexerBuilder) SetMaxWriteLimitBytes(n int64) *OnlineIndexerBuilder {
	b.indexer.maxWriteLimitBytes = n
	return b
}

// SetTransactionTimeLimitMilliseconds is Java's
// setTransactionTimeLimitMilliseconds: a range commits once its transaction
// is older than this (default 4,000 ms; 0 or less disables the check).
func (b *OnlineIndexerBuilder) SetTransactionTimeLimitMilliseconds(ms int64) *OnlineIndexerBuilder {
	b.indexer.transactionTimeLimitMs = ms
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

// SetSourceIndex sets the source index for the BY_INDEX strategy. Build refuses
// it with several targets or a mutual policy. Everything else about it is
// checked when BuildIndex runs, as Java checks it: the source must be a
// scannable VALUE index on the target's one record type whose root expression
// creates no duplicates, and a source that is not is answered by the build
// catcher with a records-scan fallback (or an IndexingValidationError under
// IndexingPolicy.ForbidRecordScan).
// Matches Java's OnlineIndexer.Builder.setSourceIndex().
func (b *OnlineIndexerBuilder) SetSourceIndex(index *Index) *OnlineIndexerBuilder {
	b.indexer.sourceIndex = index
	return b
}

// SetMaxAttempts sets how many attempts each of the indexer's transactions
// makes, as Java's OnlineIndexOperationBaseBuilder.setMaxAttempts sets its
// runner's (default: the database's, 10). The lessen-work retries of
// SetMaxRetries run over each attempt-bounded transaction.
func (b *OnlineIndexerBuilder) SetMaxAttempts(maxAttempts int) *OnlineIndexerBuilder {
	b.indexer.maxAttempts = maxAttempts
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

// SetEnforcedPostTransactionDelay sets a fixed delay in milliseconds applied after each
// build transaction. When positive, it is used INSTEAD of the records-per-second throttle.
// Default 0 disables it (records-per-second behaviour). Matches Java's
// OnlineIndexer.Builder.setEnforcedPostTransactionDelay().
func (b *OnlineIndexerBuilder) SetEnforcedPostTransactionDelay(ms int) *OnlineIndexerBuilder {
	b.indexer.enforcedPostTransactionDelay = ms
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
// the key space. If nil/empty, the boundaries are the primary keys at the shard
// boundaries of the indexed records (GetPrimaryKeyBoundaries).
// Matches Java's OnlineIndexer.Builder.setMutualIndexingBoundaries().
func (b *OnlineIndexerBuilder) SetMutualIndexingBoundaries(boundaries [][]byte) *OnlineIndexerBuilder {
	b.indexer.mutual = true
	b.indexer.mutualBoundaries = boundaries
	return b
}

// defaultLeaseLengthMs is Java's OnlineIndexOperationConfig.DEFAULT_LEASE_LENGTH_MILLIS
// (OnlineIndexOperationConfig.java:61). A peer judges another session's heartbeat
// stale by ITS OWN lease (IndexingHeartbeat.java:106-107, indexing_heartbeat.go
// checkAdmission), so a default-configured Java builder treats a Go heartbeat
// older than 10s as dead; a longer Go default would only make Go slower to admit
// after a Java session died, while a Go session that renews less often than 10s
// is admitted-over by Java either way.
const defaultLeaseLengthMs int64 = 10_000

// resolvedLeaseLengthMs maps the unset (zero) lease to Java's default.
func resolvedLeaseLengthMs(ms int64) int64 {
	if ms == 0 {
		return defaultLeaseLengthMs
	}
	return ms
}

// SetLeaseLengthMs sets the heartbeat lease duration in milliseconds. If a
// heartbeat is not updated within this duration, the process is presumed dead.
// Default is Java's 10000 (defaultLeaseLengthMs). Only relevant for non-mutual
// mode; in mutual mode heartbeats are written but not checked.
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
	if b.setIndexErr != nil {
		return nil, b.setIndexErr
	}
	if b.indexer.db == nil {
		return nil, fmt.Errorf("online indexer: database is required")
	}
	if b.indexer.metaData == nil {
		return nil, fmt.Errorf("online indexer: metadata is required")
	}
	if len(b.indexer.targetIndexes) == 0 {
		// Java validateIndexSetting's first check (OnlineIndexer.java:862-864).
		return nil, &MetaDataError{Message: "index must be set"}
	}
	if b.indexer.subspace == nil {
		return nil, fmt.Errorf("online indexer: subspace is required")
	}
	if b.indexer.limit <= 0 {
		b.indexer.limit = 100
	}

	// Java OnlineIndexer.Builder.validateIndexSetting (OnlineIndexer.java:866-878),
	// in its order: a source index with more than one target, counted BEFORE
	// duplicates are removed, then the dedup, then a source index with a mutual
	// policy, each a ValidationException; the targets' presence in the metadata
	// after that, and the record types (validateTypes) last.
	if b.indexer.sourceIndex != nil {
		if len(b.indexer.targetIndexes) > 1 {
			return nil, &IndexingValidationError{Message: "Indexing multi targets by a source index is not supported (yet)", SourceIndexName: b.indexer.sourceIndex.Name}
		}
	}

	if err := b.validateAndSortTargets(); err != nil {
		return nil, err
	}

	// And every PRESET RECORD TYPE, for the same reason and with a worse failure
	// if it is skipped. Both readers of recordTypes resolve a name, and neither
	// used to ERROR on one that resolves to nothing: indexedRecordTypes silently
	// dropped it, the empty set made computeRecordsRange decline, the build fell
	// back to a full scan, the per-record predicate matched nothing, and the index
	// was then marked READABLE. A built, readable, EMPTY index that queries answer
	// from -- the same disaster the resolution fix closed for a MISSPELLED name,
	// reached instead through a name that names nothing at all.
	//
	// Java cannot reach this: IndexingCommon.fillTargetIndexers takes a
	// Collection<RecordType>, so an unresolvable name is not expressible. This is
	// a Go-only builder API, and it failed OPEN on the write path.
	for _, name := range b.indexer.recordTypes {
		if b.indexer.metaData.GetRecordType(name) == nil {
			return nil, fmt.Errorf("online indexer: record type %q not contained within specified metadata", name)
		}
	}

	isMulti := len(b.indexer.targetIndexes) > 1

	// Multi-target restrictions (matches Java's IndexingCommon).
	if (isMulti || b.indexer.mutual) && len(b.indexer.recordTypes) > 0 {
		return nil, fmt.Errorf("online indexer: preset record types not allowed with multi-target/mutual indexing")
	}
	b.indexer.leaseLengthMs = resolvedLeaseLengthMs(b.indexer.leaseLengthMs)

	// Create adaptive throttle (matches Java's IndexingThrottle initialization)
	b.indexer.throttle = newIndexingThrottle(b.indexer.limit, b.indexer.maxRetries, b.indexer.recordsPerSecond, b.indexer.enforcedPostTransactionDelay).
		withLimits(b.indexer.limit, b.indexer.initialLimit, b.indexer.increaseLimitAfter)

	return &b.indexer, nil
}

// validateAndSortTargets deduplicates, validates, sorts, and resolves target
// indexes against the metadata.
func (b *OnlineIndexerBuilder) validateAndSortTargets() error {
	// Remove duplicates as Java's HashSet does (OnlineIndexer.java:871-874):
	// by Index.equals, the FIRST of equal objects kept. Two objects that share a
	// name but differ in anything Java's equality compares (a subspace key, a
	// version, a root) are both kept, and the one that is not the metadata's
	// own is refused below; an equal copy listed after the metadata's own is
	// dropped, and one listed before it is kept and refused, as in Java.
	deduped := make([]*Index, 0, len(b.indexer.targetIndexes))
	for _, idx := range b.indexer.targetIndexes {
		dup := false
		for _, kept := range deduped {
			if kept.equalsJava(idx) {
				dup = true
				break
			}
		}
		if !dup {
			deduped = append(deduped, idx)
		}
	}
	b.indexer.targetIndexes = deduped
	if b.indexer.sourceIndex != nil && b.indexer.mutual {
		return &IndexingValidationError{Message: "Indexing mutually by a source index is not supported (yet)", SourceIndexName: b.indexer.sourceIndex.Name}
	}
	for _, idx := range b.indexer.targetIndexes {
		if idx == nil {
			// Java's ArrayList admits a null target, and its HashSet keeps one
			// of several. The build then fails with a NullPointerException at
			// the sort or the metadata check below (OnlineIndexer.java:879-880),
			// after the two ValidationExceptions above. Go reports the missing
			// index at the same point instead of panicking.
			return &MetaDataError{Message: "index must be set"}
		}
	}

	// Sort target indexes by name before the metadata check, as Java does
	// (targetIndexes.sort(Comparator.comparing(Index::getName)),
	// OnlineIndexer.java:879), so the refusal names the alphabetically first
	// target that fails, and so the primary index (the first) is deterministic.
	// The sort is stable: objects of one name keep the order they were given.
	sort.SliceStable(b.indexer.targetIndexes, func(i, j int) bool {
		return b.indexer.targetIndexes[i].Name < b.indexer.targetIndexes[j].Name
	})

	// Every target must be the metadata's OWN index object, as Java's
	// validateIndexSetting requires (`index != metaData.getIndex(name)`,
	// OnlineIndexer.java:880-883): the range set, the maintainers and the index
	// entries are keyed by the object's subspace key and versions, so an object
	// that only shares the metadata index's name would build under keys the
	// metadata does not name, while the state and publication go by name.
	md := b.indexer.metaData
	for _, idx := range b.indexer.targetIndexes {
		if md.GetIndex(idx.Name) != idx {
			return &MetaDataError{Message: "Index " + idx.Name + " not contained within specified metadata"}
		}
	}
	return nil
}
