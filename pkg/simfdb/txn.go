package simfdb

import (
	"bytes"
	"sort"

	fdb "fdb.dev/pkg/fdbgo/fdb"
)

// mutationKind tags a queued write in the RYW buffer.
type mutationKind int

const (
	mutSet mutationKind = iota
	mutClear
	mutClearRange
	mutAtomic
	mutVersionstampedKey
	mutVersionstampedValue
)

// mutation is one queued write. For mutClearRange, key/end bound the range; otherwise key is
// the target and value is the Set value / atomic param / versionstamp param. noWriteConflict
// records that SetNextWriteNoWriteConflictRange was active when this write was issued (the
// versionstamp path), so it adds no write conflict range here.
type mutation struct {
	kind            mutationKind
	key             []byte
	end             []byte
	value           []byte
	op              atomicOp
	noWriteConflict bool
}

// keyRange is a half-open [begin, end) byte range used for conflict tracking.
type keyRange struct{ begin, end []byte }

// simTxn is SimFDB's WritableTransaction. It is single-goroutine (the sim driver serializes
// use); the db mutex only guards the shared store/version counter against a concurrent commit.
type simTxn struct {
	db *SimDB

	readVersion int64
	rvSet       bool

	buffer         []mutation // RYW write buffer, in issue order
	readConflicts  []keyRange
	writeConflicts []keyRange

	// SetNextWriteNoWriteConflictRange arms this for exactly the next write (FDB semantics).
	nextWriteNoConflict bool
	// SetReadYourWritesDisable — record layer rarely sets it; when true, reads skip the buffer.
	rywDisabled bool
	// SetWriteConflictsDisabled — when true, writes add no write conflict ranges at all.
	writeConflictsDisabled bool

	snapshot bool // a Snapshot() view: reads add no read conflict ranges

	committed        bool
	committedVersion int64
	cancelled        bool

	// versionstamp assigned at commit (10-byte tx version), for GetVersionstamp.
	versionstamp []byte

	opts fdb.TransactionOptions
}

var _ fdb.WritableTransaction = (*simTxn)(nil)

func (db *SimDB) newTxn(snapshot bool) *simTxn {
	tx := &simTxn{db: db, snapshot: snapshot}
	tx.opts = &txnOptions{tx: tx}
	return tx
}

// ensureReadVersion lazily pins the read version at the highest committed version, the first
// time the transaction reads or commits (matching a lazy GRV).
func (tx *simTxn) ensureReadVersion() {
	if tx.rvSet {
		return
	}
	tx.db.mu.Lock()
	tx.readVersion = tx.db.currentReadVersion()
	tx.db.mu.Unlock()
	tx.rvSet = true
}

// ---- reads ----------------------------------------------------------------------------

// cloneVal returns a copy of v that preserves nil-ness: nil stays nil (absent/cleared), a
// present value — including an empty one — stays non-nil.
func cloneVal(v []byte) []byte {
	if v == nil {
		return nil
	}
	c := make([]byte, len(v))
	copy(c, v)
	return c
}

// snapshotValue returns key's committed value at the read version, copied.
func (tx *simTxn) snapshotValue(key []byte) []byte {
	tx.db.mu.Lock()
	v := tx.db.store.valueAt(key, tx.readVersion)
	tx.db.mu.Unlock()
	return cloneVal(v)
}

// resolveKey computes the RYW-merged value of key: the snapshot value with the transaction's
// own pending writes replayed in order. nil means absent/cleared.
func (tx *simTxn) resolveKey(key []byte) []byte {
	cur := tx.snapshotValue(key)
	if tx.rywDisabled {
		return cur
	}
	for _, m := range tx.buffer {
		switch m.kind {
		case mutSet, mutVersionstampedValue:
			if bytes.Equal(m.key, key) {
				cur = cloneVal(m.value)
			}
		case mutVersionstampedKey:
			// The key isn't finalized until commit; a RYW read of the placeholder key sees
			// its param value. Exact-key match on the (still-placeholder) key.
			if bytes.Equal(m.key, key) {
				cur = cloneVal(m.value)
			}
		case mutClear:
			if bytes.Equal(m.key, key) {
				cur = nil
			}
		case mutClearRange:
			if bytes.Compare(key, m.key) >= 0 && bytes.Compare(key, m.end) < 0 {
				cur = nil
			}
		case mutAtomic:
			if bytes.Equal(m.key, key) {
				nv, clear := applyAtomic(m.op, cur, m.value)
				if clear {
					cur = nil
				} else {
					cur = nv
				}
			}
		}
	}
	return cur
}

func (tx *simTxn) Get(key fdb.KeyConvertible) fdb.FutureByteSlice {
	if tx.cancelled {
		return newReadyByteSlice(nil, fdb.Error{Code: 1025}) // transaction_cancelled
	}
	tx.ensureReadVersion()
	k := []byte(key.FDBKey())
	if !tx.snapshot {
		tx.addReadConflict(k, keyAfter(k))
	}
	return newReadyByteSlice(tx.resolveKey(k), nil)
}

// buildView returns the RYW-merged, sorted, live keyspace at the read version. Used to resolve
// key selectors for GetKey and GetRange. A transient map coalesces per-key state; the result
// is re-sorted so iteration order is deterministic regardless of map order.
func (tx *simTxn) buildView() []fdb.KeyValue {
	m := make(map[string][]byte)
	tx.db.mu.Lock()
	for _, e := range tx.db.store.entries {
		if v := tx.db.store.valueAtEntry(e, tx.readVersion); v != nil {
			m[string(e.key)] = cloneVal(v)
		}
	}
	tx.db.mu.Unlock()
	if !tx.rywDisabled {
		for _, mut := range tx.buffer {
			applyMutationToView(m, mut)
		}
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]fdb.KeyValue, len(keys))
	for i, k := range keys {
		out[i] = fdb.KeyValue{Key: fdb.Key(k), Value: m[k]}
	}
	return out
}

// applyMutationToView folds one pending mutation into the merged-view map.
func applyMutationToView(m map[string][]byte, mut mutation) {
	switch mut.kind {
	case mutSet, mutVersionstampedKey, mutVersionstampedValue:
		m[string(mut.key)] = cloneVal(mut.value)
	case mutClear:
		delete(m, string(mut.key))
	case mutClearRange:
		for k := range m {
			if bytes.Compare([]byte(k), mut.key) >= 0 && bytes.Compare([]byte(k), mut.end) < 0 {
				delete(m, k)
			}
		}
	case mutAtomic:
		nv, clear := applyAtomic(mut.op, m[string(mut.key)], mut.value)
		if clear {
			delete(m, string(mut.key))
		} else {
			m[string(mut.key)] = nv
		}
	}
}

// resolveSelector returns the index into view of the key selected by ks. The result may be < 0
// (before the first key) or >= len(view) (past the last key). FDB selector arithmetic: base =
// keyAfter(key) if OrEqual else key; i = first index with view[i].Key >= base; target = i +
// Offset - 1.
func resolveSelector(view []fdb.KeyValue, ks fdb.KeySelector) int {
	base := []byte(ks.Key.FDBKey())
	if ks.OrEqual {
		base = keyAfter(base)
	}
	i := sort.Search(len(view), func(i int) bool {
		return bytes.Compare(view[i].Key, base) >= 0
	})
	return i + ks.Offset - 1
}

// endKeyMarker is the resolved key returned when a selector points past the last key: the
// exclusive upper bound of the user keyspace (\xff), matching FDB's clamp.
var endKeyMarker = fdb.Key{0xff}

func (tx *simTxn) GetKey(sel fdb.Selectable) fdb.FutureKey {
	if tx.cancelled {
		return newReadyKey(nil, fdb.Error{Code: 1025})
	}
	tx.ensureReadVersion()
	ks := sel.FDBKeySelector()
	view := tx.buildView()
	idx := resolveSelector(view, ks)
	var result fdb.Key
	switch {
	case idx < 0:
		result = fdb.Key{} // before the first key: empty key
	case idx >= len(view):
		result = endKeyMarker
	default:
		result = append(fdb.Key(nil), view[idx].Key...)
	}
	if !tx.snapshot {
		// The read touched the resolved key; conservatively conflict on it.
		tx.addReadConflict([]byte(ks.Key.FDBKey()), keyAfter([]byte(result)))
	}
	return newReadyKey(result, nil)
}

func (tx *simTxn) GetRange(r fdb.Range, options fdb.RangeOptions) fdb.RangeResult {
	tx.ensureReadVersion()
	beginSel, endSel := r.FDBRangeKeySelectors()
	view := tx.buildView()
	bi := clampIndex(resolveSelector(view, beginSel.FDBKeySelector()), len(view))
	ei := clampIndex(resolveSelector(view, endSel.FDBKeySelector()), len(view))
	var kvs []fdb.KeyValue
	if bi < ei {
		kvs = append(kvs, view[bi:ei]...)
	}
	if options.Reverse {
		reverseKVs(kvs)
	}
	if options.Limit > 0 && len(kvs) > options.Limit {
		kvs = kvs[:options.Limit]
	}
	if !tx.snapshot {
		// Clamp the read conflict range to the data actually returned (RFC-179: GetRange
		// conflicts on the returned extent, not the requested span) so SimFDB does not
		// over-conflict relative to real FDB.
		if len(kvs) > 0 {
			lo, hi := kvs[0].Key, kvs[len(kvs)-1].Key
			if bytes.Compare(lo, hi) > 0 {
				lo, hi = hi, lo
			}
			tx.addReadConflict([]byte(lo), keyAfter([]byte(hi)))
		}
	}
	return newReadyRangeResult(kvs)
}

func clampIndex(i, n int) int {
	if i < 0 {
		return 0
	}
	if i > n {
		return n
	}
	return i
}

func reverseKVs(kvs []fdb.KeyValue) {
	for i, j := 0, len(kvs)-1; i < j; i, j = i+1, j-1 {
		kvs[i], kvs[j] = kvs[j], kvs[i]
	}
}

func (tx *simTxn) GetReadVersion() fdb.FutureInt64 {
	tx.ensureReadVersion()
	return newReadyInt64(tx.readVersion, nil)
}

func (tx *simTxn) Snapshot() fdb.ReadTransaction {
	snap := *tx
	snap.snapshot = true
	return &snap
}

func (tx *simTxn) GetEstimatedRangeSizeBytes(r fdb.ExactRange) fdb.FutureInt64 {
	tx.ensureReadVersion()
	begin, end := r.FDBRangeKeys()
	b, e := []byte(begin.FDBKey()), []byte(end.FDBKey())
	tx.db.mu.Lock()
	kvs := tx.db.store.rangeAt(b, e, tx.readVersion)
	tx.db.mu.Unlock()
	var total int64
	for _, kv := range kvs {
		total += int64(len(kv.Key) + len(kv.Value))
	}
	return newReadyInt64(total, nil)
}

func (tx *simTxn) GetRangeSplitPoints(r fdb.ExactRange, chunkSize int64) fdb.FutureKeyArray {
	// Single logical shard: no interior split points (begin/end only, per FDB when the range
	// is smaller than a shard). v1 returns the empty set of interior boundaries.
	return newReadyKeyArray(nil, nil)
}

func (tx *simTxn) Options() fdb.TransactionOptions { return tx.opts }

// ---- writes ---------------------------------------------------------------------------

// enqueue appends a mutation and (unless armed off) its write conflict range. For a point
// write the range is [key, keyAfter(key)); for a clear range it is [begin, end).
func (tx *simTxn) enqueue(m mutation) {
	m.noWriteConflict = tx.nextWriteNoConflict
	tx.nextWriteNoConflict = false // arms exactly one write (FDB semantics)
	tx.buffer = append(tx.buffer, m)
	if !m.noWriteConflict && !tx.writeConflictsDisabled {
		switch m.kind {
		case mutClearRange:
			tx.addWriteConflict(m.key, m.end)
		default:
			tx.addWriteConflict(m.key, keyAfter(m.key))
		}
	}
}

func (tx *simTxn) Set(key fdb.KeyConvertible, value []byte) {
	tx.enqueue(mutation{kind: mutSet, key: []byte(key.FDBKey()), value: cloneVal(value)})
}

func (tx *simTxn) Clear(key fdb.KeyConvertible) {
	tx.enqueue(mutation{kind: mutClear, key: []byte(key.FDBKey())})
}

func (tx *simTxn) ClearRange(er fdb.ExactRange) {
	b, e := er.FDBRangeKeys()
	tx.enqueue(mutation{kind: mutClearRange, key: []byte(b.FDBKey()), end: []byte(e.FDBKey())})
}

func (tx *simTxn) atomic(op atomicOp, key fdb.KeyConvertible, param []byte) {
	tx.enqueue(mutation{kind: mutAtomic, op: op, key: []byte(key.FDBKey()), value: cloneVal(param)})
}
func (tx *simTxn) Add(key fdb.KeyConvertible, param []byte)     { tx.atomic(atomicAdd, key, param) }
func (tx *simTxn) And(key fdb.KeyConvertible, param []byte)     { tx.atomic(atomicAnd, key, param) }
func (tx *simTxn) BitAnd(key fdb.KeyConvertible, param []byte)  { tx.atomic(atomicAnd, key, param) }
func (tx *simTxn) Or(key fdb.KeyConvertible, param []byte)      { tx.atomic(atomicOr, key, param) }
func (tx *simTxn) BitOr(key fdb.KeyConvertible, param []byte)   { tx.atomic(atomicOr, key, param) }
func (tx *simTxn) Xor(key fdb.KeyConvertible, param []byte)     { tx.atomic(atomicXor, key, param) }
func (tx *simTxn) BitXor(key fdb.KeyConvertible, param []byte)  { tx.atomic(atomicXor, key, param) }
func (tx *simTxn) Max(key fdb.KeyConvertible, param []byte)     { tx.atomic(atomicMax, key, param) }
func (tx *simTxn) Min(key fdb.KeyConvertible, param []byte)     { tx.atomic(atomicMin, key, param) }
func (tx *simTxn) ByteMax(key fdb.KeyConvertible, param []byte) { tx.atomic(atomicByteMax, key, param) }
func (tx *simTxn) ByteMin(key fdb.KeyConvertible, param []byte) { tx.atomic(atomicByteMin, key, param) }
func (tx *simTxn) AppendIfFits(key fdb.KeyConvertible, param []byte) {
	tx.atomic(atomicAppendIfFits, key, param)
}

func (tx *simTxn) CompareAndClear(key fdb.KeyConvertible, param []byte) {
	tx.atomic(atomicCompareAndClear, key, param)
}

func (tx *simTxn) SetVersionstampedKey(key fdb.KeyConvertible, param []byte) {
	tx.enqueue(mutation{kind: mutVersionstampedKey, key: []byte(key.FDBKey()), value: cloneVal(param)})
}

func (tx *simTxn) SetVersionstampedValue(key fdb.KeyConvertible, param []byte) {
	tx.enqueue(mutation{kind: mutVersionstampedValue, key: []byte(key.FDBKey()), value: cloneVal(param)})
}

// []byte fast-path overloads: delegate to the KeyConvertible forms (fdb.Key is a KeyConvertible).
func (tx *simTxn) SetBytes(key, value []byte)         { tx.Set(fdb.Key(key), value) }
func (tx *simTxn) ClearBytes(key []byte)              { tx.Clear(fdb.Key(key)) }
func (tx *simTxn) AddBytes(key, param []byte)         { tx.Add(fdb.Key(key), param) }
func (tx *simTxn) MaxBytes(key, param []byte)         { tx.Max(fdb.Key(key), param) }
func (tx *simTxn) MinBytes(key, param []byte)         { tx.Min(fdb.Key(key), param) }
func (tx *simTxn) CompareAndClearBytes(key, p []byte) { tx.CompareAndClear(fdb.Key(key), p) }

// ---- conflict ranges ------------------------------------------------------------------

func (tx *simTxn) addReadConflict(begin, end []byte) {
	tx.readConflicts = append(tx.readConflicts, keyRange{append([]byte(nil), begin...), append([]byte(nil), end...)})
}

func (tx *simTxn) addWriteConflict(begin, end []byte) {
	tx.writeConflicts = append(tx.writeConflicts, keyRange{append([]byte(nil), begin...), append([]byte(nil), end...)})
}

func (tx *simTxn) AddReadConflictRange(er fdb.ExactRange) error {
	b, e := er.FDBRangeKeys()
	tx.addReadConflict([]byte(b.FDBKey()), []byte(e.FDBKey()))
	return nil
}

func (tx *simTxn) AddReadConflictKey(key fdb.KeyConvertible) error {
	k := []byte(key.FDBKey())
	tx.addReadConflict(k, keyAfter(k))
	return nil
}

func (tx *simTxn) AddWriteConflictRange(er fdb.ExactRange) error {
	b, e := er.FDBRangeKeys()
	tx.addWriteConflict([]byte(b.FDBKey()), []byte(e.FDBKey()))
	return nil
}

func (tx *simTxn) AddWriteConflictKey(key fdb.KeyConvertible) error {
	k := []byte(key.FDBKey())
	tx.addWriteConflict(k, keyAfter(k))
	return nil
}

// ---- lifecycle ------------------------------------------------------------------------

func (tx *simTxn) Commit() fdb.FutureNil {
	if tx.cancelled {
		return newReadyNil(fdb.Error{Code: 1025})
	}
	if tx.committed {
		return newReadyNil(fdb.Error{Code: 2017}) // used_during_commit-ish; committed twice
	}
	if err := tx.db.commit(tx); err != nil {
		return newReadyNil(err)
	}
	return newReadyNil(nil)
}

func (tx *simTxn) Cancel() { tx.cancelled = true }

// Reset returns the transaction to a fresh state for reuse (matching FDB Transaction.reset):
// clears the buffer, conflict ranges, and read version; keeps the db handle.
func (tx *simTxn) Reset() {
	tx.readVersion = 0
	tx.rvSet = false
	tx.buffer = nil
	tx.readConflicts = nil
	tx.writeConflicts = nil
	tx.nextWriteNoConflict = false
	tx.committed = false
	tx.committedVersion = 0
	tx.cancelled = false
	tx.versionstamp = nil
}

// OnError classifies e: for a retryable code it resets the transaction (fresh read version,
// dropped buffer) and resolves success so the runner retries; otherwise it resolves the error.
// Matches FDB Transaction.onError.
func (tx *simTxn) OnError(e fdb.Error) fdb.FutureNil {
	if fdb.IsOnErrorRetryable(e.Code) {
		tx.Reset()
		return newReadyNil(nil)
	}
	return newReadyNil(e)
}

func (tx *simTxn) SetReadVersion(version int64) {
	tx.readVersion = version
	tx.rvSet = true
}

// ---- post-commit ----------------------------------------------------------------------

func (tx *simTxn) GetCommittedVersion() (int64, error) {
	if !tx.committed {
		return -1, fdb.Error{Code: 2017} // commit not yet called / no committed version
	}
	return tx.committedVersion, nil
}

func (tx *simTxn) GetVersionstamp() fdb.FutureKey {
	if !tx.committed || tx.versionstamp == nil {
		return newReadyKey(nil, fdb.Error{Code: 2017})
	}
	return newReadyKey(append(fdb.Key(nil), tx.versionstamp...), nil)
}

func (tx *simTxn) GetApproximateSize() fdb.FutureInt64 {
	var size int64
	for _, m := range tx.buffer {
		size += int64(len(m.key) + len(m.end) + len(m.value))
	}
	return newReadyInt64(size, nil)
}

// A transaction is itself a Transactor (the interface embeds ReadTransactor via
// ReadTransaction, and WritableTransaction is used where a Transactor is expected in the
// record layer's nested-context paths): run fn against itself, no new transaction.
func (tx *simTxn) Transact(fn func(fdb.WritableTransaction) (any, error)) (any, error) {
	return fn(tx)
}

func (tx *simTxn) ReadTransact(fn func(fdb.ReadTransaction) (any, error)) (any, error) {
	return fn(tx)
}
