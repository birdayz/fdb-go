package simfdb

import (
	"bytes"
	"sort"
	"sync/atomic"
	"time"

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

// simTxn is SimFDB's WritableTransaction. Data operations are single-owner;
// only Cancel may run concurrently, as when a SQL statement's context expires.
// The db mutex guards the shared store/version counter, not transaction reads.
type simTxn struct {
	db *SimDB

	// viewRowsTouched counts store entries examined while building range views. It makes
	// the per-page cost observable: a page is supposed to cost the page, not the tail
	// still ahead of it, and no assertion on the returned rows can tell the two apart.
	// Read only by tests.
	viewRowsTouched int

	// wholeViewBuilds counts UNBOUNDED buildView calls — the ones that clone, map and
	// sort the entire keyspace regardless of what the caller asked for. viewRowsTouched
	// cannot stand in for it: that counter measures how much of a RANGE a bounded build
	// walked, and the whole-keyspace build is the case where no range bounded anything.
	// Read only by tests.
	wholeViewBuilds int

	deferredErr error // First void-operation error; cleared by Reset or a successful OnError retry.
	readVersion int64
	rvSet       bool
	// rvInstant is when the read version was pinned, on the ENV clock — the sim's
	// analog of the client's GRV stamp. It must come from env.Now() and never
	// time.Now(): a budget measured against it decides how much work a page does,
	// and a run whose page boundaries move with the host's speed is not a
	// deterministic replay. Zero until rvSet.
	rvInstant time.Time

	buffer         []mutation // RYW write buffer, in issue order
	readConflicts  []keyRange
	writeConflicts []keyRange
	knownReads     []keyRange // successful logical reads at this transaction's read version

	// SetNextWriteNoWriteConflictRange arms this for exactly the next write (FDB semantics).
	nextWriteNoConflict bool
	// SetReadYourWritesDisable — record layer rarely sets it; when true, reads skip the buffer.
	rywDisabled bool
	// SetWriteConflictsDisabled — when true, writes add no write conflict ranges at all.
	writeConflictsDisabled bool

	// unreadable is the set of keys carrying a pending versionstamped op. A read that REACHES
	// one throws accessed_unreadable(1036): the stamp is assigned by the cluster at commit, so
	// the value is not knowable client-side and there is nothing honest to return. SimFDB used
	// to hand back the unstamped operand as though it were a real value — a phantom present key
	// whose contents no real client could ever have observed, and which the same transaction
	// would then find changed after commit.
	//
	// STICKY, mirroring C++ WriteMap.cpp:97 (`is_unreadable = it.is_unreadable() || op is a
	// versionstamped op`, ported at client/ryw.go:483-488): a later plain Set on the key does NOT
	// clear it (client/ryw.go:225-229 — the stack-replacing SetValue fast path is gated on
	// !is_unreadable, so the Set is pushed and the flag survives). Only a clear removes it
	// (client/ryw.go:243-244, 270-271) — a cleared key is readable because you know it is empty.
	unreadable map[string]struct{}

	// unreadableRanges are the SVK CANDIDATE STAMP ranges: sorted, non-overlapping. A
	// SetVersionstampedKey does not just make one key unknowable — nobody knows WHERE the
	// stamped key will land, only that it lands somewhere in [template@minStamp, template@\xff×10),
	// so every position in that span is unknowable and a read reaching ANY of them throws 1036.
	// C++ writes.addUnmodifiedAndUnreadableRange(getVersionstampKeyRange(...))
	// (ReadYourWrites.actor.cpp:2271 over Atomic.h:268-287), ported at client/ryw.go:1699
	// (addUnreadableRangeLocked into rywCache.unreadableRanges).
	//
	// Marking only the exact template key — which is what this modelled before — UNDER-THROWS:
	// a Get of a different key inside the candidate range returned a value where libfdb_c raises
	// accessed_unreadable, so a record-layer path that reads near its own pending stamp would
	// pass under the sim and fail against a real cluster.
	unreadableRanges []keyRange

	// SetBypassUnreadable (FDB_TR_OPTION_BYPASS_UNREADABLE): reads of unreadable keys return the
	// write-map value with the versionstamp placeholder bytes AS WRITTEN instead of throwing
	// (client/ryw.go:55-60).
	bypassUnreadable bool

	// retryLimit / hasRetryLimit / retryCount are the retry_limit option (FDB_TR_OPTION_RETRY_LIMIT)
	// and the count of retries OnError has granted: OnError returns the input error once
	// retryCount >= retryLimit (except code 0 becomes 4100), as the client does (C++
	// Transaction::onError). The limit is a PERSISTENT option: it survives an OnError retry and
	// reverts to the database default (unlimited) on a user Reset, which also zeroes the count.
	retryLimit    int
	hasRetryLimit bool
	retryCount    int

	committed        bool
	committedVersion int64
	cancelled        atomic.Bool

	// versionstamp assigned at commit (10-byte tx version), for GetVersionstamp.
	versionstamp []byte

	opts fdb.TransactionOptions
}

var (
	_ fdb.WritableTransaction        = (*simTxn)(nil)
	_ fdb.ReadVersionInstantReporter = (*simTxn)(nil)
)

func (db *SimDB) newTxn() *simTxn {
	tx := &simTxn{db: db}
	tx.opts = &txnOptions{tx: tx}
	return tx
}

// ensureReadVersion lazily pins the read version the first time the transaction reads or commits
// (matching a lazy GRV). The version comes from db.latestVersion — the highest committed version
// OR the simulated clock's, whichever is further along — because that is what a real GRV returns:
// the cluster's version advances with time even while idle, so a transaction that starts after a
// long quiet period does not read at the version of the last write.
func (tx *simTxn) ensureReadVersion() {
	if tx.rvSet {
		return
	}
	tx.db.mu.Lock()
	tx.readVersion = tx.db.latestVersion()
	tx.db.mu.Unlock()
	tx.rvSet = true
	tx.rvInstant = tx.db.env.Now()
}

// ReadVersionInstant implements fdb.ReadVersionInstantReporter. ok is false before
// the lazy GRV has fired, so a caller cannot mistake "not yet pinned" for "pinned
// at the zero time" — the distinction the whole capability exists to make.
func (tx *simTxn) ReadVersionInstant() (time.Time, bool) {
	if !tx.rvSet || tx.rvInstant.IsZero() {
		return time.Time{}, false
	}
	return tx.rvInstant, true
}

// ---- unreadable (pending versionstamp) tracking -----------------------------------------

// markUnreadable records that a versionstamped op landed on key.
func (tx *simTxn) markUnreadable(key []byte) {
	if tx.unreadable == nil {
		tx.unreadable = make(map[string]struct{})
	}
	tx.unreadable[string(key)] = struct{}{}
}

// clearUnreadable makes key readable again. Called only from the clear paths: a cleared key is
// known to be empty, so there is nothing unknowable about it (client/ryw.go:243-244). The
// single-key clear also subtracts [key, key+\x00) from the candidate ranges, so clearing the one
// position inside an SVK span makes that position — and only it — readable.
func (tx *simTxn) clearUnreadable(key []byte) {
	delete(tx.unreadable, string(key))
	tx.subtractUnreadableRange(key, keyAfter(key))
}

// clearUnreadableRange makes every unreadable key in [begin, end) readable — the ClearRange half
// of the same rule (client/ryw.go:270-271, 280) — and subtracts the span from the SVK candidate
// ranges (client/ryw.go's subtractRangeList; C++ gets it free from the shared PTree, where
// clear() inserts readable entries over the span, WriteMap.cpp:195).
func (tx *simTxn) clearUnreadableRange(begin, end []byte) {
	for k := range tx.unreadable {
		if bytes.Compare([]byte(k), begin) >= 0 && bytes.Compare([]byte(k), end) < 0 {
			delete(tx.unreadable, k)
		}
	}
	tx.subtractUnreadableRange(begin, end)
}

// hasUnreadable reports whether the transaction carries any unreadable state at all — the
// fast-path guard every gate opens with (client/ryw.go:1853 tests BOTH populations; testing only
// the key set is how the range half stayed dead).
func (tx *simTxn) hasUnreadable() bool {
	return len(tx.unreadable) > 0 || len(tx.unreadableRanges) > 0
}

// addUnreadableRange merges [begin, end) into the sorted, non-overlapping candidate ranges.
// Port of client/ryw.go's addUnreadableRangeLocked (C++
// writes.addUnmodifiedAndUnreadableRange, ReadYourWrites.actor.cpp:2271).
func (tx *simTxn) addUnreadableRange(begin, end []byte) {
	if bytes.Compare(begin, end) >= 0 {
		return
	}
	n := len(tx.unreadableRanges)
	hiIdx := sort.Search(n, func(i int) bool {
		return bytes.Compare(tx.unreadableRanges[i].begin, end) > 0
	})
	loIdx := sort.Search(n, func(i int) bool {
		return bytes.Compare(tx.unreadableRanges[i].end, begin) >= 0
	})
	newBegin := append([]byte(nil), begin...)
	newEnd := append([]byte(nil), end...)
	for i := loIdx; i < hiIdx; i++ {
		if bytes.Compare(tx.unreadableRanges[i].begin, newBegin) < 0 {
			newBegin = tx.unreadableRanges[i].begin
		}
		if bytes.Compare(tx.unreadableRanges[i].end, newEnd) > 0 {
			newEnd = tx.unreadableRanges[i].end
		}
	}
	merged := append([]keyRange(nil), tx.unreadableRanges[:loIdx]...)
	merged = append(merged, keyRange{begin: newBegin, end: newEnd})
	merged = append(merged, tx.unreadableRanges[hiIdx:]...)
	tx.unreadableRanges = merged
}

// subtractUnreadableRange removes [begin, end) from the candidate ranges, splitting any range
// that straddles the span. Port of client/ryw.go's subtractRangeList.
func (tx *simTxn) subtractUnreadableRange(begin, end []byte) {
	if len(tx.unreadableRanges) == 0 || bytes.Compare(begin, end) >= 0 {
		return
	}
	out := make([]keyRange, 0, len(tx.unreadableRanges)+1)
	for _, r := range tx.unreadableRanges {
		if bytes.Compare(r.end, begin) <= 0 || bytes.Compare(r.begin, end) >= 0 {
			out = append(out, r) // no overlap
			continue
		}
		if bytes.Compare(r.begin, begin) < 0 {
			out = append(out, keyRange{begin: r.begin, end: append([]byte(nil), begin...)})
		}
		if bytes.Compare(r.end, end) > 0 {
			out = append(out, keyRange{begin: append([]byte(nil), end...), end: r.end})
		}
	}
	tx.unreadableRanges = out
}

// inUnreadableRange reports whether key falls inside a candidate stamp range
// (client/ryw.go's isUnreadableLocked).
func (tx *simTxn) inUnreadableRange(key []byte) bool {
	// First range with end > key; key is inside iff that range's begin <= key.
	i := sort.Search(len(tx.unreadableRanges), func(i int) bool {
		return bytes.Compare(tx.unreadableRanges[i].end, key) > 0
	})
	return i < len(tx.unreadableRanges) && bytes.Compare(tx.unreadableRanges[i].begin, key) <= 0
}

// firstUnreadableRangeIn returns the smallest position in [begin, end) covered by a candidate
// range, or nil. Port of client/ryw.go's firstUnreadableInLocked.
func (tx *simTxn) firstUnreadableRangeIn(begin, end []byte) []byte {
	i := sort.Search(len(tx.unreadableRanges), func(i int) bool {
		return bytes.Compare(tx.unreadableRanges[i].end, begin) > 0
	})
	if i < len(tx.unreadableRanges) && bytes.Compare(tx.unreadableRanges[i].begin, end) < 0 {
		// The intersection starts at max(range.begin, begin).
		if bytes.Compare(tx.unreadableRanges[i].begin, begin) > 0 {
			return tx.unreadableRanges[i].begin
		}
		return begin
	}
	return nil
}

// lastUnreadableRangeIn returns the exclusive END of the last candidate range intersecting
// [begin, end), or nil — the reverse-scan counterpart. Port of client/ryw.go's
// lastUnreadableInLocked.
func (tx *simTxn) lastUnreadableRangeIn(begin, end []byte) []byte {
	i := sort.Search(len(tx.unreadableRanges), func(i int) bool {
		return bytes.Compare(tx.unreadableRanges[i].begin, end) >= 0
	}) - 1
	if i < 0 || bytes.Compare(tx.unreadableRanges[i].end, begin) <= 0 {
		return nil
	}
	if bytes.Compare(tx.unreadableRanges[i].end, end) < 0 {
		return tx.unreadableRanges[i].end
	}
	return end
}

// isUnreadable reports whether a read of key must throw accessed_unreadable — either because the
// key itself carries a pending versionstamped op, or because it sits inside an SVK candidate
// stamp range whose winner could land exactly there.
func (tx *simTxn) isUnreadable(key []byte) bool {
	if tx.rywDisabled || tx.bypassUnreadable || !tx.hasUnreadable() {
		return false
	}
	if _, ok := tx.unreadable[string(key)]; ok {
		return true
	}
	return tx.inUnreadableRange(key)
}

// unreadableScanCap returns the position at which a scan of [begin, end) must stop because of a
// pending versionstamped op, or nil if there is none in the window. A port of the client's
// unreadableScanCapLocked (client/ryw.go:1805-1839):
//
//   - forward: the SMALLEST unreadable key in the window becomes the scan's exclusive END, so
//     the key itself is excluded.
//   - reverse: the LARGEST one, and its cap is keyAfter(k) because that becomes the scan's
//     INCLUSIVE begin — without the +\x00 the reverse scan would return the very key it must
//     not read. Only the largest matters: a reverse scan walks down from end, so the highest
//     unreadable key is the first one it can reach and everything below hides behind it.
//
// Both populations participate: the pending-op entry keys AND the SVK candidate stamp ranges.
func (tx *simTxn) unreadableScanCap(begin, end []byte, reverse bool) []byte {
	if tx.rywDisabled || tx.bypassUnreadable || !tx.hasUnreadable() {
		return nil
	}
	// The SVK candidate ranges cap the scan too, and they cap it EARLIER than any entry key: a
	// range's head holds no write-map key at all, so a walk that consulted only the entry set
	// would sail through the unknowable span and report rows for positions the stamp may occupy
	// (client/ryw.go:1858/1867 folds both populations into one cap for the same reason).
	// The two populations produce caps in the SAME coordinate — a scan bound — but reach it
	// differently: a range already carries an exclusive end, while an entry key needs the
	// keyAfter(+\x00) on the reverse path to fall below the window. Convert first, combine
	// second; combining raw positions would apply the +\x00 to a range end and let a reverse
	// scan read the range's last position.
	var entryPos []byte
	for k := range tx.unreadable {
		kb := []byte(k)
		if bytes.Compare(kb, begin) < 0 || bytes.Compare(kb, end) >= 0 {
			continue
		}
		switch {
		case entryPos == nil:
			entryPos = kb
		case reverse && bytes.Compare(kb, entryPos) > 0:
			entryPos = kb
		case !reverse && bytes.Compare(kb, entryPos) < 0:
			entryPos = kb
		}
	}
	if entryPos != nil && reverse {
		entryPos = keyAfter(entryPos)
	}
	var rangePos []byte
	if reverse {
		rangePos = tx.lastUnreadableRangeIn(begin, end)
	} else {
		rangePos = tx.firstUnreadableRangeIn(begin, end)
	}
	switch {
	case entryPos == nil:
		return rangePos
	case rangePos == nil:
		return entryPos
	case reverse:
		// Reverse: the scan walks down from end, so the HIGHER cap is the first thing it meets.
		if bytes.Compare(rangePos, entryPos) > 0 {
			return rangePos
		}
		return entryPos
	default:
		if bytes.Compare(rangePos, entryPos) < 0 {
			return rangePos
		}
		return entryPos
	}
}

func (tx *simTxn) GetReadVersion() fdb.FutureInt64 {
	if tx.deferredErr != nil {
		return newReadyInt64(0, tx.deferredErr)
	}
	// Explicit pins are returned unchanged; SimFDB validates age at commit.
	if tx.cancelled.Load() {
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
	// The client's precedence, verbatim (client/metrics.go getEstimatedRangeSizeBytesImpl): inverted_range(2005) FIRST —
	// libfdb_c's KeyRangeRef constructor throws before the metric op runs — and only then
	// transaction_cancelled(1025), which this path must gate EXPLICITLY because it never fetches a
	// read version and so never passes through ensureReadVersion's checkCancelled. Adding the
	// cancel gate without the 2005 above it would invert the order the client reports.
	if bytes.Compare(b, e) > 0 {
		return newReadyInt64(0, fdb.Error{Code: 2005}) // inverted_range
	}
	if tx.deferredErr != nil {
		return newReadyInt64(0, tx.deferredErr)
	}
	if tx.cancelled.Load() {
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
	// (client/metrics.go getRangeSplitPointsImpl): inverted_range(2005), then transaction_cancelled(1025), gated
	// explicitly because this path fetches no read version either.
	begin, end := r.FDBRangeKeys()
	if bytes.Compare([]byte(begin.FDBKey()), []byte(end.FDBKey())) > 0 {
		return newReadyKeyArray(nil, fdb.Error{Code: 2005}) // inverted_range
	}
	if tx.deferredErr != nil {
		return newReadyKeyArray(nil, tx.deferredErr)
	}
	if tx.cancelled.Load() {
		return newReadyKeyArray(nil, fdb.Error{Code: 1025})
	}
	// Single logical shard: no interior split points (begin/end only, per FDB when the range
	// is smaller than a shard). v1 returns the empty set of interior boundaries.
	return newReadyKeyArray(nil, nil)
}

func (tx *simTxn) Options() fdb.TransactionOptions { return tx.opts }

// A transaction is itself a Transactor (the interface embeds ReadTransactor via
// ReadTransaction, and WritableTransaction is used where a Transactor is expected in the
// record layer's nested-context paths): run fn against itself, no new transaction.
func (tx *simTxn) Transact(fn func(fdb.WritableTransaction) (any, error)) (any, error) {
	return fn(tx)
}

func (tx *simTxn) ReadTransact(fn func(fdb.ReadTransaction) (any, error)) (any, error) {
	return fn(tx)
}
