package client

import (
	"bytes"
	"encoding/binary"

	"fdb.dev/internal/fdbwire"
)

// Mutation represents a key-value mutation in a transaction.
type Mutation struct {
	Type  MutationType
	Key   []byte
	Value []byte
}

// MutationType is the type of mutation.
type MutationType uint8

// Mutation types — MUST match C++ MutationRef::Type enum values exactly.
// Wire format uses these values directly. See CommitTransaction.h.
const (
	MutSetValue               MutationType = 0
	MutClearRange             MutationType = 1
	MutAddValue               MutationType = 2
	MutAnd                    MutationType = 6  // C++: And (skips DebugKeyRange=3, DebugKey=4, NoOp=5)
	MutOr                     MutationType = 7  // C++: Or
	MutXor                    MutationType = 8  // C++: Xor
	MutAppendIfFits           MutationType = 9  // C++: AppendIfFits
	MutMax                    MutationType = 12 // C++: Max (skips AvailableForReuse=10, Reserved=11)
	MutMin                    MutationType = 13 // C++: Min
	MutSetVersionstampedKey   MutationType = 14 // C++: SetVersionstampedKey
	MutSetVersionstampedValue MutationType = 15 // C++: SetVersionstampedValue
	MutByteMin                MutationType = 16 // C++: ByteMin
	MutByteMax                MutationType = 17 // C++: ByteMax
	MutMinV2                  MutationType = 18 // C++: MinV2
	MutAndV2                  MutationType = 19 // C++: AndV2
	MutCompareAndClear        MutationType = 20 // C++: CompareAndClear
)

// atomicOpMask mirrors C++ MutationRef::ATOMIC_MASK (CommitTransaction.h:570-572): the bitset of
// op-codes that are valid atomic operations. Any op outside it (SetValue=0, ClearRange=1, the
// reserved/debug codes 3,4,5,10,11, or >=21) is NOT a valid argument to atomicOp.
const atomicOpMask uint32 = (1 << MutAddValue) | (1 << MutAnd) | (1 << MutOr) | (1 << MutXor) |
	(1 << MutAppendIfFits) | (1 << MutMax) | (1 << MutMin) | (1 << MutSetVersionstampedKey) |
	(1 << MutSetVersionstampedValue) | (1 << MutByteMin) | (1 << MutByteMax) | (1 << MutMinV2) |
	(1 << MutAndV2) | (1 << MutCompareAndClear)

// isAtomicOp reports whether op is a valid atomic operation — C++ isValidMutationType &&
// isAtomicOp (CommitTransaction.h:603-611). op-codes >= 32 cannot be in the mask (a non-atomic,
// out-of-range type). atomicOp() rejects anything this returns false for with invalid_mutation_type.
func isAtomicOp(op MutationType) bool {
	return op < 32 && atomicOpMask&(1<<op) != 0
}

// maxWriteKey returns the maximum writable key for this transaction.
// The result is shared and read-only: callers only compare against it.
func (tx *Transaction) maxWriteKey() []byte {
	if tx.writeSystemKeys {
		return maxWriteKeySystem
	}
	return maxWriteKeyUser
}

// maxWriteKey's bounds, never written.
var (
	maxWriteKeySystem = []byte{0xff, 0xff}
	maxWriteKeyUser   = []byte{0xff}
)

// metadataVersionKeyBytes is \xff/metadataVersion — exempt from system key checks.
// C++ RYW: `key != metadataVersionKey` in getValue check.
var metadataVersionKeyBytes = []byte("\xff/metadataVersion")

// metadataVersionKeyEndBytes is \xff/metadataVersion\x00 (SystemData.cpp:1386) — the end of the
// metadataVersionKey range exempted from addReadConflictRange's maxReadKey check (ReadYourWrites.actor.cpp:1955).
var metadataVersionKeyEndBytes = []byte("\xff/metadataVersion\x00")

// metadataVersionRequiredValue is the ONLY operand libfdb_c accepts for a
// SetVersionstampedValue to metadataVersionKey (SystemData.cpp:1387 — 14 zero bytes: a
// 10-byte versionstamp placeholder + the 4-byte LE offset suffix 0). C++ RYW::atomicOp
// (:2226-2229) rejects any other operand — and any non-SVV op — with client_invalid_operation.
var metadataVersionRequiredValue = []byte("\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00")

// ownMutationLocked takes ownership before the caller can reuse its buffers,
// matching ThreadSafeTransaction::set/clear/atomicOp. Caller holds conflictMu.
func (tx *Transaction) ownMutationLocked(typ MutationType, key, value []byte) Mutation {
	if len(key)+len(value) == 0 {
		return Mutation{Type: typ, Key: bytes.Clone(key), Value: bytes.Clone(value)}
	}
	buf := tx.conflictBufAlloc(len(key) + len(value))
	var ownedKey, ownedValue []byte
	if key != nil {
		ownedKey = buf[:len(key):len(key)]
		copy(ownedKey, key)
	}
	if value != nil {
		ownedValue = buf[len(key):len(buf):len(buf)]
		copy(ownedValue, value)
	}
	return Mutation{Type: typ, Key: ownedKey, Value: ownedValue}
}

// ownPointMutationLocked is ownMutationLocked for a single-key operation, with
// keyAfter(key) carved from the same bytes: key, a zero byte, then value. The
// key's write-conflict range [key, keyAfter(key)) shares them. Caller holds conflictMu.
func (tx *Transaction) ownPointMutationLocked(typ MutationType, key, value []byte) (Mutation, []byte) {
	n := len(key)
	buf := tx.conflictBufAlloc(n + 1 + len(value))
	copy(buf, key)
	buf[n] = 0 // explicit zero — pooled buffer may have stale data
	m := Mutation{Type: typ}
	if key != nil {
		m.Key = buf[:n:n]
	}
	if value != nil {
		m.Value = buf[n+1 : len(buf) : len(buf)]
		copy(m.Value, value)
	}
	return m, buf[: n+1 : n+1]
}

func (tx *Transaction) stateSet(key, value []byte) {
	// The mutation and its write-conflict range must become visible to a
	// concurrent Commit snapshot as ONE atomic unit — otherwise the snapshot
	// could ship the mutation without its conflict range, so a concurrent
	// transaction that read the key would not be conflicted (a missed conflict,
	// not just a spurious one). Hold conflictMu across both appends.
	// The write map, which holds the write conflict with RYW, is updated under the
	// same lock (conflict_map.go).
	tx.conflictMu.Lock()
	defer tx.conflictMu.Unlock()
	if tx.rywDisabled {
		m, after := tx.ownPointMutationLocked(MutSetValue, key, value)
		tx.appendMutationLocked(m)
		tx.writeConflictLocked(after[:len(key):len(key)], after, true)
		return
	}
	// The op-log shares the bytes the write map keeps: one copy, as C++ RYW
	// keeps one in its arena (ReadYourWrites.actor.cpp:2337-2353).
	k, v := tx.ryw.setWith(key, value, tx.writeConflictPointLocked(key))
	tx.appendMutationLocked(Mutation{Type: MutSetValue, Key: nilIfNil(key, k), Value: nilIfNil(value, v)})
}

// nilIfNil is kept, or nil when the caller passed nil: the op-log keeps the
// caller's nil, as ownMutationLocked does.
func nilIfNil(caller, kept []byte) []byte {
	if caller == nil {
		return nil
	}
	return kept
}

// consumeNextWriteNoConflict resets the one-shot NEXT_WRITE_NO_WRITE_CONFLICT_RANGE
// flag without adding a conflict range. C++ RYW set/clear/atomicOp consume the flag
// (getAndResetWriteConflictDisabled) at the TOP, before any size-based no-op return
// (ReadYourWrites.actor.cpp:2407 for clear), so an oversized/empty clear still
// consumes it and it never leaks to the following write.
func (tx *Transaction) consumeNextWriteNoConflict() {
	tx.conflictMu.Lock()
	tx.nextWriteNoConflict = false
	tx.conflictMu.Unlock()
}

func (tx *Transaction) stateClear(key []byte) {
	// C++ clear(KeyRef) drops an oversized single-key clear entirely
	// (NativeAPI.actor.cpp:6045-6047): no mutation, no conflict range, no RYW write —
	// but the no-conflict flag is still consumed (RYW :2407, above the size check).
	// RYW clear() checks the legal range before the size drop (ReadYourWrites.actor.cpp:2419-2424): an
	// out-of-range key, even an oversized one, falls through and becomes the deferred 2004.
	legal := bytes.Compare(key, tx.maxWriteKey()) < 0 || bytes.Equal(key, metadataVersionKeyBytes)
	if legal && len(key) > getMaxClearKeySize(key) {
		tx.consumeNextWriteNoConflict()
		return
	}
	tx.conflictMu.Lock()
	defer tx.conflictMu.Unlock()
	if !tx.rywDisabled {
		// The op-log shares the key\x00 the write map keeps (RYW:2426-2435).
		end := tx.ryw.clearWith(key, tx.writeConflictPointLocked(key))
		tx.appendMutationLocked(Mutation{Type: MutClearRange, Key: nilIfNil(key, end[:len(key):len(key)]), Value: end})
		tx.singleKeyClearCount++ // C++ charges this mutation sizeof(KeyRangeRef), not MutationRef (RYW:2431)
		return
	}
	// One owned copy of key\x00 serves the mutation's begin and end and the conflict range.
	m, end := tx.ownPointMutationLocked(MutClearRange, key, nil)
	m.Value = end
	tx.appendMutationLocked(m)
	tx.singleKeyClearCount++ // C++ charges this mutation sizeof(KeyRangeRef), not MutationRef (RYW:2431)
	tx.writeConflictLocked(end[:len(key):len(key)], end, true)
}

func (tx *Transaction) stateClearRange(begin, end []byte) error {
	if bytes.Compare(begin, end) > 0 {
		// libfdb_c throws inverted_range inside the network-thread lambda, so it is
		// also the deferred error (ThreadSafeTransaction.cpp:626-640).
		err := &wire.FDBError{Code: ErrInvertedRange}
		tx.recordDeferredError(err)
		return err
	}
	// RYW clear(range) checks the legal range before its empty return (ReadYourWrites.actor.cpp:2358,
	// 2370-2372, 2396): consume the flag, defer the error, buffer nothing.
	if maxKey := tx.maxWriteKey(); bytes.Compare(begin, maxKey) > 0 || bytes.Compare(end, maxKey) > 0 {
		tx.consumeNextWriteNoConflict()
		tx.recordDeferredError(&wire.FDBError{Code: 2004}) // key_outside_legal_range
		return nil
	}
	// C++ clear(KeyRangeRef) clamps oversized range keys to maxSize+1 bytes rather
	// than rejecting (NativeAPI.actor.cpp:6019-6028) — there are no stored keys
	// larger than the max, so a too-large bound is equivalent to its truncation.
	if bmax := getMaxClearKeySize(begin); len(begin) > bmax {
		begin = begin[:bmax+1]
	}
	if emax := getMaxClearKeySize(end); len(end) > emax {
		end = end[:emax+1]
	}
	// Zero-width (begin == end) or clamped-to-empty (begin >= end): C++ returns
	// without recording a mutation (r.empty()), but still consumes the no-conflict
	// flag (consumed at the top of RYW clear, above the empty check).
	if bytes.Compare(begin, end) >= 0 {
		tx.consumeNextWriteNoConflict()
		return nil
	}
	tx.conflictMu.Lock()
	defer tx.conflictMu.Unlock()
	if !tx.rywDisabled {
		// The op-log shares the bounds the write map keeps (RYW:2395-2402).
		addConflict := tx.writeConflictLocked(begin, end, true) // charges only, with RYW
		b, e := tx.ryw.clearRangeWith(begin, end, addConflict)
		tx.appendMutationLocked(Mutation{Type: MutClearRange, Key: nilIfNil(begin, b), Value: nilIfNil(end, e)})
		return nil
	}
	m := tx.ownMutationLocked(MutClearRange, begin, end)
	tx.appendMutationLocked(m)
	tx.writeConflictLocked(m.Key, m.Value, true)
	return nil
}

func (tx *Transaction) stateAtomic(op MutationType, key, operand []byte) {
	op, key, operand, ok := tx.prepareAtomicOp(op, key, operand)
	if !ok {
		return
	}
	// SetVersionstampedKey: commit the key TRANSFORMED with the cached-read-version min-bound stamp at
	// the placeholder, matching libfdb_c/Java. C++ atomicOp captures getCachedReadVersion().orDefault(0)
	// and mutates k in place (ReadYourWrites.actor.cpp:2276), inserts THAT key into the write map (:2295),
	// and the commit flush ships the write-map key to the proxy (:2059). The commit proxy overwrites
	// [pos,pos+10) with the ASSIGNED stamp and strips the 4-byte offset, so the STORED record is
	// byte-identical whether the placeholder held zeros or the min-bound stamp — but the commit-REQUEST
	// wire bytes now match the reference clients (the same key also feeds the RYW read model below). A
	// malformed key (no room for the offset/stamp) is buffered as-is; client_invalid_operation is
	// deferred at the call (deferIllegalLocked), as C++ throws it there.
	mutKey := key
	var svkUnreadBegin, svkUnreadEnd []byte
	svkTransformed := false
	// Only transform a raw key that is in the legal WRITE range. C++ getVersionstampKeyRange rejects an
	// out-of-legal-range key BEFORE transforming (ReadYourWrites.actor.cpp:2266); leaving an out-of-range
	// raw key un-transformed keeps validateMutation (at the call) checking the RAW key, so a \xff
	// placeholder at offset 0 (a raw system key on a non-system txn) still reports key_outside_legal_range
	// (2004). Transforming it first would hide the leading \xff behind the read-version/zero stamp and
	// bypass the check. For a placeholder at offset > 0 the leading byte is unchanged by the
	// transform, so the raw/transformed legal-range status is identical; the guard only matters at offset 0.
	if op == MutSetVersionstampedKey && bytes.Compare(key, tx.maxWriteKey()) < 0 {
		minVersion := int64(0)
		tx.readVersionMu.Lock()
		if tx.hasReadVersion {
			minVersion = tx.readVersion // C++ tr.getCachedReadVersion().orDefault(0)
		}
		tx.readVersionMu.Unlock()
		if rb, re, transformed, ok := versionstampKeyRange(key, minVersion, tx.maxReadKey()); ok {
			mutKey, svkUnreadBegin, svkUnreadEnd, svkTransformed = transformed, rb, re, true
		}
	}
	tx.applyAtomicMutation(op, key, mutKey, operand, svkUnreadBegin, svkUnreadEnd, svkTransformed)
}

func (tx *Transaction) prepareAtomicOp(op MutationType, key, operand []byte) (MutationType, []byte, []byte, bool) {
	// Min→MinV2 / And→AndV2 op-code upgrade (C++ RYW::atomicOp,
	// ReadYourWrites.actor.cpp:2243-2248): apiVersionAtLeast(510) upgrades the
	// legacy codes to their V2 variants, which fold correctly on an absent key.
	// Done here (the RYW::atomicOp analog) not in the fdb facade, so every caller
	// — including cmd/fdb-stacktester — gets it, matching libfdb_c.
	if tx.db != nil && tx.db.apiVersionAtLeast(510) {
		switch op {
		case MutMin:
			op = MutMinV2
		case MutAnd:
			op = MutAndV2
		}
	}
	// C++ ReadYourWritesTransaction::atomicOp (ReadYourWrites.actor.cpp:2235) rejects any op that is
	// not isValidMutationType && isAtomicOp with invalid_mutation_type, BEFORE adding it to the write
	// map. In libfdb_c that throw happens on the NETWORK thread inside the onMainThreadVoid functor
	// (ThreadSafeTransaction.cpp:601-609) and is captured into the transaction's single deferredError
	// slot (doOnMainThreadVoid, ThreadHelper.actor.h:44-55) — fdb_c.cpp's CATCH_AND_DIE never fires
	// (enqueueing cannot throw). Every subsequent read AND the commit then re-throw it via
	// checkDeferredError. Go matches exactly: record into deferredErr (surfaced by the per-op gates)
	// and — critically — do NOT buffer the mutation, so a misused op-code (e.g.
	// Atomic(MutClearRange,...), indistinguishable from a real Clear at commit time) can never reach
	// the shared cluster. The eager record matches "first invalid op wins" call ordering.
	if !isAtomicOp(op) {
		// C++ atomicOp checks metadataVersionKey (2000) / legal-range (2004) BEFORE the op-validity
		// check (2018) — ReadYourWrites.actor.cpp:2226-2235. Surface the same precedence eagerly so
		// Atomic(invalidOp, systemKey) reports key_outside_legal_range and Atomic(invalidOp,
		// metadataVersionKey) reports client_invalid_operation — not 2018.
		var code int
		switch {
		case bytes.Equal(key, metadataVersionKeyBytes):
			code = 2000 // client_invalid_operation
		case bytes.Compare(key, tx.maxWriteKey()) >= 0:
			code = 2004 // key_outside_legal_range
		default:
			code = ErrInvalidMutationType // 2018
		}
		// C++ throws in the call (ReadYourWrites.actor.cpp:2226-2235); an earlier illegal write was
		// deferred at its own call, and the first deferred error is kept.
		tx.recordDeferredError(&wire.FDBError{Code: code})
		return 0, nil, nil, false
	}
	// API < 520 versionstamp offset zero-extension (C++ RYW::atomicOp, ReadYourWrites.actor.cpp:2250-2261).
	// Pre-520 the SetVersionstampedKey offset was a 2-byte suffix and SetVersionstampedValue carried no
	// user offset; libfdb_c widens both to the unified 4-byte offset the commit proxy reads (parsed as the
	// trailing int32, ReadYourWrites.actor.cpp:2196/2283 → Atomic.h:258-264) by appending \x00\x00 (key,
	// zero-extending the 2-byte offset) / \x00\x00\x00\x00 (value, a literal offset-0). The Go client
	// accepts apiVersion >= MinAPIVersion (510), so 510-519 is reachable; without this a Go app at that
	// version writes versionstamp bytes libfdb_c would not — a wire divergence. Done here (the RYW::atomicOp
	// analog) BEFORE the versionstampKeyRange transform + buffering below, so the suffixed key/operand flow
	// through identically to C++. apiVersionAtLeast(520) → no-op, so the modern path is byte-unchanged.
	// (Applied unconditionally per op-type like C++ 2251/2257; the metadataVersionKey SetVersionstampedValue
	// case is unreachable here since metadataVersion requires API >= 610.)
	if tx.db != nil && !tx.db.apiVersionAtLeast(520) {
		switch op {
		case MutSetVersionstampedKey:
			key = append(append(make([]byte, 0, len(key)+2), key...), 0, 0)
		case MutSetVersionstampedValue:
			operand = append(append(make([]byte, 0, len(operand)+4), operand...), 0, 0, 0, 0)
		}
	}
	return op, key, operand, true
}

func (tx *Transaction) applyAtomicMutation(op MutationType, key, mutKey, operand, svkUnreadBegin, svkUnreadEnd []byte, svkTransformed bool) {
	tx.conflictMu.Lock()
	var after []byte // keyAfter(key), sharing the mutation's key bytes
	if op == MutSetVersionstampedKey {
		tx.appendMutationLocked(tx.ownMutationLocked(op, mutKey, operand))
	} else {
		var m Mutation
		m, after = tx.ownPointMutationLocked(op, key, operand)
		tx.appendMutationLocked(m)
	}
	// Atomic ops add a write conflict range but NOT a read conflict range —
	// EXCEPT SetVersionstampedKey. Its key carries an incomplete versionstamp
	// (the 10-byte stamp is filled in server-side at commit), so a conflict range
	// over the placeholder bytes is meaningless and would spuriously conflict two
	// transactions that stamp "the same" logical key. C++ RYW atomicOp forces
	// AddConflictRange::False for SetVersionstampedKey (ReadYourWrites.actor.cpp:2268)
	// while STILL consuming the NEXT_WRITE_NO_WRITE_CONFLICT_RANGE flag (getAndReset
	// at :2220). Match both: consume the flag, skip the range.
	addConflict := false
	if op == MutSetVersionstampedKey {
		tx.nextWriteNoConflict = false
	} else {
		addConflict = tx.writeConflictLocked(after[:len(key):len(key)], after, true)
	}
	// The write map is updated under the same lock (conflict_map.go).
	defer tx.conflictMu.Unlock()
	if !tx.rywDisabled {
		if svkTransformed {
			// SVK is RANGE-unreadable in the RYW model (RFC-098; C++
			// ReadYourWrites.actor.cpp:2263-2277): the entire candidate stamp range
			// [key@minStamp, key@\xff…) becomes unreadable, and the pending entry is stored at the
			// min-bound-transformed key (mutKey) — the SAME key committed above, matching C++.
			// The conflicts of the writes Go keeps in the span move to the list
			// (charged there by GetApproximateSize instead).
			for _, kr := range tx.ryw.addUnreadableRangeKeeping(svkUnreadBegin, svkUnreadEnd) {
				tx.rywConflictSize -= int64(len(kr.Begin)+len(kr.End)) + sizeofKeyRangeRef
				tx.writeConflicts = append(tx.writeConflicts, kr)
			}
			tx.ryw.atomicWith(op, mutKey, operand, false)
			return
		}
		tx.ryw.atomicWith(op, key, operand, addConflict)
	}
}

// versionstampKeyRange ports C++ getVersionstampKeyRange + the in-place key
// transform (Atomic.h:258-300): key carries a trailing 4-byte LE offset of
// the 10-byte placeholder. Returns the candidate stamp range
// [key@stamp(minVersion,0), key@\xff×10 + \x00) clamped to maxKey, and the
// key transformed with the min-bound stamp (suffix preserved). ok=false on a
// malformed key (validated again by the commit path's eager checks).
func versionstampKeyRange(key []byte, minVersion int64, maxKey []byte) (begin, end, transformed []byte, ok bool) {
	if len(key) < 4 {
		return nil, nil, nil, false
	}
	pos := int(int32(binary.LittleEndian.Uint32(key[len(key)-4:])))
	// pos > len-14 (not pos+10 > len-4): the subtraction form can't overflow
	// for any int32 pos on a 32-bit int.
	if pos < 0 || pos > len(key)-4-10 {
		return nil, nil, nil, false
	}
	// begin = key[:len-4] with placeVersionstamp(minVersion, 0) at pos.
	begin = append([]byte(nil), key[:len(key)-4]...)
	placeVersionstamp(begin[pos:], minVersion, 0)
	// end = key[:len-3] with trailing byte 0x00 and \xff×10 at pos
	// (Atomic.h:277-284: substr(0, size-3) then last byte = 0).
	end = append([]byte(nil), key[:len(key)-3]...)
	end[len(end)-1] = 0x00
	for i := 0; i < 10; i++ {
		end[pos+i] = 0xff
	}
	if bytes.Compare(end, maxKey) > 0 {
		end = append([]byte(nil), maxKey...)
	}
	// transformed = full key (suffix INCLUDED) with the min-bound stamp.
	transformed = append([]byte(nil), key...)
	placeVersionstamp(transformed[pos:], minVersion, 0)
	return begin, end, transformed, true
}

// placeVersionstamp writes the 10-byte versionstamp: 8-byte BIG-endian
// version + 2-byte BIG-endian transaction number (Atomic.h:243-256).
func placeVersionstamp(dst []byte, version int64, txnNumber uint16) {
	binary.BigEndian.PutUint64(dst[:8], uint64(version))
	binary.BigEndian.PutUint16(dst[8:10], txnNumber)
}

// validateMutation returns the error C++ set/atomicOp/clear throw for m in the call, checked in
// libfdb_c's order, or nil. deferIllegalLocked records it at each write; Commit's loop is a backstop.
func (tx *Transaction) validateMutation(m Mutation, maxWrite []byte) error {
	if bytes.Equal(m.Key, metadataVersionKeyBytes) && m.Type != MutClearRange {
		// C++ RYW::atomicOp (:2226-2229) and set() (:2300): the ONLY legal mutation to
		// metadataVersionKey is SetVersionstampedValue with operand == metadataVersionRequiredValue.
		// A plain Set, any other atomic op (Add, SVK, …), or SVV with a different operand →
		// client_invalid_operation (2000), BEFORE the size/offset checks. The legal write is exempt
		// from the legal-range check (system key) but still subject to the size/versionstamp checks
		// below, which metadataVersionRequiredValue passes. clear()/clear(range) (C++ :2357, :2406)
		// have NO metadataVersionKey gate — a clear of metadataVersionKey falls to the legal-range
		// check (metadataVersionKey >= maxWriteKey → 2004). Hence the `!= MutClearRange` guard.
		if m.Type != MutSetVersionstampedValue || !bytes.Equal(m.Value, metadataVersionRequiredValue) {
			return &wire.FDBError{Code: 2000} // client_invalid_operation
		}
	} else if bytes.Compare(m.Key, maxWrite) >= 0 {
		return &wire.FDBError{Code: 2004} // key_outside_legal_range
	}
	if m.Type == MutClearRange {
		// stateClearRange owns clear(range)'s end check (RYW:2370-2372) and buffers no range past it;
		// clear sizes are clamped, not rejected, as C++ clear() translates them.
		return nil
	}
	// set()/atomicOp() reject oversized keys/values (RYW:2238-2241, 2345-2348); the throw becomes the
	// deferred error, in libfdb_c (ThreadSafeTransaction.cpp:611-617) and here.
	// hasRawAccess: C++ sets options.rawAccess for RAW_ACCESS/ACCESS_SYSTEM_KEYS/READ_SYSTEM_KEYS
	// (NativeAPI.actor.cpp:7159-7170); ACCESS==writeSystemKeys, READ==readSystemKeys, either raising
	// the non-system key limit by the tenant-prefix slack (KEY_SIZE_LIMIT+8). But that +8 slack is the
	// tenant-prefix allowance (getMaxWriteKeySize, NativeAPI:11630): when THIS client prepends the
	// prefix itself (tenantId >= 0), the user key must stay within KEY_SIZE_LIMIT, so the slack is
	// gated on the no-tenant case (C++ forbids raw-access options on tenant transactions for this).
	rawAccess := (tx.writeSystemKeys || tx.readSystemKeys) && tx.tenantId < 0
	// C++ checks key/operand size EAGERLY in atomicOp on the ORIGINAL user bytes (RYW:2237-2242), BEFORE
	// appending the API<520 versionstamp offset suffix (:2250-2261). Go checks the buffered mutation,
	// which already carries that client-added suffix (2B for SVK — the versionstampKeyRange
	// transform preserves it, transaction.go:1516; 4B for SVV) — so it must be discounted here, else a
	// boundary-sized legacy value is spuriously value_too_large. At apiVersion>=520 there is no
	// client-added suffix (nothing to discount); the user-provided offset there IS counted by C++, which
	// len(m.Key)/len(m.Value) as-is already matches.
	keyLen, valLen := len(m.Key), len(m.Value)
	if tx.db != nil && !tx.db.apiVersionAtLeast(520) {
		switch m.Type {
		case MutSetVersionstampedKey:
			keyLen -= 2
		case MutSetVersionstampedValue:
			valLen -= 4
		}
	}
	if keyLen > getMaxWriteKeySize(m.Key, rawAccess) {
		return &wire.FDBError{Code: 2102} // key_too_large
	}
	if valLen > valueSizeLimit {
		return &wire.FDBError{Code: 2103} // value_too_large
	}
	// Versionstamp offset validation → client_invalid_operation (2000), after the size checks (an
	// oversized versionstamp key/value reports 2102/2103), as atomicOp checks in the call.
	switch m.Type {
	case MutSetVersionstampedKey:
		if err := validateVersionstampOffset(m.Key); err != nil {
			return err
		}
	case MutSetVersionstampedValue:
		if err := validateVersionstampOffset(m.Value); err != nil {
			return err
		}
	}
	return nil
}

// validateVersionstampOffset checks that a SET_VERSIONSTAMPED_KEY or
// SET_VERSIONSTAMPED_VALUE operand has a valid 4-byte LE offset suffix
// and that the 10-byte versionstamp fits within the operand.
// Matches C++ ReadYourWritesTransaction::atomicOp validation.
func validateVersionstampOffset(data []byte) error {
	if len(data) < 4 {
		return &wire.FDBError{Code: 2000} // client_invalid_operation
	}
	offset := int32(binary.LittleEndian.Uint32(data[len(data)-4:]))
	bodyLen := int32(len(data) - 4) // length without the offset suffix
	if offset < 0 || offset+10 > bodyLen {
		return &wire.FDBError{Code: 2000} // client_invalid_operation
	}
	return nil
}

// appendMutationLocked appends m to the op-log; an illegal m becomes the deferred
// error, as C++ throws it in the call. Caller holds conflictMu.
func (tx *Transaction) appendMutationLocked(m Mutation) {
	tx.growMutationsLocked()
	tx.mutations = append(tx.mutations, m)
	tx.deferIllegalLocked(m)
}

// growMutationsLocked doubles a full op-log: append's 1.25x growth would copy a
// large one several times over. Caller holds conflictMu.
func (tx *Transaction) growMutationsLocked() {
	if len(tx.mutations) == cap(tx.mutations) {
		grown := make([]Mutation, len(tx.mutations), max(2*cap(tx.mutations), 64))
		copy(grown, tx.mutations)
		tx.mutations = grown
	}
}
