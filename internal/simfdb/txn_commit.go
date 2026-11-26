package simfdb

import (
	"bytes"
	"encoding/binary"
	"time"

	fdb "fdb.dev/pkg/fdbgo/fdb"
)

// presentVal copies v as a PRESENT value: a nil operand becomes an empty, non-nil slice.
//
// nil is SimFDB's spelling of "absent" everywhere below this line — store.put(k, nil) writes a
// tombstone, resolveKey returns nil for a cleared key, buildView drops nil-valued entries. A
// Set operand must therefore never be allowed to carry that meaning: there is no such thing as
// "Set to absent" in FDB. The C API takes a (pointer, length) pair, so a nil operand is
// indistinguishable from a zero-length one and both store a PRESENT, zero-length value; the
// pure-Go client makes that explicit — rywCache.set copies with `make([]byte, len(value))`
// (client/ryw.go:223-224), which turns nil into non-nil empty, and rywEntry documents
// `value==nil ("Set to empty bytes", a present key)` as distinct from its `absent` flag
// (client/ryw.go:88-91).
//
// Without the normalization one Set(k, nil) produced three different answers: Get saw nil and
// reported absent, GetRange's view map held the key with a nil value and reported it PRESENT,
// and the commit wrote a tombstone so the key vanished from the store. Normalizing at the write
// boundary — rather than teaching each of the three readers about a fourth state — is what makes
// them agree, and it is what the client does.
func presentVal(v []byte) []byte {
	c := make([]byte, len(v))
	copy(c, v)
	return c
}

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
	// presentVal, not cloneVal: Set(k, nil) writes a present empty value, never a tombstone.
	tx.enqueue(mutation{kind: mutSet, key: []byte(key.FDBKey()), value: presentVal(value)})
}

func (tx *simTxn) Clear(key fdb.KeyConvertible) {
	k := []byte(key.FDBKey())
	// A clear is the ONE thing that makes a key readable again: the transaction knows it is
	// empty, so there is no pending stamp to be ignorant of (client/ryw.go:243-244).
	tx.clearUnreadable(k)
	tx.enqueue(mutation{kind: mutClear, key: k})
}

func (tx *simTxn) ClearRange(er fdb.ExactRange) {
	b, e := er.FDBRangeKeys()
	begin, end := []byte(b.FDBKey()), []byte(e.FDBKey())
	tx.clearUnreadableRange(begin, end)
	tx.enqueue(mutation{kind: mutClearRange, key: begin, end: end})
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

// SetVersionstampedKey / SetVersionstampedValue make their key UNREADABLE for the rest of the
// transaction: the cluster fills the stamp at commit, so no client can know the resulting bytes
// beforehand, and a read must say so rather than invent an answer (client/ryw.go:483-488).
// SetVersionstampedKey additionally marks the whole CANDIDATE STAMP RANGE unreadable and buffers
// the mutation at the key TRANSFORMED with the min-bound stamp — both halves of C++ RYW::atomicOp
// (ReadYourWrites.actor.cpp:2263-2277, over getVersionstampKeyRange in Atomic.h:268-300), ported
// at client/transaction.go:1553-1613. The transform is invisible after commit (the proxy
// overwrites [pos,pos+10) with the assigned stamp either way) but it decides WHERE the pending
// entry sits during the transaction, which is what the range head's boundaries are measured from.
func (tx *simTxn) SetVersionstampedKey(key fdb.KeyConvertible, param []byte) {
	k := []byte(key.FDBKey())
	// C++ captures getCachedReadVersion().orDefault(0): a transaction that has not yet pinned a
	// read version stamps from 0, so the candidate range starts at the all-zero stamp.
	minVersion := int64(0)
	if tx.rvSet {
		minVersion = tx.readVersion
	}
	// The legal-write-range guard is C++'s (:2266 rejects an out-of-legal-range key BEFORE
	// transforming). SimFDB models no system keyspace — every option that would widen it is a
	// no-op here — so the bound is the plain user-keyspace end, the same key endKeyMarker names.
	if bytes.Compare(k, []byte(endKeyMarker)) < 0 {
		if rb, re, transformed, ok := versionstampKeyRange(k, minVersion, []byte(endKeyMarker)); ok {
			k = transformed
			tx.addUnreadableRange(rb, re)
		}
	}
	tx.markUnreadable(k)
	tx.enqueue(mutation{kind: mutVersionstampedKey, key: k, value: cloneVal(param)})
}

// versionstampKeyRange ports C++ getVersionstampKeyRange plus the in-place key transform
// (Atomic.h:258-300), identically to client/transaction.go:1623-1655. `key` carries a trailing
// 4-byte little-endian offset naming the position of its 10-byte placeholder. Returns the
// candidate stamp range [key@stamp(minVersion,0), key@\xff×10 + \x00) clamped to maxKey, and the
// key transformed with the min-bound stamp — offset suffix PRESERVED, because the suffix is what
// the commit path reads to find the placeholder again. ok=false on a malformed key.
func versionstampKeyRange(key []byte, minVersion int64, maxKey []byte) (begin, end, transformed []byte, ok bool) {
	if len(key) < 4 {
		return nil, nil, nil, false
	}
	pos := int(int32(binary.LittleEndian.Uint32(key[len(key)-4:])))
	// pos > len-14 rather than pos+10 > len-4: the subtraction form cannot overflow for any
	// int32 pos on a 32-bit int.
	if pos < 0 || pos > len(key)-4-10 {
		return nil, nil, nil, false
	}
	begin = append([]byte(nil), key[:len(key)-4]...)
	placeVersionstamp(begin[pos:], minVersion, 0)
	// end = key[:len-3] with a trailing 0x00 and \xff×10 at pos (Atomic.h:277-284).
	end = append([]byte(nil), key[:len(key)-3]...)
	end[len(end)-1] = 0x00
	for i := 0; i < 10; i++ {
		end[pos+i] = 0xff
	}
	if bytes.Compare(end, maxKey) > 0 {
		end = append([]byte(nil), maxKey...)
	}
	transformed = append([]byte(nil), key...)
	placeVersionstamp(transformed[pos:], minVersion, 0)
	return begin, end, transformed, true
}

// placeVersionstamp writes the 10-byte versionstamp: 8-byte BIG-endian version followed by a
// 2-byte BIG-endian transaction number (Atomic.h:243-256).
func placeVersionstamp(dst []byte, version int64, txnNumber uint16) {
	binary.BigEndian.PutUint64(dst[:8], uint64(version))
	binary.BigEndian.PutUint16(dst[8:10], txnNumber)
}

func (tx *simTxn) SetVersionstampedValue(key fdb.KeyConvertible, param []byte) {
	k := []byte(key.FDBKey())
	tx.markUnreadable(k)
	tx.enqueue(mutation{kind: mutVersionstampedValue, key: k, value: cloneVal(param)})
}

// []byte fast-path overloads: delegate to the KeyConvertible forms (fdb.Key is a KeyConvertible).
func (tx *simTxn) SetBytes(key, value []byte)         { tx.Set(fdb.Key(key), value) }
func (tx *simTxn) ClearBytes(key []byte)              { tx.Clear(fdb.Key(key)) }
func (tx *simTxn) AddBytes(key, param []byte)         { tx.Add(fdb.Key(key), param) }
func (tx *simTxn) MaxBytes(key, param []byte)         { tx.Max(fdb.Key(key), param) }
func (tx *simTxn) MinBytes(key, param []byte)         { tx.Min(fdb.Key(key), param) }
func (tx *simTxn) AppendIfFitsBytes(key, p []byte)    { tx.AppendIfFits(fdb.Key(key), p) }
func (tx *simTxn) CompareAndClearBytes(key, p []byte) { tx.CompareAndClear(fdb.Key(key), p) }
func (tx *simTxn) AddReadConflictRangeBytes(b, e []byte) error {
	return tx.AddReadConflictRange(fdb.KeyRange{Begin: fdb.Key(b), End: fdb.Key(e)})
}

func (tx *simTxn) AddReadConflictKeyBytes(key []byte) error {
	return tx.AddReadConflictKey(fdb.Key(key))
}

func (tx *simTxn) AddWriteConflictRangeBytes(b, e []byte) error {
	return tx.AddWriteConflictRange(fdb.KeyRange{Begin: fdb.Key(b), End: fdb.Key(e)})
}

func (tx *simTxn) AddWriteConflictKeyBytes(key []byte) error {
	return tx.AddWriteConflictKey(fdb.Key(key))
}

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

// AddReadConflictKey is AddReadConflictRange over [k, keyAfter(k)); the clamp empties that range
// for a key past the size limit (client addReadConflictKeyRYW).
func (tx *simTxn) AddReadConflictKey(key fdb.KeyConvertible) error {
	k := []byte(key.FDBKey())
	if len(k) > getMaxClearKeySize(k) {
		return nil
	}
	tx.addFilteredReadConflictKey(k)
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

// AddWriteConflictKey is AddWriteConflictRange over [k, keyAfter(k)), clamp included.
func (tx *simTxn) AddWriteConflictKey(key fdb.KeyConvertible) error {
	k := []byte(key.FDBKey())
	if len(k) > getMaxClearKeySize(k) {
		return nil
	}
	tx.addWriteConflict(k, keyAfter(k))
	return nil
}

// ---- lifecycle ------------------------------------------------------------------------

func (tx *simTxn) Commit() fdb.FutureNil {
	if tx.deferredErr != nil {
		return newReadyNil(tx.deferredErr)
	}
	if tx.cancelled.Load() {
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
	tx.rvInstant = time.Time{}
	tx.buffer = nil
	tx.knownReads = nil
	tx.unreadable = nil // the stamps are resolved; the keys are readable again
	tx.unreadableRanges = nil
	tx.readConflicts = nil
	tx.writeConflicts = nil
}

func (tx *simTxn) Cancel() { tx.cancelled.Store(true) }

// Reset returns the transaction to a fresh state for reuse (matching FDB Transaction.reset):
// clears the buffer, conflict ranges, and read version; keeps the db handle.
func (tx *simTxn) Reset() {
	tx.resetState()
	tx.hasRetryLimit = false
	tx.retryLimit = 0
	tx.retryCount = 0
}

// resetState clears everything an OnError retry clears, keeping the persistent options.
func (tx *simTxn) resetState() {
	tx.deferredErr = nil
	tx.readVersion = 0
	tx.rvSet = false
	tx.rvInstant = time.Time{}
	tx.buffer = nil
	tx.knownReads = nil
	tx.unreadable = nil
	tx.unreadableRanges = nil
	tx.readConflicts = nil
	tx.writeConflicts = nil
	tx.nextWriteNoConflict = false
	tx.committed = false
	tx.committedVersion = 0
	tx.cancelled.Store(false)
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
// A failed OnError retires the handle until Reset, matching FDB API >=610.
func (tx *simTxn) OnError(e fdb.Error) fdb.FutureNil {
	// SimFDB models cancellation, not delivered transaction timeouts.
	if tx.cancelled.Load() {
		return newReadyNil(fdb.Error{Code: 1025})
	}
	if e.Code == 1031 {
		tx.Cancel()
		return newReadyNil(e)
	}
	if tx.hasRetryLimit && tx.retryCount >= tx.retryLimit {
		tx.Cancel()
		if e.Code == 0 {
			return newReadyNil(fdb.Error{Code: 4100})
		}
		return newReadyNil(e)
	}
	if e.Code == 0 {
		tx.Cancel()
		return newReadyNil(fdb.Error{Code: 2000})
	}
	if !fdb.IsOnErrorRetryable(e.Code) {
		tx.Cancel()
		return newReadyNil(e)
	}
	tx.retryCount++
	if !maybeCommitted(e.Code) {
		tx.resetState()
		return newReadyNil(nil)
	}
	selfConflicts := make([]keyRange, len(tx.writeConflicts))
	for i, wr := range tx.writeConflicts {
		selfConflicts[i] = keyRange{
			begin: append([]byte(nil), wr.begin...),
			end:   append([]byte(nil), wr.end...),
		}
	}
	tx.resetState()
	tx.readConflicts = append(tx.readConflicts, selfConflicts...)
	return newReadyNil(nil)
}

func (tx *simTxn) SetReadVersion(version int64) {
	if tx.deferredErr != nil {
		return
	}
	// GRV is synchronous here: there is no separate pending-GRV state.
	if tx.rvSet {
		tx.deferredErr = fdb.Error{Code: 2010}
		return
	}
	if version <= 0 {
		tx.deferredErr = fdb.Error{Code: 2011}
		return
	}
	tx.readVersion = version
	tx.rvSet = true
	// An accepted explicit pin has no locally observed MVCC anchor.
	tx.rvInstant = time.Time{}
}

// ---- post-commit ----------------------------------------------------------------------

func (tx *simTxn) GetCommittedVersion() (int64, error) {
	if !tx.committed {
		return -1, fdb.Error{Code: 2017}
	}
	return tx.committedVersion, nil
}

// GetVersionstamp captures deferred errors at issue; otherwise it reads the stamp at Get.
// The record layer obtains a clean future before commit and consumes it afterward.
func (tx *simTxn) GetVersionstamp() fdb.FutureKey {
	if tx.deferredErr != nil {
		return newReadyKey(nil, tx.deferredErr)
	}
	return &lazyVersionstamp{tx: tx}
}

type lazyVersionstamp struct{ tx *simTxn }

func (f *lazyVersionstamp) Get() (fdb.Key, error) {
	// transaction_cancelled(1025) out-ranks the not-yet-committed verdict, as in the client
	// (client/transaction.go:2217-2219: checkCancelled precedes the hasCommitted check).
	if f.tx.cancelled.Load() {
		return nil, fdb.Error{Code: 1025}
	}
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

// GetApproximateSize returns the transaction's size the way the client's RYW counter does
// (client/transaction.go:2510-2543): the commit accounting — every mutation charged
// sizeof(MutationRef), every read and write conflict range charged sizeof(KeyRangeRef) — with one
// correction. C++ models a single-key clear as a write-map RANGE entry and charges its MUTATION
// half sizeof(KeyRangeRef) rather than sizeof(MutationRef) (ReadYourWrites.actor.cpp:2431), so
// each one is refunded the difference. The commit-time 2101 gate deliberately does NOT apply that
// refund — in the native commit a single-key clear IS a ClearRange mutation charged the full 44 —
// which is why the two callers share commitSize but not this line.
//
// Deliberately NOT gated on Cancel(): C++ gates getApproximateSize on the deferred error and
// nothing else (ThreadSafeTransaction.cpp:715-721 — no resetPromise race), so a cancelled txn
// still reports its size. See TestCancelledTransactionApproximateSizeStillAnswers.
func (tx *simTxn) GetApproximateSize() fdb.FutureInt64 {
	if tx.deferredErr != nil {
		return newReadyInt64(0, tx.deferredErr)
	}
	var singleKeyClears int64
	for _, m := range tx.buffer {
		if m.kind == mutClear {
			singleKeyClears++
		}
	}
	return newReadyInt64(commitSize(tx)-singleKeyClears*(sizeofMutationRef-sizeofKeyRangeRef), nil)
}
