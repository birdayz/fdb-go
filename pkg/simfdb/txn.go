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

	committed        bool
	committedVersion int64
	cancelled        bool

	// versionstamp assigned at commit (10-byte tx version), for GetVersionstamp.
	versionstamp []byte

	opts fdb.TransactionOptions
}

var _ fdb.WritableTransaction = (*simTxn)(nil)

func (db *SimDB) newTxn() *simTxn {
	tx := &simTxn{db: db}
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
	return tx.get(key, false)
}

func (tx *simTxn) get(key fdb.KeyConvertible, snapshot bool) fdb.FutureByteSlice {
	if tx.cancelled {
		return newReadyByteSlice(nil, fdb.Error{Code: 1025}) // transaction_cancelled
	}
	tx.ensureReadVersion()
	k := []byte(key.FDBKey())
	if !snapshot {
		tx.addFilteredReadConflictKey(k)
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
	return tx.getKey(sel, false)
}

func (tx *simTxn) getKey(sel fdb.Selectable, snapshot bool) fdb.FutureKey {
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
	if !snapshot {
		tx.addGetKeyConflictRange(ks, []byte(result))
	}
	return newReadyKey(result, nil)
}

// addGetKeyConflictRange records the read-conflict range a non-snapshot GetKey takes when it
// resolves ks to resolved — a port of the client's addGetKeyConflictRange
// (C++ addConflictRange(GetKeyReq)). The span runs between the selector base and the resolved
// key, ORIENTED BY THE OFFSET SIGN: a backward selector (offset <= 0) resolves BELOW the base,
// so the span is [resolved, base); a naive [base, keyAfter(resolved)) would be inverted and add
// no conflict at all on reverse cursors.
//
// GetRange shares this: for a bound the real backend has to resolve with an actual GetKey, the
// resolution ITSELF is a read and takes this range. See the call site there.
func (tx *simTxn) addGetKeyConflictRange(ks fdb.KeySelector, resolved []byte) {
	selKey := []byte(ks.Key.FDBKey())
	var cBegin, cEnd []byte
	if ks.Offset <= 0 {
		cBegin = resolved
		if ks.OrEqual {
			cEnd = keyAfter(selKey)
		} else {
			cEnd = selKey
		}
	} else {
		if ks.OrEqual {
			cBegin = keyAfter(selKey)
		} else {
			cBegin = selKey
		}
		cEnd = keyAfter(resolved)
	}
	if bytes.Compare(cBegin, cEnd) < 0 {
		tx.addFilteredReadConflict(cBegin, cEnd)
	}
}

// selectorResolvesViaGetKey reports whether the real backend has to issue an actual GetKey to
// turn ks into a byte key, mirroring fdb/range_result.go resolveSelector's two client-side
// short-circuits: FirstGreaterOrEqual(k) IS k, and FirstGreaterThan(k) is keyAfter(k). Every
// other selector costs a GetKey — which is a READ, and therefore takes a conflict range.
func selectorResolvesViaGetKey(ks fdb.KeySelector) bool {
	if !ks.OrEqual && ks.Offset == 1 { // FirstGreaterOrEqual
		return false
	}
	if ks.OrEqual && ks.Offset == 1 { // FirstGreaterThan
		return false
	}
	return true
}

func (tx *simTxn) GetRange(r fdb.Range, options fdb.RangeOptions) fdb.RangeResult {
	return tx.getRange(r, options, false)
}

func (tx *simTxn) getRange(r fdb.Range, options fdb.RangeOptions, snapshot bool) fdb.RangeResult {
	// transaction_cancelled(1025) before anything else, as on every other read entry point: the
	// client reaches it through ensureReadVersion, whose FIRST act is checkCancelled
	// (client/transaction.go:662-665), and getRangeDir calls ensureReadVersion before its own
	// legal-range and limit validation (client/transaction.go:1252-1268). Get and GetKey already
	// gate here; GetRange did not, so a cancelled transaction could still scan the keyspace — and
	// a sim that answers reads no real client answers lets a test "verify" behaviour that does
	// not exist.
	if tx.cancelled {
		return &readyRangeResult{err: fdb.Error{Code: 1025}}
	}
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
	more := false
	if options.Limit > 0 && len(kvs) >= options.Limit {
		kvs = kvs[:options.Limit]
		// more is true whenever the limit was MET, not only when strictly more rows exist.
		// C++ getExactRange sets output.more = (data.size() == limit) unconditionally when the
		// limit is reached (client/readpath.go:745-751 ports this) — a real storage server
		// stops at the limit and cannot know whether the next key exists, so it always reports
		// more. SimFDB holds the whole result set and could tell, but reporting more=false at
		// exactly-the-limit is a DIVERGENCE with two consequences: the conflict extent stays
		// the full requested range instead of narrowing to the returned data (over-conflict),
		// and a cursor over SimFDB sees a different exhaustion signal than over real FDB.
		more = true
	}
	// The conflict extent is derived from the RESOLVED [begin,end), not the raw selector
	// anchors: the real backend resolves a non-trivial selector to a key BEFORE issuing the
	// read (fdb/range_result.go resolveRange). Taking the anchor instead under-conflicts on a
	// backward selector — LastLessThan(k) resolves BELOW k, so rows the transaction really read
	// fall outside the recorded range and a concurrent write to them does not conflict.
	bks, eks := beginSel.FDBKeySelector(), endSel.FDBKeySelector()
	reqBegin := resolveRangeBound(view, bks)
	reqEnd := resolveRangeBound(view, eks)

	if !snapshot {
		// Resolving a non-trivial bound is itself a READ, and it happens at GetRange() time —
		// before any data batch — so its conflict is recorded here rather than deferred with
		// the extent. The real backend turns such a bound into a key by issuing a non-snapshot
		// GetKey (fdb/range_result.go resolveRange -> resolveSelector), and that GetKey takes
		// its own conflict range over the span it had to scan. Omitting it under-conflicts in a
		// way the range extent alone cannot express: when both bounds resolve to the SAME key
		// the range is empty and contributes nothing, yet the resolution still read the
		// keyspace between each anchor and its resolved key. A concurrent write there conflicts
		// on real FDB and did not here.
		for _, ks := range [2]fdb.KeySelector{bks, eks} {
			if selectorResolvesViaGetKey(ks) {
				tx.addGetKeyConflictRange(ks, resolveRangeBound(view, ks))
			}
		}
	}

	// The client's guard: an INVERTED requested range records no data conflict at all
	// (client/transaction.go:1290-1293). Without it a degenerate or inverted [begin,end)
	// reaches the conflict list, where rangesOverlap can still match it against a real write
	// range and abort a transaction that read nothing. Collapse it to an empty extent so the
	// deferred registration finds nothing to record.
	if bytes.Compare(reqBegin, reqEnd) > 0 {
		reqBegin, reqEnd = nil, nil
	}

	// The extent is registered when the result is CONSUMED, not here — per fetched batch for an
	// iterator, in one go for GetSlice. See readyRangeResult.
	return &readyRangeResult{
		tx:       tx,
		snapshot: snapshot,
		kvs:      kvs,
		more:     more,
		options:  options,
		reqBegin: reqBegin,
		reqEnd:   reqEnd,
	}
}

// resolveRangeBound returns the byte key a GetRange bound resolves to, mirroring the real
// backend's resolveSelector case for case (fdb/range_result.go:309-355) so the conflict
// extent SimFDB records is the one the client would have recorded:
//
//   - FirstGreaterOrEqual(k) is trivial and resolves to k with no lookup. Every exact range
//     in the tree (KeyRange, subspace, Tuple) reports its bounds as FirstGreaterOrEqual of
//     its raw keys, so this arm is also what keeps phantom protection intact for the common
//     case: [a,z) stays [a,z) including its leading and trailing gaps, rather than narrowing
//     to the first and last keys that happen to exist. The backend's separate ExactRange
//     short-circuit is a round-trip optimization it needs and SimFDB does not — there is no
//     GetKey to avoid here, and adding the branch would only duplicate this arm.
//   - FirstGreaterThan(k) (OrEqual && Offset==1) resolves to keyAfter(k) in the KEY SPACE,
//     not to the next key that exists. The distinction is observable: resolving to an
//     existing key would pull the gap between k and that key into the conflict range and
//     over-conflict on an insert there.
//   - everything else (LastLessThan / LastLessOrEqual / any other offset) needs the GetKey
//     resolution, which is index-based over the merged view, with GetKey's own clamps: the
//     empty key before the first key, endKeyMarker past the last.
func resolveRangeBound(view []fdb.KeyValue, ks fdb.KeySelector) []byte {
	if !ks.OrEqual && ks.Offset == 1 { // FirstGreaterOrEqual — trivial
		return []byte(ks.Key.FDBKey())
	}
	if ks.OrEqual && ks.Offset == 1 { // FirstGreaterThan(k) == FirstGreaterOrEqual(k+\x00)
		return keyAfter([]byte(ks.Key.FDBKey()))
	}
	switch idx := resolveSelector(view, ks); {
	case idx < 0:
		return []byte{}
	case idx >= len(view):
		return append([]byte(nil), endKeyMarker...)
	default:
		return append([]byte(nil), view[idx].Key...)
	}
}

// rangeConflictExtent clamps a GetRange's read-conflict range exactly as the pure-Go client and
// libfdb_c do (client transaction.go rangeConflictExtent). begin/end are the requested range
// bounds. A !more (exhausted) or empty read keeps the full [begin,end) — phantom protection; only
// a limit-truncated non-empty read narrows to the returned data (forward: [begin,keyAfter(last));
// reverse: [first,end)). kvs[len-1] is the highest returned (forward) or lowest (reverse).
func rangeConflictExtent(begin, end []byte, kvs []fdb.KeyValue, more, reverse bool) (cBegin, cEnd []byte) {
	if !more || len(kvs) == 0 {
		return begin, end
	}
	last := []byte(kvs[len(kvs)-1].Key)
	if reverse {
		return last, end
	}
	return begin, keyAfter(last)
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
	// The client's GetReadVersion is ensureReadVersion + a field read
	// (client/transaction.go:2435-2447), and ensureReadVersion opens with checkCancelled
	// (client/transaction.go:662-665).
	if tx.cancelled {
		return newReadyInt64(0, fdb.Error{Code: 1025})
	}
	tx.ensureReadVersion()
	return newReadyInt64(tx.readVersion, nil)
}

func (tx *simTxn) Snapshot() fdb.ReadTransaction {
	return simSnapshot{tx: tx}
}

// simSnapshot is the Snapshot() view of a transaction. It HOLDS the transaction — the shape of
// the real client's snapshot struct (fdb/snapshot.go: `type snapshot struct { tx *transaction }`,
// every method reaching through sn.s.tx) — rather than copying it.
//
// A value copy is not a snapshot, it is a FORK, and each forked field breaks a different
// invariant:
//
//   - readVersion/rvSet: a copy that pins the GRV first leaves the parent to pin its own, later,
//     one. The parent then resolves its read conflicts against a version NEWER than the one the
//     snapshot read at, so a concurrent write in between is invisible to the resolver and the
//     transaction commits on stale data — a silent lost update where real FDB returns 1020. The
//     record layer writes exactly that shape: BunchedMap.Put snapshot-reads the bunch, then
//     writes the merged bunch back under an explicit read conflict on the same key.
//   - buffer: the copy freezes the write buffer at its length as of Snapshot(). Writes issued on
//     the parent afterwards are then invisible to snapshot reads — but a real FDB snapshot read
//     is read-your-writes (SNAPSHOT_RYW_ENABLE is the default); snapshot suppresses CONFLICT
//     RANGES, never the RYW merge.
//   - opts: the copy's opts still points at the parent's txnOptions, so an option set through the
//     snapshot handle lands on the parent and the copy's own flag fields silently disagree.
//
// Holding the transaction makes all three correct by construction: there is one readVersion, one
// buffer, one option set. The only difference between the two views is that reads through this
// one pass snapshot=true and so add no read-conflict range.
type simSnapshot struct{ tx *simTxn }

var _ fdb.ReadTransaction = simSnapshot{}

func (s simSnapshot) Get(key fdb.KeyConvertible) fdb.FutureByteSlice {
	return s.tx.get(key, true)
}

func (s simSnapshot) GetKey(sel fdb.Selectable) fdb.FutureKey {
	return s.tx.getKey(sel, true)
}

func (s simSnapshot) GetRange(r fdb.Range, options fdb.RangeOptions) fdb.RangeResult {
	return s.tx.getRange(r, options, true)
}

func (s simSnapshot) GetReadVersion() fdb.FutureInt64 { return s.tx.GetReadVersion() }

// Snapshot of a snapshot is the same view (fdb/snapshot.go: `func (sn Snapshot) Snapshot()
// ReadTransaction { return sn }`).
func (s simSnapshot) Snapshot() fdb.ReadTransaction { return s }

func (s simSnapshot) GetEstimatedRangeSizeBytes(r fdb.ExactRange) fdb.FutureInt64 {
	return s.tx.GetEstimatedRangeSizeBytes(r)
}

func (s simSnapshot) GetRangeSplitPoints(r fdb.ExactRange, chunkSize int64) fdb.FutureKeyArray {
	return s.tx.GetRangeSplitPoints(r, chunkSize)
}

// Options reaches through to the transaction's option handle, as the real client's
// Snapshot.Options does (goTransactionOptions{tx: sn.s.tx}): options are a property of the
// transaction, not of the view.
func (s simSnapshot) Options() fdb.TransactionOptions { return s.tx.Options() }

func (s simSnapshot) ReadTransact(fn func(fdb.ReadTransaction) (any, error)) (any, error) {
	return fn(s)
}

func (tx *simTxn) GetEstimatedRangeSizeBytes(r fdb.ExactRange) fdb.FutureInt64 {
	begin, end := r.FDBRangeKeys()
	b, e := []byte(begin.FDBKey()), []byte(end.FDBKey())
	// The client's precedence, verbatim (client/metrics.go:29-38): inverted_range(2005) FIRST —
	// libfdb_c's KeyRangeRef constructor throws before the metric op runs — and only then
	// transaction_cancelled(1025), which this path must gate EXPLICITLY because it never fetches a
	// read version and so never passes through ensureReadVersion's checkCancelled. Adding the
	// cancel gate without the 2005 above it would invert the order the client reports.
	if bytes.Compare(b, e) > 0 {
		return newReadyInt64(0, fdb.Error{Code: 2005}) // inverted_range
	}
	if tx.cancelled {
		return newReadyInt64(0, fdb.Error{Code: 1025})
	}
	tx.ensureReadVersion()
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
	// Same entry-point precedence as its sibling GetEstimatedRangeSizeBytes
	// (client/metrics.go:177-187): inverted_range(2005), then transaction_cancelled(1025), gated
	// explicitly because this path fetches no read version either.
	begin, end := r.FDBRangeKeys()
	if bytes.Compare([]byte(begin.FDBKey()), []byte(end.FDBKey())) > 0 {
		return newReadyKeyArray(nil, fdb.Error{Code: 2005}) // inverted_range
	}
	if tx.cancelled {
		return newReadyKeyArray(nil, fdb.Error{Code: 1025})
	}
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

// AddReadConflictRange records an EXPLICIT read conflict. It is filtered through the write map
// exactly like an implicit one: C++ addReadConflictRange runs updateConflictMap
// (ReadYourWrites.actor.cpp:1986, ported at client/transaction.go:3140), because a segment the
// transaction already wrote independently was satisfied with no database read whether the
// caller asked for the conflict or not.
func (tx *simTxn) AddReadConflictRange(er fdb.ExactRange) error {
	b, e := er.FDBRangeKeys()
	begin, end, ok, err := clampConflictRange([]byte(b.FDBKey()), []byte(e.FDBKey()))
	if err != nil || !ok {
		return err
	}
	tx.addFilteredReadConflict(begin, end)
	return nil
}

func (tx *simTxn) AddReadConflictKey(key fdb.KeyConvertible) error {
	tx.addFilteredReadConflictKey([]byte(key.FDBKey()))
	return nil
}

// AddWriteConflictRange records an EXPLICIT write conflict, validated exactly as the client
// validates it (client/transaction.go:3153-3176).
//
// An UNVALIDATED inverted range is not inert here — it is actively harmful. rangesOverlap
// (conflict.go) is the plain half-open predicate `a.begin < b.end && b.begin < a.end`, which an
// inverted [hi, lo) satisfies against any read range that STRADDLES it: an explicit write
// conflict of ["n","c") "overlaps" a reader's ["a","z") and aborts it with not_committed(1020).
// So the sim answered a caller error with a spurious conflict verdict — in the harness whose one
// job is to certify conflict verdicts.
func (tx *simTxn) AddWriteConflictRange(er fdb.ExactRange) error {
	b, e := er.FDBRangeKeys()
	begin, end, ok, err := clampConflictRange([]byte(b.FDBKey()), []byte(e.FDBKey()))
	if err != nil || !ok {
		return err
	}
	tx.addWriteConflict(begin, end)
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
	if tx.committed && len(tx.buffer) == 0 {
		// Idempotent no-op: real FDB resets the transaction to active+empty after a
		// successful commit (client postCommitReset), so committing again with nothing new
		// buffered succeeds as a no-op. The record layer relies on this — the SQL DDL path
		// commits inside a db.Run closure (ddl.go:829 txn.Commit()), then Run's Transact
		// commits the same transaction again.
		return newReadyNil(nil)
	}
	if tx.committed {
		// Re-commit newly-buffered mutations. The read version and conflict ranges were
		// already cleared by postCommitReset when the previous commit succeeded, so this
		// commit resolves as the fresh logical transaction it is.
		tx.committed = false
	}
	if err := tx.db.commit(tx); err != nil {
		return newReadyNil(err)
	}
	return newReadyNil(nil)
}

// postCommitReset returns a successfully-committed handle to the state a NEW logical
// transaction starts in, mirroring the client's Transaction.postCommitReset
// (client/transaction.go:3198-3213). Both the read version AND the three buffers go:
// clearing the buffers alone leaves the handle reading at its pre-commit snapshot, which
// breaks read-your-writes across the commit boundary AND makes the handle's next commit
// conflict against its own previous one (its commit version is greater than the stale read
// version, and its read-conflict ranges are still attached).
//
// committedVersion and versionstamp deliberately SURVIVE: GetCommittedVersion and
// GetVersionstamp are defined on a committed handle, and the client keeps them for the
// same reason.
func (tx *simTxn) postCommitReset() {
	tx.readVersion = 0
	tx.rvSet = false
	tx.buffer = nil
	tx.readConflicts = nil
	tx.writeConflicts = nil
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

// maybeCommitted reports whether code is in C++'s FDB_ERROR_PREDICATE_MAYBE_COMMITTED set —
// the errors after which the transaction's mutations MAY already be durable. Same two codes
// the client's OnError special-cases (client/transaction.go:2382, ErrCommitUnknownResult /
// ErrClusterVersionChanged).
func maybeCommitted(code int) bool {
	return code == 1021 || code == 1039
}

// OnError classifies e: for a retryable code it resets the transaction (fresh read version,
// dropped buffer) and resolves success so the runner retries; otherwise it resolves the error.
// Matches FDB Transaction.onError.
//
// A MAYBE_COMMITTED error additionally makes the retry SELF-CONFLICTING: the write conflict
// ranges of the attempt that may have landed are carried over as READ conflict ranges, so if
// that commit did land, the retry sees its own write as a concurrent one and aborts 1020
// instead of applying the mutations a second time. This is the client's behaviour
// (client/transaction.go:2382-2400) and it is the only thing that makes a non-idempotent
// mutation — an Add — survive a 1021 retry with the right value.
//
// The sim is synchronous, so the specific interleaving the client guards against — a commit
// still in flight at the retry's GRV — cannot arise here. The port is load-bearing anyway,
// because the promoted ranges are ORDINARY read conflicts and are resolved as such: a
// write-only transaction (an atomic Add takes no read conflict of its own) that is retried
// after a maybe-committed error carries a read conflict on the keys it wrote, so a concurrent
// writer landing after the retry's read version aborts it. Without the promotion that retry
// has an empty read-conflict set and overwrites the concurrent write — a lost update SimFDB
// would certify and a real client would not.
func (tx *simTxn) OnError(e fdb.Error) fdb.FutureNil {
	if !fdb.IsOnErrorRetryable(e.Code) {
		return newReadyNil(e)
	}
	if !maybeCommitted(e.Code) {
		tx.Reset()
		return newReadyNil(nil)
	}
	selfConflicts := make([]keyRange, len(tx.writeConflicts))
	for i, wr := range tx.writeConflicts {
		selfConflicts[i] = keyRange{
			begin: append([]byte(nil), wr.begin...),
			end:   append([]byte(nil), wr.end...),
		}
	}
	tx.Reset()
	tx.readConflicts = append(tx.readConflicts, selfConflicts...)
	return newReadyNil(nil)
}

func (tx *simTxn) SetReadVersion(version int64) {
	tx.readVersion = version
	tx.rvSet = true
}

// ---- post-commit ----------------------------------------------------------------------

func (tx *simTxn) GetCommittedVersion() (int64, error) {
	if !tx.committed {
		return -1, fdb.Error{Code: 2017}
	}
	return tx.committedVersion, nil
}

// GetVersionstamp returns a LAZY future: the record layer obtains it BEFORE commit
// (database.go:372) and resolves it AFTER commit, so it must read the versionstamp at Get()
// time, not at construction. A ready future capturing the (nil) pre-commit stamp would surface
// as used_during_commit(2017) on the metadata-version-stamp path.
func (tx *simTxn) GetVersionstamp() fdb.FutureKey {
	return &lazyVersionstamp{tx: tx}
}

type lazyVersionstamp struct{ tx *simTxn }

func (f *lazyVersionstamp) Get() (fdb.Key, error) {
	if !f.tx.committed || f.tx.versionstamp == nil {
		return nil, fdb.Error{Code: 2017}
	}
	return append(fdb.Key(nil), f.tx.versionstamp...), nil
}

func (f *lazyVersionstamp) MustGet() fdb.Key {
	k, err := f.Get()
	if err != nil {
		panic(err)
	}
	return k
}

func (f *lazyVersionstamp) BlockUntilReady() {}
func (f *lazyVersionstamp) IsReady() bool    { return f.tx.committed }
func (f *lazyVersionstamp) Cancel()          {}

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
