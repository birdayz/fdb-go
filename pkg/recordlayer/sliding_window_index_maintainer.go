package recordlayer

import (
	"bytes"
	"fmt"
	"time"

	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"google.golang.org/protobuf/proto"
)

// Subspace and meta keys inside a sliding window's keyspace-10 region.
// Matches Java's SlidingWindowIndexMaintainer.java:190-196.
//
// 2 and 5 are NOT typos or room left for growth — they are simply absent from
// Java's meta region, and closing the gap would change the stored bytes.
const (
	// slidingWindowEntriesSubspaceKey scopes the per-partition entry list:
	// [windowValue..., primaryKey...] -> packed primaryKey.
	slidingWindowEntriesSubspaceKey = 0
	// slidingWindowMetaSubspaceKey scopes the per-partition metadata region.
	slidingWindowMetaSubspaceKey = 1
	// slidingWindowCountKey holds the window count as a packed long.
	slidingWindowCountKey = 3
	// slidingWindowBoundaryKey holds the packed entry key of the worst entry
	// currently inside the window.
	slidingWindowBoundaryKey = 4
)

// Instrumentation for the sliding window, matching Java's
// SlidingWindowIndexMaintainer.SlidingWindowCounter / SlidingWindowEvent /
// SlidingWindowSizeEvent enums (:722-792) name for name.
var (
	CountSWItemAddedToWindowFilling  = Event{"sw_item_added_to_window_filling", "item added to window while filling up", KindCount}
	CountSWItemAddedToEntriesOnly    = Event{"sw_item_added_to_entries_only", "item worse than boundary added to entries", KindCount}
	CountSWDeleteUntracked           = Event{"sw_delete_untracked", "delete called for untracked record", KindCount}
	CountSWOverflowEntryDeleted      = Event{"sw_overflow_entry_deleted", "overflow entry deleted from entries", KindCount}
	CountSWWindowEntryDeleted        = Event{"sw_window_entry_deleted", "in-window non-boundary entry deleted", KindCount}
	CountSWItemPromotedFromOverflow  = Event{"sw_item_promoted_from_overflow", "item promoted from overflow into window", KindCount}
	CountSWWindowShrunkNoOverflow    = Event{"sw_window_shrunk_no_overflow", "window shrunk: no overflow available for re-election", KindCount}
	CountSWPartitionEmptied          = Event{"sw_partition_emptied", "partition emptied (no entries remain)", KindCount}
	CountSWEvictedRecordMissing      = Event{"sw_evicted_record_missing", "boundary record could not be loaded for eviction", KindCount}
	CountSWPromotedRecordMissing     = Event{"sw_promoted_record_missing", "overflow record could not be loaded for promotion", KindCount}
	CountSWPreemptiveDeleteWriteOnly = Event{"sw_preemptive_delete_write_only", "preemptive delete during write-only index build", KindCount}
	CountSWPartitionCleared          = Event{"sw_partition_cleared", "partition cleared via deleteWhere", KindCount}

	EventSWEvictAndReplace           = Event{"sw_evict_and_replace", "evict boundary and insert better entry", KindTimed}
	EventSWReElectFromOverflow       = Event{"sw_re_elect_from_overflow", "re-elect overflow entry into window", KindTimed}
	EventSWBoundaryRescanAfterEvict  = Event{"sw_boundary_rescan_after_evict", "rescan to locate new boundary after eviction", KindTimed}
	EventSWBoundaryRescanAfterDelete = Event{"sw_boundary_rescan_after_delete", "rescan to locate new boundary after boundary delete", KindTimed}
	EventSWDelegateInsert            = Event{"sw_delegate_insert", "insert into the delegate index", KindTimed}
	EventSWDelegateDelete            = Event{"sw_delegate_delete", "delete from the delegate index", KindTimed}

	SizeSWWindowCount = Event{"sw_window_count", "window count after update", KindSizeDistribution}
)

// slidingWindowExtremum selects which end of the ordering the window keeps.
// Matches Java's SlidingWindowIndexMaintainer.Type (:100-186).
type slidingWindowExtremum int

const (
	// slidingWindowMin keeps the SMALLEST ordering values (Direction ASC).
	// The window is the LEFT side of the boundary; overflow is to the right.
	slidingWindowMin slidingWindowExtremum = iota
	// slidingWindowMax keeps the LARGEST ordering values (Direction DESC).
	// The window is the RIGHT side of the boundary; overflow is to the left.
	slidingWindowMax
)

// compare orders two entry keys by the extremum's notion of "better first".
// MIN uses natural tuple order, MAX uses its reverse — Java's
// Comparator.naturalOrder() / reverseOrder() over Tuple.
//
// Tuples are compared by their PACKED bytes, which is the same order Java's
// Tuple.compareTo yields for every element type the FDB tuple layer encodes:
// the encoding is order-preserving by construction, and it is also the order
// the entry list is physically stored in, so the comparator and the range scans
// below cannot disagree. The repo settles this the same way elsewhere
// (permuted_min_max_index_maintainer.go, time_window_leaderboard_maintainer.go).
func (t slidingWindowExtremum) compare(a, b tuple.Tuple) int {
	c := bytes.Compare(a.Pack(), b.Pack())
	if t == slidingWindowMax {
		return -c
	}
	return c
}

// isBetter reports whether candidate is STRICTLY better than worst.
// Matches Java's Type.isBetter (:117-122).
func (t slidingWindowExtremum) isBetter(candidate, worst tuple.Tuple) bool {
	return t.compare(candidate, worst) < 0
}

// isInWindow reports whether entryKey is on the good side of the boundary,
// inclusive of the boundary itself.
// Matches Java's Type.isInWindow (:124-131).
func (t slidingWindowExtremum) isInWindow(entryKey, boundaryKey tuple.Tuple) bool {
	return t.compare(entryKey, boundaryKey) <= 0
}

// isWorseOrEqual reports whether candidate is not better than boundary — used
// while the window is still filling to decide whether the new entry becomes the
// boundary. Matches Java's Type.isWorseOrEqual (:133-140).
func (t slidingWindowExtremum) isWorseOrEqual(candidate, boundary tuple.Tuple) bool {
	return !t.isBetter(candidate, boundary)
}

// slidingWindowStore is the store surface the sliding window needs beyond the
// shared indexStoreContext: it has to load the records it evicts and promotes,
// because the delegate index is updated with RECORDS, not with entry keys.
type slidingWindowStore interface {
	indexStoreContext
	loadRecordForIndexMaintenance(primaryKey tuple.Tuple) (*FDBStoredRecord[proto.Message], error)
}

// slidingWindowIndexMaintainer keeps only the top-N records per partition in
// the wrapped index, by a window key.
// Matches Java's SlidingWindowIndexMaintainer.
//
// The design's central property, and the reason it is worth porting rather than
// reinventing: ALL entries for a partition — in-window and overflow alike —
// live in ONE sorted subspace, and a single boundary pointer marks the worst
// entry currently inside the window. Eviction and re-election MOVE THE POINTER;
// they never move data. An implementation that instead maintained two ranges
// would rewrite a range on every insert.
type slidingWindowIndexMaintainer struct {
	index    *Index
	delegate IndexMaintainer
	// swSubspace is <storeSubspace>/10/<index subspace tuple key>.
	swSubspace subspace.Subspace
	tx         fdb.WritableTransaction
	store      slidingWindowStore
	timer      *StoreTimer

	extremumType        slidingWindowExtremum
	windowSize          int
	windowKey           KeyExpression
	windowKeyColumnSize int
	// partitionKey is nil when the window is unpartitioned; the partition tuple
	// is then the empty tuple, which is a real code path and not a degenerate
	// one — it scopes every entry under the sliding-window subspace directly.
	partitionKey           KeyExpression
	partitionKeyColumnSize int
}

// delegateMaintainer exposes the wrapped maintainer so that store-level entry
// points which need a CONCRETE maintainer type can see through the decoration.
// See unwrapVectorMaintainer.
func (m *slidingWindowIndexMaintainer) delegateMaintainer() IndexMaintainer { return m.delegate }

// indexMaintainerDecorator is implemented by maintainers that wrap another
// maintainer and forward the read path to it.
//
// It exists because Go's store-level vector entry points reach their maintainer
// by CONCRETE TYPE ASSERTION (`maintainer.(*vectorIndexMaintainer)`), which a
// decorator silently breaks: a windowed vector index would answer every search
// with "index X is not a VECTOR index". Java has no equivalent problem — there
// the decorator implements the same abstract IndexMaintainer and forwards
// scan(), so nothing downstream ever asks what the concrete class is.
type indexMaintainerDecorator interface {
	delegateMaintainer() IndexMaintainer
}

// maintainerAs peels decorators off a maintainer until one of them satisfies T.
//
// Every access method beyond the four on IndexMaintainer is reached by asking
// whether the maintainer satisfies some capability — a concrete type for the
// vector search entry points, a byDistanceScanner or orderedStreamScanner for
// the generic index scan. A decorator satisfies none of them, so wrapping an
// index silently REMOVES capabilities its delegate has, and the caller reports
// the index as one that never had them ("does not support BY_DISTANCE scan").
//
// Asking through this helper makes the decorator's capability set exactly its
// delegate's, automatically. The alternative — forwarding each capability from
// the decorator by hand — breaks silently again the next time a capability
// interface is added, and the breakage looks like a missing feature rather than
// like a wrapper.
//
// Java has no equivalent problem: its decorator extends the same abstract
// IndexMaintainer and overrides scan(), so nothing downstream ever asks what
// the concrete class is.
func maintainerAs[T any](m IndexMaintainer) (T, bool) {
	for {
		if t, ok := any(m).(T); ok {
			return t, true
		}
		d, ok := m.(indexMaintainerDecorator)
		if !ok {
			var zero T
			return zero, false
		}
		m = d.delegateMaintainer()
	}
}

// unwrapVectorMaintainer peels any decorators off a maintainer and returns the
// vector maintainer underneath, if there is one.
func unwrapVectorMaintainer(m IndexMaintainer) (*vectorIndexMaintainer, bool) {
	return maintainerAs[*vectorIndexMaintainer](m)
}

func newSlidingWindowIndexMaintainer(
	index *Index,
	delegate IndexMaintainer,
	swSubspace subspace.Subspace,
	tx fdb.WritableTransaction,
	store slidingWindowStore,
	timer *StoreTimer,
) (*slidingWindowIndexMaintainer, error) {
	spec, err := index.RowNumberWindowSpec()
	if err != nil {
		return nil, err
	}
	windowKey, err := spec.OrderingKey()
	if err != nil {
		return nil, fmt.Errorf("sliding window index %q: ordering key: %w", index.Name, err)
	}
	partitionKey, err := spec.PartitionKey()
	if err != nil {
		return nil, fmt.Errorf("sliding window index %q: partition key: %w", index.Name, err)
	}
	partitionColumns := 0
	if partitionKey != nil {
		partitionColumns = partitionKey.ColumnSize()
	}
	extremum := slidingWindowMax
	if spec.Direction == RowNumberWindowAscending {
		extremum = slidingWindowMin
	}
	return &slidingWindowIndexMaintainer{
		index:                  index,
		delegate:               delegate,
		swSubspace:             swSubspace,
		tx:                     tx,
		store:                  store,
		timer:                  timer,
		extremumType:           extremum,
		windowSize:             spec.Size,
		windowKey:              windowKey,
		windowKeyColumnSize:    windowKey.ColumnSize(),
		partitionKey:           partitionKey,
		partitionKeyColumnSize: partitionColumns,
	}, nil
}

// Scan delegates to the wrapped maintainer. The window changes WHICH records
// the wrapped index holds, never how it is read.
// Matches Java's SlidingWindowIndexMaintainer.scan (:236-256).
func (m *slidingWindowIndexMaintainer) Scan(
	scanRange TupleRange,
	continuation []byte,
	scanProperties ScanProperties,
) RecordCursor[*IndexEntry] {
	return m.delegate.Scan(scanRange, continuation, scanProperties)
}

// Update applies window semantics to a mutation and then lets the wrapped
// maintainer see only the records that are inside the window.
// Matches Java's SlidingWindowIndexMaintainer.update (:365-393).
//
// The whole mutation is serialized on the sliding-window subspace, exactly as
// Java takes doWithWriteLock(LockIdentifier(swSubspace)): the count, the
// boundary pointer and the entry list are read-modify-written together, so two
// concurrent updates inside one transaction would otherwise interleave into an
// inconsistent boundary.
func (m *slidingWindowIndexMaintainer) Update(oldRecord, newRecord *FDBStoredRecord[proto.Message]) error {
	lockKey := string(m.swSubspace.Bytes())
	m.store.AcquireWriteLock(lockKey)
	defer m.store.ReleaseWriteLock(lockKey)
	return m.updateLocked(oldRecord, newRecord)
}

func (m *slidingWindowIndexMaintainer) updateLocked(oldRecord, newRecord *FDBStoredRecord[proto.Message]) error {
	if oldRecord != nil && m.shouldMaintain(oldRecord) {
		if err := m.handleDelete(oldRecord); err != nil {
			return err
		}
	}
	if newRecord != nil && m.shouldMaintain(newRecord) {
		if err := m.handleInsert(newRecord); err != nil {
			return err
		}
	}
	return nil
}

// shouldMaintain is the per-record gate — Java's
// IndexMaintenanceUtils.getFilterTypeForRecord == ALL branch (:377-390).
//
// Go has no IndexMaintenanceFilter framework; the equivalent per-record gate is
// the compiled Index.Predicate, which for a row-number window index accepts
// every record by construction (see rowNumberWindowPredicateFromProto). It is
// still consulted rather than assumed, because the window arm may sit inside a
// conjunction with genuinely filtering arms — `AND(price > 10, rowWindow)` is a
// legal shape, and then only the qualifying records may enter the window.
func (m *slidingWindowIndexMaintainer) shouldMaintain(record *FDBStoredRecord[proto.Message]) bool {
	return m.index.Predicate == nil || m.index.Predicate(record.Record)
}

// UpdateWhileWriteOnly maintains the window during an online index build.
// Matches Java's SlidingWindowIndexMaintainer.updateWhileWriteOnly (:395-424).
//
// A sliding window is NOT idempotent: it keeps a count, so applying the same
// insert twice inflates the count and then evicts a record that should have
// stayed. During a build the indexer may already have processed newRecord in an
// earlier range scan, so update(null, newRecord) alone would double-count.
//
// Java's answer is a preemptive delete of newRecord before the ordinary update,
// and it is deliberately simpler than the standard maintainer's range-set check:
//   - if newRecord was not previously indexed the delete is a no-op, because
//     the entry is simply absent from the entry list;
//   - if it was, the delete removes it and decrements the count, so the
//     following insert cannot double-count.
//
// Both calls run under ONE lock acquisition. Java's two `update` calls each take
// the write lock, but its lock is re-entrant per context; Go's is not, so
// nesting would deadlock. Holding it across both is also the stronger
// guarantee — the preemptive delete and the re-insert are one atomic window
// mutation, which is what the comment above claims they are.
func (m *slidingWindowIndexMaintainer) UpdateWhileWriteOnly(oldRecord, newRecord *FDBStoredRecord[proto.Message]) error {
	lockKey := string(m.swSubspace.Bytes())
	m.store.AcquireWriteLock(lockKey)
	defer m.store.ReleaseWriteLock(lockKey)

	if newRecord != nil {
		m.timer.Increment(CountSWPreemptiveDeleteWriteOnly)
		if err := m.updateLocked(newRecord, nil); err != nil {
			return err
		}
	}
	return m.updateLocked(oldRecord, newRecord)
}

// partitionSubspaces resolves the three subspaces a record's partition uses.
// Composition is scope-by-partition-then-region, matching Java (:441-444):
//
//	swSubspace.subspace(partitionTuple).subspace(ENTRIES | META)
func (m *slidingWindowIndexMaintainer) partitionSubspaces(partitionTuple tuple.Tuple) (entriesSub, metaSub subspace.Subspace) {
	partitionSub := m.swSubspace
	if len(partitionTuple) > 0 {
		args := make([]tuple.TupleElement, len(partitionTuple))
		for i, v := range partitionTuple {
			args[i] = v
		}
		partitionSub = m.swSubspace.Sub(args...)
	}
	return partitionSub.Sub(slidingWindowEntriesSubspaceKey),
		partitionSub.Sub(slidingWindowMetaSubspaceKey)
}

// evaluatePartition evaluates the partition key against a record, returning the
// EMPTY tuple when the window is unpartitioned.
// Matches Java's evaluatePartition (:426-433).
func (m *slidingWindowIndexMaintainer) evaluatePartition(record *FDBStoredRecord[proto.Message]) (tuple.Tuple, error) {
	if m.partitionKey == nil {
		return tuple.Tuple{}, nil
	}
	return evaluateSingletonKey(m.partitionKey, record, "partition key", m.index.Name)
}

// evaluateWindowValue evaluates the window (ordering) key against a record.
func (m *slidingWindowIndexMaintainer) evaluateWindowValue(record *FDBStoredRecord[proto.Message]) (tuple.Tuple, error) {
	return evaluateSingletonKey(m.windowKey, record, "window key", m.index.Name)
}

// evaluateSingletonKey is Java's KeyExpression.evaluateSingleton: exactly one
// key, or an error. A fan-out here would produce several window positions for
// one record, which the count and the boundary pointer have no way to represent.
func evaluateSingletonKey(
	expr KeyExpression,
	record *FDBStoredRecord[proto.Message],
	what string,
	indexName string,
) (tuple.Tuple, error) {
	tuples, err := expr.Evaluate(record, record.Record)
	if err != nil {
		return nil, fmt.Errorf("sliding window index %q: evaluate %s: %w", indexName, what, err)
	}
	if len(tuples) != 1 {
		return nil, fmt.Errorf(
			"sliding window index %q: %s produced %d keys, expected exactly one",
			indexName, what, len(tuples))
	}
	out := make(tuple.Tuple, len(tuples[0]))
	for i, v := range tuples[0] {
		out[i] = v
	}
	return out, nil
}

// entryKeyFor builds the sorted entry key for a record: the window value
// followed by the FULL primary key.
// Matches Java's `windowValue.addAll(primaryKey)` (:448).
//
// The primary key is NOT trimmed here even though the delegate trims it for the
// HNSW graph. The suffix is what makes two records with equal window values
// distinct entries in one sorted list; trimming it could collapse them onto one
// key and lose an entry.
func (m *slidingWindowIndexMaintainer) entryKeyFor(record *FDBStoredRecord[proto.Message]) (tuple.Tuple, error) {
	windowValue, err := m.evaluateWindowValue(record)
	if err != nil {
		return nil, err
	}
	entryKey := make(tuple.Tuple, 0, len(windowValue)+len(record.PrimaryKey))
	entryKey = append(entryKey, windowValue...)
	entryKey = append(entryKey, record.PrimaryKey...)
	return entryKey, nil
}

// handleInsert is Java's handleInsert (:435-499).
func (m *slidingWindowIndexMaintainer) handleInsert(record *FDBStoredRecord[proto.Message]) error {
	partitionTuple, err := m.evaluatePartition(record)
	if err != nil {
		return err
	}
	entriesSub, metaSub := m.partitionSubspaces(partitionTuple)

	entryKey, err := m.entryKeyFor(record)
	if err != nil {
		return err
	}

	// The entry is written unconditionally: the entry list tracks every record
	// in the partition, in window and overflow alike. Only the delegate index
	// is restricted to the window.
	m.tx.Set(entriesSub.Pack(entryKey), record.PrimaryKey.Pack())

	counterKey := metaSub.Pack(tuple.Tuple{slidingWindowCountKey})
	boundaryMetaKey := metaSub.Pack(tuple.Tuple{slidingWindowBoundaryKey})

	counterBytes, err := m.tx.Get(counterKey).Get()
	if err != nil {
		return fmt.Errorf("sliding window index %q: read count: %w", m.index.Name, err)
	}
	count, err := decodeSlidingWindowLong(counterBytes)
	if err != nil {
		return fmt.Errorf("sliding window index %q: decode count: %w", m.index.Name, err)
	}

	if count < int64(m.windowSize) {
		m.timer.Increment(CountSWItemAddedToWindowFilling)
		if err := m.delegateInsert(record); err != nil {
			return err
		}
		boundaryBytes, err := m.tx.Get(boundaryMetaKey).Get()
		if err != nil {
			return fmt.Errorf("sliding window index %q: read boundary: %w", m.index.Name, err)
		}
		m.tx.Set(counterKey, encodeSlidingWindowLong(count+1))
		m.timer.RecordSize(SizeSWWindowCount, count+1)
		if boundaryBytes == nil {
			m.tx.Set(boundaryMetaKey, entryKey.Pack())
			return nil
		}
		boundaryEntryKey, err := tuple.Unpack(boundaryBytes)
		if err != nil {
			return fmt.Errorf("sliding window index %q: unpack boundary: %w", m.index.Name, err)
		}
		if m.extremumType.isWorseOrEqual(entryKey, boundaryEntryKey) {
			m.tx.Set(boundaryMetaKey, entryKey.Pack())
		}
		return nil
	}

	// Window full: the new entry only enters if it beats the current boundary.
	boundaryBytes, err := m.tx.Get(boundaryMetaKey).Get()
	if err != nil {
		return fmt.Errorf("sliding window index %q: read boundary: %w", m.index.Name, err)
	}
	if boundaryBytes == nil {
		return &SlidingWindowCorruptionError{
			IndexName: m.index.Name,
			Message:   "sliding window boundary is missing but count >= windowSize, possible corruption",
		}
	}
	boundaryEntryKey, err := tuple.Unpack(boundaryBytes)
	if err != nil {
		return fmt.Errorf("sliding window index %q: unpack boundary: %w", m.index.Name, err)
	}
	if !m.extremumType.isBetter(entryKey, boundaryEntryKey) {
		m.timer.Increment(CountSWItemAddedToEntriesOnly)
		// Already written to the entry list, on the overflow side. Nothing else.
		return nil
	}
	return m.instrument(EventSWEvictAndReplace, func() error {
		return m.evictBoundaryAndReplace(record, entryKey, entriesSub, boundaryEntryKey, boundaryMetaKey)
	})
}

// handleDelete is Java's handleDelete (:501-561).
func (m *slidingWindowIndexMaintainer) handleDelete(record *FDBStoredRecord[proto.Message]) error {
	partitionTuple, err := m.evaluatePartition(record)
	if err != nil {
		return err
	}
	entriesSub, metaSub := m.partitionSubspaces(partitionTuple)

	entryKey, err := m.entryKeyFor(record)
	if err != nil {
		return err
	}
	packedEntryKey := entriesSub.Pack(entryKey)

	entryValue, err := m.tx.Get(packedEntryKey).Get()
	if err != nil {
		return fmt.Errorf("sliding window index %q: read entry: %w", m.index.Name, err)
	}
	if entryValue == nil {
		m.timer.Increment(CountSWDeleteUntracked)
		return nil
	}

	m.tx.Clear(packedEntryKey)

	counterKey := metaSub.Pack(tuple.Tuple{slidingWindowCountKey})
	boundaryMetaKey := metaSub.Pack(tuple.Tuple{slidingWindowBoundaryKey})

	boundaryBytes, err := m.tx.Get(boundaryMetaKey).Get()
	if err != nil {
		return fmt.Errorf("sliding window index %q: read boundary: %w", m.index.Name, err)
	}
	if boundaryBytes == nil {
		return &SlidingWindowCorruptionError{
			IndexName: m.index.Name,
			Message:   "sliding window boundary is missing but entry exists, possible corruption",
		}
	}
	boundaryEntryKey, err := tuple.Unpack(boundaryBytes)
	if err != nil {
		return fmt.Errorf("sliding window index %q: unpack boundary: %w", m.index.Name, err)
	}

	if !m.extremumType.isInWindow(entryKey, boundaryEntryKey) {
		m.timer.Increment(CountSWOverflowEntryDeleted)
		// Overflow entry: it was never in the delegate, and the count and
		// boundary describe the window only, so neither moves.
		return nil
	}

	counterBytes, err := m.tx.Get(counterKey).Get()
	if err != nil {
		return fmt.Errorf("sliding window index %q: read count: %w", m.index.Name, err)
	}
	count, err := decodeSlidingWindowLong(counterBytes)
	if err != nil {
		return fmt.Errorf("sliding window index %q: decode count: %w", m.index.Name, err)
	}
	newCount := count - 1
	if newCount < 0 {
		newCount = 0
	}
	m.tx.Set(counterKey, encodeSlidingWindowLong(newCount))
	m.timer.RecordSize(SizeSWWindowCount, newCount)

	if err := m.delegateDelete(record); err != nil {
		return err
	}

	currentBoundaryPacked, err := m.updateBoundaryAfterDelete(
		entriesSub, entryKey, boundaryEntryKey, boundaryMetaKey, packedEntryKey)
	if err != nil {
		return err
	}
	return m.instrument(EventSWReElectFromOverflow, func() error {
		return m.reElectFromOverflow(entriesSub, currentBoundaryPacked, boundaryMetaKey, counterKey, newCount)
	})
}

// evictBoundaryAndReplace removes the boundary record from the delegate, adds
// the new record, and moves the pointer one entry inward.
// Matches Java's evictBoundaryAndReplace (:563-602).
func (m *slidingWindowIndexMaintainer) evictBoundaryAndReplace(
	newRecord *FDBStoredRecord[proto.Message],
	newEntryKey tuple.Tuple,
	entriesSub subspace.Subspace,
	boundaryEntryKey tuple.Tuple,
	boundaryMetaKey fdb.Key,
) error {
	if len(boundaryEntryKey) < m.windowKeyColumnSize {
		return &SlidingWindowCorruptionError{
			IndexName: m.index.Name,
			Message: fmt.Sprintf("boundary entry key has %d columns, fewer than the %d-column window key",
				len(boundaryEntryKey), m.windowKeyColumnSize),
		}
	}
	boundaryPrimaryKey := tuple.Tuple(boundaryEntryKey[m.windowKeyColumnSize:])
	oldBoundaryPackedKey := entriesSub.Pack(boundaryEntryKey)

	evicted, err := m.store.loadRecordForIndexMaintenance(boundaryPrimaryKey)
	if err != nil {
		return fmt.Errorf("sliding window index %q: load boundary record %v: %w",
			m.index.Name, boundaryPrimaryKey, err)
	}
	if evicted == nil {
		// The entry list still names a record the store no longer holds. Java
		// counts it and carries on rather than failing the write: the entry is
		// about to stop being the boundary anyway, and refusing here would
		// wedge every subsequent insert into the partition.
		m.timer.Increment(CountSWEvictedRecordMissing)
	} else if err := m.delegateDelete(evicted); err != nil {
		return err
	}

	if err := m.delegateInsert(newRecord); err != nil {
		return err
	}

	var newBoundaryKV *fdb.KeyValue
	if err := m.instrument(EventSWBoundaryRescanAfterEvict, func() error {
		var scanErr error
		newBoundaryKV, scanErr = m.newBoundaryAfterEviction(entriesSub, oldBoundaryPackedKey)
		return scanErr
	}); err != nil {
		return err
	}

	if newBoundaryKV == nil {
		// No entry inward of the evicted boundary — the new entry is now the
		// only one in the window, so it becomes the boundary.
		m.tx.Set(boundaryMetaKey, newEntryKey.Pack())
		return nil
	}
	newBoundaryKey, err := entriesSub.Unpack(newBoundaryKV.Key)
	if err != nil {
		return fmt.Errorf("sliding window index %q: unpack new boundary: %w", m.index.Name, err)
	}
	m.tx.Set(boundaryMetaKey, newBoundaryKey.Pack())
	return nil
}

// updateBoundaryAfterDelete returns the packed key of the boundary after an
// in-window delete, or nil when no entries remain.
// Matches Java's updateBoundaryAfterDelete (:604-637).
func (m *slidingWindowIndexMaintainer) updateBoundaryAfterDelete(
	entriesSub subspace.Subspace,
	entryKey, boundaryEntryKey tuple.Tuple,
	boundaryMetaKey fdb.Key,
	packedEntryKey fdb.Key,
) ([]byte, error) {
	if !tuplesEqual(entryKey, boundaryEntryKey) {
		m.timer.Increment(CountSWWindowEntryDeleted)
		return entriesSub.Pack(boundaryEntryKey), nil
	}

	var kv *fdb.KeyValue
	if err := m.instrument(EventSWBoundaryRescanAfterDelete, func() error {
		var scanErr error
		kv, scanErr = m.newBoundaryAfterEviction(entriesSub, packedEntryKey)
		return scanErr
	}); err != nil {
		return nil, err
	}
	if kv == nil {
		// No entry INWARD of the deleted boundary. That is not the same as "the
		// partition is empty", and Java conflates the two — a DELIBERATE
		// DIVERGENCE, argued below.
		//
		// The inward scan only looks at the window side. When the deleted
		// boundary was the extreme-most entry in the whole partition — which is
		// exactly the steady state of a size-1 window — there is nothing inward
		// while overflow may be sitting just past it. Java clears the boundary,
		// counts SW_PARTITION_EMPTIED and returns null, and reElectFromOverflow
		// then exits on that null, leaving count 0 and an empty graph with
		// overflow entries stranded: never promoted, never searchable.
		//
		// Java's own code calls the resulting state corrupt. Deleting one of
		// those stranded entries later reads a nil boundary with an entry
		// present and throws "sliding window boundary is missing but entry
		// exists, possible corruption" — so this is a defect, not a design, and
		// the counter's own name ("partition emptied (no entries remain)")
		// states the intent it fails to implement.
		//
		// Go promotes from overflow instead. The divergence is in CONTENTS, not
		// in format: the bytes written are the same kinds in the same layout,
		// and the state Go produces — a real boundary, a matching count, the
		// record in the graph — is one Java reads and continues from correctly,
		// whereas the state Java produces is the one Java itself later refuses.
		// Reported upstream; revisit if Java fixes it differently.
		best, berr := m.bestInOverflow(entriesSub, packedEntryKey)
		if berr != nil {
			return nil, berr
		}
		if best == nil {
			// Genuinely empty — no entry on either side. This is the case Java's
			// counter names, and here it is true.
			m.timer.Increment(CountSWPartitionEmptied)
			m.tx.Clear(boundaryMetaKey)
			return nil, nil
		}
		// Overflow exists, so hand back the DELETED entry's key as the scan
		// PIVOT rather than promoting here. reElectFromOverflow scans strictly
		// past the pivot, and the deleted key is no longer in the entry list, so
		// that scan lands on this same overflow entry and performs the single
		// promotion — setting the boundary, the count and the delegate insert in
		// one place. Promoting here as well would put two records into a window
		// that lost one.
		//
		// The overflow read happens twice (once to decide, once in the
		// promotion). Both are the same range read in the same transaction, so
		// the second is served from the read cache; the alternative is threading
		// a pre-read result through a signature whose other callers do not have
		// one.
		return packedEntryKey, nil
	}
	newBoundaryKey, err := entriesSub.Unpack(kv.Key)
	if err != nil {
		return nil, fmt.Errorf("sliding window index %q: unpack new boundary: %w", m.index.Name, err)
	}
	m.tx.Set(boundaryMetaKey, newBoundaryKey.Pack())
	return kv.Key, nil
}

// reElectFromOverflow promotes the best overflow entry into the window.
// Matches Java's reElectFromOverflow (:639-684).
func (m *slidingWindowIndexMaintainer) reElectFromOverflow(
	entriesSub subspace.Subspace,
	currentBoundaryPacked []byte,
	boundaryMetaKey fdb.Key,
	counterKey fdb.Key,
	newCount int64,
) error {
	if currentBoundaryPacked == nil {
		return nil
	}
	best, err := m.bestInOverflow(entriesSub, currentBoundaryPacked)
	if err != nil {
		return err
	}
	if best == nil {
		m.timer.Increment(CountSWWindowShrunkNoOverflow)
		return nil
	}
	m.timer.Increment(CountSWItemPromotedFromOverflow)
	bestEntryKey, err := entriesSub.Unpack(best.Key)
	if err != nil {
		return fmt.Errorf("sliding window index %q: unpack overflow entry: %w", m.index.Name, err)
	}
	bestPrimaryKey, err := tuple.Unpack(best.Value)
	if err != nil {
		return fmt.Errorf("sliding window index %q: unpack overflow primary key: %w", m.index.Name, err)
	}
	// The boundary and the count move BEFORE the record is loaded, and they are
	// not rolled back if the load comes up empty. That ordering is Java's
	// (:665-668 sets both, then :669 loads), and it is kept rather than
	// "improved": an entry naming a record the store no longer holds means the
	// entry list and the records disagree, and the count is then wrong either
	// way — one too high if it stays, one too low relative to a boundary that
	// now points at an entry outside the window if it does not. Java counts the
	// anomaly (SW_PROMOTED_RECORD_MISSING) and keeps the window's bookkeeping
	// internally consistent; diverging here would make the two engines' stored
	// bytes differ after the same sequence of operations.
	m.tx.Set(boundaryMetaKey, bestEntryKey.Pack())
	m.tx.Set(counterKey, encodeSlidingWindowLong(newCount+1))
	m.timer.RecordSize(SizeSWWindowCount, newCount+1)

	promoted, err := m.store.loadRecordForIndexMaintenance(bestPrimaryKey)
	if err != nil {
		return fmt.Errorf("sliding window index %q: load promoted record %v: %w",
			m.index.Name, bestPrimaryKey, err)
	}
	if promoted == nil {
		m.timer.Increment(CountSWPromotedRecordMissing)
		return nil
	}
	return m.delegateInsert(promoted)
}

// bestInOverflow returns the first entry on the OVERFLOW side of the boundary.
// Matches Java's Type.getBestInOverflow (:142-160).
//
// MIN: overflow is after the boundary, so scan forward from just past it.
// MAX: overflow is before the boundary, so scan backward from just before it.
func (m *slidingWindowIndexMaintainer) bestInOverflow(entriesSub subspace.Subspace, boundaryPackedKey []byte) (*fdb.KeyValue, error) {
	begin, end := entriesSub.FDBRangeKeys()
	if m.extremumType == slidingWindowMin {
		return m.firstInRange(fdb.Key(slidingWindowKeyAfter(boundaryPackedKey)), end.FDBKey(), false)
	}
	return m.firstInRange(begin.FDBKey(), fdb.Key(boundaryPackedKey), true)
}

// newBoundaryAfterEviction returns the entry just INWARD of the given boundary
// key — the one that becomes the boundary once that key leaves the window.
// Matches Java's Type.getNewBoundaryAfterEviction (:162-186).
func (m *slidingWindowIndexMaintainer) newBoundaryAfterEviction(entriesSub subspace.Subspace, oldBoundaryPackedKey []byte) (*fdb.KeyValue, error) {
	begin, end := entriesSub.FDBRangeKeys()
	if m.extremumType == slidingWindowMin {
		// Window is on the left; the new boundary is the entry just before.
		return m.firstInRange(begin.FDBKey(), fdb.Key(oldBoundaryPackedKey), true)
	}
	// Window is on the right; the new boundary is the entry just after.
	return m.firstInRange(fdb.Key(slidingWindowKeyAfter(oldBoundaryPackedKey)), end.FDBKey(), false)
}

// firstInRange reads a single key-value from [begin, end), from whichever end
// `reverse` selects. An empty range yields (nil, nil).
func (m *slidingWindowIndexMaintainer) firstInRange(begin, end fdb.Key, reverse bool) (*fdb.KeyValue, error) {
	if bytes.Compare(begin, end) >= 0 {
		// FDB treats begin >= end as an empty range, but constructing the read
		// at all is pointless and an inverted range is worth not sending.
		return nil, nil
	}
	kvs, err := m.tx.GetRange(
		fdb.KeyRange{Begin: begin, End: end},
		fdb.RangeOptions{Limit: 1, Reverse: reverse, Mode: fdb.StreamingModeExact},
	).GetSliceWithError()
	if err != nil {
		return nil, fmt.Errorf("sliding window index %q: scan entries: %w", m.index.Name, err)
	}
	if len(kvs) == 0 {
		return nil, nil
	}
	kv := kvs[0]
	return &kv, nil
}

func (m *slidingWindowIndexMaintainer) delegateInsert(record *FDBStoredRecord[proto.Message]) error {
	return m.instrument(EventSWDelegateInsert, func() error {
		return m.delegate.Update(nil, record)
	})
}

func (m *slidingWindowIndexMaintainer) delegateDelete(record *FDBStoredRecord[proto.Message]) error {
	return m.instrument(EventSWDelegateDelete, func() error {
		return m.delegate.Update(record, nil)
	})
}

// instrument times fn under event, the way Java's timer.instrument(event, future)
// does. A nil timer is a no-op (StoreTimer's methods are nil-safe), but the
// clock read is skipped entirely rather than being taken and thrown away.
func (m *slidingWindowIndexMaintainer) instrument(event Event, fn func() error) error {
	if m.timer == nil {
		return fn()
	}
	start := time.Now()
	err := fn()
	m.timer.RecordSince(event, start)
	return err
}

// DeleteWhere clears a partition group from the sliding-window subspace and
// delegates. Supported ONLY when the window is partitioned AND the prefix names
// partition columns — both decided by checkSlidingWindowDeleteWhere, which runs
// as a preflight in DeleteRecordsWhere.
//
// Matches Java's SlidingWindowIndexMaintainer.deleteWhere (:686-698), including
// where the two checks live. Java's capability question is canDeleteWhere
// (:317-326), asked by deleteRecordsWhereCheckIndexes before any range is
// cleared; what remains INSIDE deleteWhere is a single Verify.verify on the
// prefix width (:689-691) — an assertion, not the gate.
//
// The arity check below is that Verify: a backstop for a caller invoking the
// maintainer directly, not the thing that makes deleteRecordsWhere safe. It
// cannot be, and that distinction is the whole reason the preflight exists: by
// the time this method runs, DeleteRecordsWhere has already queued the record,
// version and count clears, so refusing here loses the records for any caller
// that commits anyway.
//
// The unpartitioned case is left to the preflight for exactly that reason —
// duplicating it here would put a second, useless refusal at the point where
// refusing no longer helps.
func (m *slidingWindowIndexMaintainer) DeleteWhere(prefix tuple.Tuple) error {
	if len(prefix) > m.partitionKeyColumnSize {
		return &SlidingWindowDeleteWhereError{
			IndexName: m.index.Name,
			Message: fmt.Sprintf("deleteRecordsWhere prefix size %d exceeds partition key column size %d",
				len(prefix), m.partitionKeyColumnSize),
		}
	}
	m.timer.Increment(CountSWPartitionCleared)
	key := m.swSubspace.Pack(prefix)
	pr, err := fdb.PrefixRange(key)
	if err != nil {
		return fmt.Errorf("sliding window index %q: PrefixRange(%x): %w", m.index.Name, key, err)
	}
	m.tx.ClearRange(pr)
	return m.delegate.DeleteWhere(prefix)
}

// slidingWindowKeyAfter is FDB's ByteArrayUtil.keyAfter: the immediate
// successor of a key, formed by appending a zero byte.
func slidingWindowKeyAfter(key []byte) []byte {
	out := make([]byte, len(key)+1)
	copy(out, key)
	return out
}

// encodeSlidingWindowLong / decodeSlidingWindowLong store the window count the
// way Java does — as a packed one-element tuple, NOT as a raw little-endian
// int64. This is wire: Java reads it with Tuple.fromBytes(bytes).getLong(0).
func encodeSlidingWindowLong(value int64) []byte {
	return tuple.Tuple{value}.Pack()
}

func decodeSlidingWindowLong(b []byte) (int64, error) {
	if b == nil {
		return 0, nil
	}
	t, err := tuple.Unpack(b)
	if err != nil {
		return 0, err
	}
	if len(t) == 0 {
		return 0, fmt.Errorf("empty tuple where a count was expected")
	}
	v, ok := asInt64(t[0])
	if !ok {
		return 0, fmt.Errorf("count element %v (%T) is not an integer", t[0], t[0])
	}
	return v, nil
}

// SlidingWindowCorruptionError reports sliding-window bookkeeping that cannot
// be true of any consistent window — a missing boundary pointer where the count
// says there must be one, or a boundary entry key shorter than the window key.
//
// Matches Java's RecordCoreException with INDEX_NAME log info; it is a distinct
// Go type so callers can tell corrupt bookkeeping apart from an ordinary
// read failure.
type SlidingWindowCorruptionError struct {
	IndexName string
	Message   string
}

func (e *SlidingWindowCorruptionError) Error() string {
	return fmt.Sprintf("sliding window index %q: %s", e.IndexName, e.Message)
}

// SlidingWindowDeleteWhereError reports a deleteRecordsWhere the sliding window
// cannot serve. Matches Java's Query.InvalidExpressionException
// ("deleteRecordsWhere not supported by index X") raised when canDeleteWhere
// returns false.
type SlidingWindowDeleteWhereError struct {
	IndexName string
	Message   string
}

func (e *SlidingWindowDeleteWhereError) Error() string {
	return fmt.Sprintf("deleteRecordsWhere not supported by index %q: %s", e.IndexName, e.Message)
}
