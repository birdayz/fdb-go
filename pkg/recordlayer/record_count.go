package recordlayer

import (
	"encoding/binary"
	"fmt"

	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"google.golang.org/protobuf/proto"

	"fdb.dev/gen"
)

// RecordCountKeySizeMismatchError is Java's
// recordCoreException("key and value are not the same size") thrown by
// getSnapshotRecordCount (FDBRecordStore.java:2295-2297) when the evaluated value
// has a different number of columns than the key expression it is supposed to be a
// value of.
//
// It is a typed error rather than a plain message because the two sizes are the
// whole diagnosis: the caller either passed a value for a different count key or
// truncated one, and the alternative to failing is answering from a slot nothing
// ever writes — a confident, wrong 0.
type RecordCountKeySizeMismatchError struct {
	KeyColumnSize int
	ValueSize     int
}

func (e *RecordCountKeySizeMismatchError) Error() string {
	return fmt.Sprintf("key and value are not the same size: keyColumnSize=%d, valueSize=%d",
		e.KeyColumnSize, e.ValueSize)
}

// Little-endian int64 constants for atomic ADD mutations.
// Must match Java's FDBRecordStore constants exactly.
var (
	littleEndianInt64One      = encodeRecordCount(1)
	littleEndianInt64MinusOne = encodeRecordCount(-1)
	littleEndianInt64Zero     = encodeRecordCount(0)
)

// encodeRecordCount encodes a count as little-endian int64 bytes.
// Matches Java's FDBRecordStore.encodeRecordCount().
func encodeRecordCount(count int64) []byte {
	buf := make([]byte, 8)
	binary.LittleEndian.PutUint64(buf, uint64(count))
	return buf
}

// decodeRecordCount decodes a little-endian int64 count from bytes.
// Returns 0 for nil (matching Java's behavior where null means 0).
// Matches Java's FDBRecordStore.decodeRecordCount().
func decodeRecordCount(b []byte) int64 {
	if len(b) < 8 {
		return 0
	}
	return int64(binary.LittleEndian.Uint64(b))
}

// isRecordCountDisabled returns true if the record count state is DISABLED.
// Goroutine-safe via stateMu (read lock).
// Matches Java's addRecordCount which wraps the check in beginRecordStoreStateRead().
func (store *FDBRecordStore) isRecordCountDisabled() bool {
	store.stateMu.RLock()
	defer store.stateMu.RUnlock()
	if store.storeHeader == nil {
		return false
	}
	return store.storeHeader.GetRecordCountState() == gen.DataStoreInfo_DISABLED
}

// addRecordCount atomically increments or decrements the record count.
// Uses FDB's atomic ADD mutation for lock-free concurrent counting.
// Skips mutation when RecordCountState is DISABLED.
// Matches Java's FDBRecordStore.addRecordCount().
//
// Key format: subspace[RecordCountKey] + evaluated_count_key_tuple
func (store *FDBRecordStore) addRecordCount(record proto.Message, increment []byte) error {
	countKey := store.metaData.GetRecordCountKey()
	if countKey == nil {
		return nil // Counting not configured
	}
	if store.isRecordCountDisabled() {
		return nil // Count state is DISABLED — skip mutation
	}

	// Evaluate the count key expression against the record.
	// Count keys should produce exactly one tuple.
	subkeys, err := countKey.Evaluate(nil, record)
	if err != nil {
		return fmt.Errorf("record count key evaluation failed: %w", err)
	}
	if len(subkeys) != 1 {
		return fmt.Errorf("record count key must evaluate to exactly one tuple, got %d", len(subkeys))
	}
	subkey := subkeys[0]

	// Build the FDB key: subspace + RecordCountKey + evaluated_subkey
	countSubspace := store.subspace.Sub(RecordCountKey)
	keyTuple := make(tuple.Tuple, len(subkey))
	for i, v := range subkey {
		keyTuple[i] = v
	}
	fdbKey := countSubspace.Pack(keyTuple)

	// Atomic ADD — no read needed, no conflicts generated
	store.context.Transaction().Add(fdbKey, increment)
	return nil
}

// snapshotRecordCountFromCountKey reads the store's MAINTAINED record counts — the
// atomic counters under the RecordCountKey subspace — for the given evaluated value
// of the store's record-count key. ok=false means the counters are not a usable
// source (no record-count key configured, or its state is not READABLE), which is
// Java's "skip the whole `if` block and fall through to the index path".
//
// Port of the header branch of Java's getSnapshotRecordCount(key, value, filter)
// (FDBRecordStore.java:2286-2317). Java's `key` argument is implicit in this
// signature: countKey is Key.Evaluated, and the KeyExpression it was evaluated
// against is the store's own record-count key — or EmptyKeyExpression.EMPTY when
// countKey is empty, which is the only shape a caller asking for a store-wide total
// can mean.
//
// That gives Java's three arms exactly:
//
//   - A countKey whose length disagrees with the column size of the expression it
//     was evaluated against is Java's `key.getColumnSize() != value.size()` throw
//     (FDBRecordStore.java:2295-2297), raised BEFORE either read and only once the
//     counters are READABLE — a caller error, never a count. Without it the
//     packed slot for a wrong-width value simply does not exist and the store
//     answers a confident 0.
//
//   - A non-empty countKey, or an empty one against an EmptyKeyExpression count key,
//     is `recordMetaData.getRecordCountKey().equals(key)` — one get of one slot.
//
//   - An empty countKey against a NON-empty (grouped) count key is
//     `key.isPrefixKey(recordMetaData.getRecordCountKey())`, which is always true
//     because EmptyKeyExpression is a prefix of everything
//     (BaseKeyExpression.java:73-75), and Java answers it by summing EVERY group in
//     the count subspace. There is no Java call shape where that combination reads
//     the ungrouped slot instead: `equals` has already failed, so a store counting
//     by record type would report the ungrouped slot's 0 as its total.
//
// The range excludes the count subspace prefix itself, as Java's
// `getSubspace().range(Tuple.from(RECORD_COUNT_KEY))` does. Only the phantom zero
// that DeleteAllRecords writes lives exactly there, and it contributes nothing.
func (store *FDBRecordStore) snapshotRecordCountFromCountKey(countKey tuple.Tuple) (int64, bool, error) {
	recordCountKey := store.metaData.GetRecordCountKey()
	if recordCountKey == nil || !store.recordCountStateIsReadable() {
		return 0, false, nil
	}

	// Java's `key` is implicit in this signature (see above): an empty countKey came
	// from EmptyKeyExpression.EMPTY, whose column size is 0 and therefore always
	// agrees; a non-empty one can only have been evaluated against the store's own
	// record-count key, so that expression's column size is what it must match.
	if len(countKey) > 0 && recordCountKey.ColumnSize() != len(countKey) {
		return 0, false, &RecordCountKeySizeMismatchError{
			KeyColumnSize: recordCountKey.ColumnSize(),
			ValueSize:     len(countKey),
		}
	}

	countSubspace := store.subspace.Sub(RecordCountKey)
	// Snapshot read (non-conflicting), matching Java's context.readTransaction(true).
	tr := store.context.Transaction().Snapshot()

	_, ungroupedCountKey := recordCountKey.(*EmptyKeyExpression)
	if len(countKey) > 0 || ungroupedCountKey {
		value, err := tr.Get(countSubspace.Pack(countKey)).Get()
		if err != nil {
			return 0, false, fmt.Errorf("failed to read record count: %w", err)
		}
		return decodeRecordCount(value), true, nil
	}

	begin, end := countSubspace.FDBRangeKeys()
	// This read takes the client's DEFAULT streaming mode, which is exactly what Java
	// does: it reaches the same read through tr.getRange(Range) with no mode argument,
	// resolving to StreamingMode.ITERATOR (FDBTransaction.java:174-176, 430-432).
	//
	// That parity is recent, and it is worth recording why the default is now safe here,
	// because for a while it was not and this call site carried an explicit
	// StreamingModeSerial to say so. Two independent client defects made an unbounded
	// fold unsafe under ITERATOR; both are fixed:
	//
	//  1. ITERATOR did not saturate. libfdb_c translates ITERATOR into a per-fetch BYTE
	//     target that walks {4096, 6144, ..., 120000} and then STOPS growing, because the
	//     iteration index is clamped to the length of that table (bindings/c/fdb_c.cpp:1006,
	//     1019-1020) — so C's peak per-fetch buffer is ~120 KB however many groups exist.
	//     This client models the progression in ROWS rather than bytes, and used to double
	//     without any ceiling, so over the count subspace — whose size is the CARDINALITY of
	//     the record-count key — the largest fetch grew with the number of groups. A
	//     streaming fold on top of an unbounded fetch is Θ(groups) at peak anyway, one level
	//     below the fold but exactly the cost the fold was meant to remove. The progression
	//     now clamps at the same iteration index C++ clamps at (fdb/range_result.go
	//     iteratorMaxIteration), so it runs 2, 4, 8, ... 1024 and then stays at 1024 rows
	//     forever. Peak per fetch is a constant, independent of how many groups exist.
	//
	//  2. The RYW snapshot cache merged each fetched page into its neighbours by
	//     concatenating and re-sorting everything cached so far, which made any sequential
	//     scan quadratic in CPU and allocation and made the cost of caching depend on the
	//     mode's page SIZE. It now keeps one fragment per fetch, as C++'s SnapshotCache
	//     does. Page size no longer feeds back into caching cost at all.
	//
	// With both landed there is no bound Serial provides that the default does not, and
	// its peak fetch is in fact the smaller of the two only by accident of the row tables
	// (Serial 500 rows, saturated ITERATOR 1024) — a difference with no bearing on the
	// asymptotics, which are O(1) in the group count either way. Keeping Serial would have
	// been a gratuitous divergence from Java on a wire-neutral knob, so it is gone.
	//
	// What is NOT a defect, and must not be "fixed" here: the snapshot cache retains every
	// row of this scan for the life of the transaction. libfdb_c does exactly the same (its
	// SnapshotCache is arena-allocated with no eviction, and snapshot RYW is on by default),
	// so Java's roll-up retains Θ(groups) too. Disabling snapshot RYW at this call site to
	// shed that memory would diverge from Java rather than match it.
	//
	// The mode selects how many rows a fetch asks for. It changes nothing about what is
	// stored, what is read, or the total — and nothing about isolation: the read stays on
	// the snapshot transaction resolved above.
	total, err := sumRecordCounts(tr.GetRange(fdb.KeyRange{Begin: begin, End: end},
		fdb.RangeOptions{}))
	if err != nil {
		return 0, false, fmt.Errorf("read grouped record counts: %w", err)
	}
	return total, true, nil
}

// sumRecordCounts folds a range of record-count slots into their total, consuming the
// range as a STREAM and never materializing it.
//
// This is Java's MoreAsyncUtil.reduce(getExecutor(), kvs.iterator(), 0L,
// (count, kv) -> count + decodeRecordCount(kv.getValue())) over
// tr.getRange(getSubspace().range(Tuple.from(RECORD_COUNT_KEY)))
// (FDBRecordStore.java:2307-2310). Java carries no row limit, no continuation and no
// ExecuteProperties into that read — it is the raw AsyncIterable over the whole count
// subspace, at the isolation the enclosing `context.readTransaction(true)` already
// fixed (snapshot) — so the Go range carries no row limit either and takes the caller's
// snapshot transaction. The only thing ported here is the fold; the caller's choice of
// streaming mode is explained at the call site.
//
// Streaming is not a micro-optimization: the number of slots is the CARDINALITY of the
// store's record-count key, so a store counting by a high-cardinality group has as many
// slots as groups. Collecting them into a slice first makes an unbounded allocation on a
// path that both GetRecordCount and the evolution rebuild's count selection sit on. The
// accumulator is a single int64 and the iterator holds one batch at a time — but that
// bounds peak memory only if the BATCH is bounded, which is a property of the streaming
// mode the caller picks, not of this fold. See the call site.
func sumRecordCounts(rr fdb.RangeResult) (int64, error) {
	iter := rr.Iterator()
	var total int64
	for iter.Advance() {
		kv, err := iter.Get()
		if err != nil {
			return 0, err
		}
		total += decodeRecordCount(kv.Value)
	}
	// Advance() returns false on exhaustion OR error — check the stored Get() error so a
	// transient transaction_too_old (1007) is not folded in as "no more groups", which
	// would answer a plausible, too-small total instead of failing.
	if _, err := iter.Get(); err != nil {
		return 0, err
	}
	return total, nil
}

// GetSnapshotRecordCount returns the count of records matching the given evaluated
// value of the store's record-count key. Uses snapshot (non-conflicting) reads.
//
// For ungrouped counting (EmptyKeyExpression), pass an empty tuple.
// For per-type counting, pass a tuple with the record type key.
//
// Port of Java's getSnapshotRecordCount(key, value, filter)
// (FDBRecordStore.java:2283-2323). The maintained counters answer when they can (see
// snapshotRecordCountFromCountKey); otherwise Java does NOT fail — it falls through
// to evaluateAggregateFunction(emptyList(), count(key), allOf(value), SNAPSHOT,
// filter) (FDBRecordStore.java:2320-2322), so a store with no record-count key at
// all, or whose counters are WRITE_ONLY/DISABLED, is still counted from a universal
// COUNT index. The empty record-type list is what restricts that to universal
// indexes (IndexFunctionHelper.java:180-181).
//
// Returns 0 if no count exists (null in FDB means 0, matching Java).
func (store *FDBRecordStore) GetSnapshotRecordCount(countKey tuple.Tuple) (int64, error) {
	count, fromCountKey, err := store.snapshotRecordCountFromCountKey(countKey)
	if err != nil || fromCountKey {
		return count, err
	}

	// Java's `key`. An empty evaluated value came from EmptyKeyExpression.EMPTY; a
	// non-empty one can only have come from the store's own record-count key, so
	// without one there is no expression to count by and no Java call shape to port.
	countKeyExpr := EmptyKey()
	scanRange := TupleRangeAll
	if len(countKey) > 0 {
		recordCountKey := store.metaData.GetRecordCountKey()
		if recordCountKey == nil {
			return 0, fmt.Errorf("record counting is not enabled (recordCountKey is nil), so the count key %v cannot be interpreted", countKey)
		}
		countKeyExpr = recordCountKey
		scanRange = TupleRangeAllOf(countKey)
	}

	fn := NewCountAggregateFunction(GroupAll(countKeyExpr))
	result, err := store.EvaluateAggregateFunction(store.context.ctx, nil, fn, scanRange, IsolationLevelSnapshot)
	if err != nil {
		return 0, err
	}
	if len(result) == 0 {
		return 0, fmt.Errorf("count aggregate returned an empty tuple")
	}
	total, isInt := result[0].(int64)
	if !isInt {
		return 0, fmt.Errorf("count aggregate returned %T, want int64", result[0])
	}
	return total, nil
}

// GetRecordCount returns the total number of records in the store.
//
// This is Java's getSnapshotRecordCount() (FDBRecordStore.java:2283, called with
// EmptyKeyExpression.EMPTY / Key.Evaluated.EMPTY), and it is a TOTAL under every
// record-count key: a GROUPED count key is rolled up by summing every group, not
// read out of the ungrouped slot that no grouped store ever writes to.
func (store *FDBRecordStore) GetRecordCount() (int64, error) {
	return store.GetSnapshotRecordCount(tuple.Tuple{})
}

// GetSnapshotRecordCountForRecordType returns the count of records for a specific record type.
// Requires that the metadata uses RecordTypeKeyExpression as the count key.
// Matches Java's getSnapshotRecordCountForRecordType().
func (store *FDBRecordStore) GetSnapshotRecordCountForRecordType(recordTypeName string) (int64, error) {
	countKey := store.metaData.GetRecordCountKey()
	if countKey == nil {
		return 0, fmt.Errorf("record counting is not enabled (recordCountKey is nil)")
	}
	if !IsRecordTypeExpression(countKey) {
		return 0, fmt.Errorf("per-type counting requires RecordTypeKeyExpression as count key")
	}
	// Use the record type key (matching Java), not the string name.
	rt := store.metaData.GetRecordType(recordTypeName)
	if rt == nil {
		return 0, &MetaDataError{Message: fmt.Sprintf("unknown record type %q", recordTypeName)}
	}
	return store.GetSnapshotRecordCount(tuple.Tuple{rt.GetRecordTypeKey()})
}

// UpdateRecordCountState transitions the record count state.
// Valid transitions: READABLE↔WRITE_ONLY, any→DISABLED. DISABLED is terminal.
// When transitioning to DISABLED, clears all count data.
// Goroutine-safe via stateMu (write lock).
// Matches Java's FDBRecordStore.updateRecordCountStateAsync().
func (store *FDBRecordStore) UpdateRecordCountState(newState gen.DataStoreInfo_RecordCountState) error {
	store.stateMu.Lock()
	defer store.stateMu.Unlock()
	if store.storeHeader == nil {
		return &RecordStoreStateNotLoadedError{}
	}

	existing := store.storeHeader.GetRecordCountState()
	if existing == newState {
		return nil // No-op
	}

	// Validate transition. Matches Java's state machine:
	// READABLE → WRITE_ONLY: allowed
	// WRITE_ONLY → READABLE: allowed
	// any → DISABLED: allowed
	// DISABLED → anything: forbidden (terminal state)
	if existing == gen.DataStoreInfo_DISABLED {
		return fmt.Errorf("invalid state transition for RecordCountState: DISABLED → %s (DISABLED is terminal)", newState)
	}
	toWriteOnly := existing == gen.DataStoreInfo_READABLE && newState == gen.DataStoreInfo_WRITE_ONLY
	toReadable := existing == gen.DataStoreInfo_WRITE_ONLY && newState == gen.DataStoreInfo_READABLE
	toDisabled := newState == gen.DataStoreInfo_DISABLED
	if !toWriteOnly && !toReadable && !toDisabled {
		return fmt.Errorf("invalid state transition for RecordCountState: %s → %s", existing, newState)
	}

	// When transitioning to DISABLED, clear all count data.
	// Use PrefixRange to include the exact prefix key — ungrouped counts
	// are stored at countSubspace.Pack(tuple.Tuple{}) which equals the prefix.
	if toDisabled {
		countSubspace := store.subspace.Sub(RecordCountKey)
		pr, err := fdb.PrefixRange(countSubspace.Bytes())
		if err != nil {
			return fmt.Errorf("record count prefix range: %w", err)
		}
		store.context.Transaction().ClearRange(pr)
	}

	store.storeHeader.RecordCountState = &newState
	return store.writeStoreHeader(store.storeHeader)
}
