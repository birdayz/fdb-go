package simfdb

import (
	"bytes"
	"encoding/binary"

	fdb "fdb.dev/pkg/fdbgo/fdb"
)

// FDB limits and the MVCC window (versions), matched to the real cluster so the layer's
// retryable-vs-terminal classification fires where it would against real FDB.
const (
	valueSizeLimit = 100_000    // value_too_large (2103)
	keySizeLimit   = 10_000     // key_too_large (2102) — the ~10KB key limit
	txnSizeLimit   = 10_000_000 // transaction_too_large (2101)
	// systemKeySizeLimit / tenantPrefixSize complete getMaxClearKeySize (below). Mirrors
	// CLIENT_KNOBS->SYSTEM_KEY_SIZE_LIMIT and TenantAPI::PREFIX_SIZE (client/sizelimits.go:18,20).
	systemKeySizeLimit = 30_000
	tenantPrefixSize   = 8
	// mvccWindow = MAX_WRITE_TRANSACTION_LIFE_VERSIONS = 5 * VERSIONS_PER_SECOND. A read
	// version older than (latestCommit - mvccWindow) yields transaction_too_old(1007). SimFDB
	// never actually GCs history (retaining it is strictly safer); the window is pure version
	// arithmetic here.
	mvccWindow = 5_000_000
)

// getMaxClearKeySize is NativeAPI.actor.cpp's getMaxClearKeySize == getMaxKeySize ==
// getMaxWriteKeySize(key, hasRawAccess=true) — the bound an explicit conflict range's endpoints
// are CLAMPED to (client/sizelimits.go:31-45). System keys get the larger limit; everything else
// gets the key limit plus the tenant-prefix slack raw access allows.
func getMaxClearKeySize(key []byte) int {
	if len(key) > 0 && key[0] == 0xFF {
		return systemKeySizeLimit
	}
	return keySizeLimit + tenantPrefixSize
}

// clampConflictRange applies the shared validation both explicit conflict-range entry points
// perform, in the client's order (client/transaction.go:3103-3128 read, :3153-3176 write):
// inverted_range(2005) first, then the oversized-endpoint clamp, then the drop when the clamp
// collapses the range to empty. ok=false means the caller returns nil without recording anything.
//
// The 2004 (key_outside_legal_range) check that sits between the two in the client is
// deliberately NOT ported: it compares the endpoints against getMaxReadKey/getMaxWriteKey, which
// are functions of the ACCESS_SYSTEM_KEYS / READ_SYSTEM_KEYS / RAW_ACCESS options, and SimFDB
// accepts all three as no-ops (options.go). Porting the check without the access model behind it
// would invent a verdict rather than reproduce one.
func clampConflictRange(begin, end []byte) (nb, ne []byte, ok bool, err error) {
	// Inverted first: in libfdb_c the KeyRangeRef constructor throws before any other check runs,
	// so an inverted range is 2005 even when it would also fail a later check.
	if bytes.Compare(begin, end) > 0 {
		return nil, nil, false, fdb.Error{Code: 2005} // inverted_range
	}
	if bmax := getMaxClearKeySize(begin); len(begin) > bmax {
		begin = begin[:bmax+1]
	}
	if emax := getMaxClearKeySize(end); len(end) > emax {
		end = end[:emax+1]
	}
	if bytes.Compare(begin, end) >= 0 {
		return nil, nil, false, nil // C++ r.empty() → returns without recording a conflict range
	}
	return begin, end, true, nil
}

// commit resolves tx under serializable snapshot isolation, applies its writes, and assigns a
// commit version. Serialized by db.mu so exactly one transaction commits at a time — any serial
// order is a valid batch order, so SimFDB needs none of FDB's intra-batch MiniConflictSet path
// (RFC-199 Tier 1 item 1). Returns an fdb.Error for a conflict/limit/too-old; nil on success.
func (db *SimDB) commit(tx *simTxn) error {
	tx.ensureReadVersion()

	db.mu.Lock()
	defer db.mu.Unlock()

	if db.closed {
		return fdb.Error{Code: 1025} // transaction_cancelled-ish: db gone
	}

	// Read-only / empty commit: a transaction with no mutations and no write conflict ranges is
	// completed CLIENT-SIDE by real FDB with no commit-proxy round-trip (ReadYourWrites.actor.cpp
	// commit()'s read-only fast path), so it never conflicts, never ages out, and never returns
	// commit_unknown — and it takes no commit version (GetCommittedVersion returns -1). Injected
	// and BUGGIFY faults must NOT fire on it either. Short-circuit before any of that.
	if len(tx.buffer) == 0 && len(tx.writeConflicts) == 0 {
		tx.committed = true
		tx.committedVersion = -1
		// The client runs the FULL postCommitReset on this path too
		// (client/transaction.go:1810-1821). It matters here even though there are no
		// mutations: a read-only transaction still accumulates READ-conflict ranges, and
		// leaving them (plus the read version) on the handle makes the NEXT logical
		// transaction resolve against reads it never issued.
		tx.postCommitReset()
		return nil
	}

	// transaction_too_old(1007): a read version below the MVCC window. A distinct, earlier
	// verdict than not_committed — and it never fires for a write-only transaction (one with no
	// read conflict ranges), matching FDB (SkipList.cpp:837 gates on read_conflict_ranges.size()).
	if len(tx.readConflicts) > 0 && tx.readVersion < db.lastVersion-mvccWindow {
		return fdb.Error{Code: 1007}
	}

	// Size limits (terminal / non-retryable): checked before the conflict resolution, matching
	// the client rejecting an over-limit mutation/transaction.
	if err := checkSizeLimits(tx); err != nil {
		return err
	}

	// SSI: a read conflict range read at readVersion conflicts iff some transaction that
	// committed AFTER readVersion has an overlapping write conflict range. Strict inequality on
	// the commit version (cw.version > readVersion) — the transaction's own future commit
	// version is larger than its read version, so it never conflicts with itself.
	for _, cw := range db.recentWrites {
		if cw.version <= tx.readVersion {
			continue
		}
		for _, rc := range tx.readConflicts {
			for _, wr := range cw.ranges {
				if rangesOverlap(rc, wr) {
					return fdb.Error{Code: 1020} // not_committed
				}
			}
		}
	}

	// BUGGIFY: seed-chosen commit-time faults so retry/idempotency paths are exercised
	// deterministically (RFC-199 Tier 1 item 7). Conflict/too-old fire BEFORE apply (nothing
	// committed); commit_unknown is resolved after them, below.
	//
	// All three draws happen HERE, unconditionally, before the targeted-injection check and
	// before any branch that could skip one. Injection is a SIM TOOL, not part of the modelled
	// system, so it must not move the seeded schedule: with the draws behind an early return
	// (or behind a `cond || db.env.Fault(site)` short-circuit) a run with InjectOnce sees a
	// different fault schedule from the same seed without it, and a reproducer stops
	// reproducing as soon as anyone adds an injection to the harness. The seed is supposed to
	// determine the schedule by itself.
	faultConflict := db.env.Fault("simfdb.commit.conflict")
	faultTooOld := db.env.Fault("simfdb.commit.too_old")
	faultUnknown := db.env.Fault("simfdb.commit.unknown")

	// Targeted injection (InjectOnce/InjectSequence): a deterministic fault for this commit,
	// which overrides the seeded verdict. 1021 is resolved below (it has two branches);
	// everything else (1020/1007/1009/size codes) fires here, before any mutation applies.
	inject := db.takeInject()
	if inject != 0 && inject != 1021 && inject != injectCommitUnknownApplied && inject != injectCommitUnknownDiscarded {
		return fdb.Error{Code: inject}
	}

	if faultConflict {
		return fdb.Error{Code: 1020}
	}
	if faultTooOld {
		return fdb.Error{Code: 1007}
	}

	// commit_unknown_result(1021) has TWO real branches, and modelling only one certifies a
	// wrong verdict. Real FDB returns 1021 when the client cannot learn the commit proxy's
	// answer: the mutations may be durable, or the commit may never have reached the proxy at
	// all. A sim that always applies them lets a caller "verify" that its 1021 handling is
	// correct while never once exercising the case where the retry has to redo the work.
	//
	// The branch is a per-seed coin (a modelling choice, not a fault), so a run is reproducible
	// and a hunt over seeds reaches both. Tests that need a NAMED branch inject it explicitly.
	unknown := inject == 1021 || faultUnknown
	applied := true
	switch {
	case inject == injectCommitUnknownApplied:
		unknown, applied = true, true
	case inject == injectCommitUnknownDiscarded:
		unknown, applied = true, false
	case unknown:
		applied = db.env.Coin("simfdb.commit.unknown.applied")
	}

	if unknown {
		db.lastUnknownApplied = applied
	}

	if unknown && !applied {
		// The commit never reached the proxy: nothing is written, no version is consumed, and
		// — as on every error return — the handle is untouched.
		return fdb.Error{Code: 1021}
	}

	cv := db.lastVersion + 1
	wcr := db.applyMutations(tx, cv)
	db.recentWrites = append(db.recentWrites, committedWrites{version: cv, ranges: wcr})
	db.lastVersion = cv

	if unknown {
		// The mutations are durable but the OUTCOME IS UNKNOWN TO THE CLIENT, so the handle
		// must NOT be advanced into the committed state. The client only sets hasCommitted on
		// the success path (client/transaction.go:1908-1925) and runs postCommitReset there
		// too. Marking the handle committed here had three consequences, each of which lets
		// SimFDB certify a wrong verdict:
		//
		//   - GetCommittedVersion/GetVersionstamp succeed after 1021. On a real client they
		//     raise used_during_commit(2017), because there IS no known committed version.
		//   - postCommitReset nils the buffer, so a re-Commit() after 1021 hits the
		//     "committed && empty buffer" idempotent-no-op arm and returns SUCCESS having
		//     written nothing. An explicit-transaction COMMIT retry — the RFC-198 path — lands
		//     exactly there and silently loses the whole transaction.
		//   - The read version and conflict ranges are dropped, so a retry on the same handle
		//     resolves against reads it no longer remembers.
		//
		// Leaving the handle alone means a re-Commit re-sends the same buffer, which is what a
		// real retry does — and what makes the applied branch's DOUBLE APPLY observable, the
		// whole point of the non-idempotent-Add surface.
		return fdb.Error{Code: 1021}
	}

	tx.committed = true
	tx.committedVersion = cv
	tx.versionstamp = versionstampBytes(cv, 0)
	tx.postCommitReset()
	return nil
}

// applyMutations applies tx's buffered writes to the store at commit version cv, in issue
// order, and returns the write conflict ranges to log for this commit. Atomics apply against
// the value already in the store at cv (including earlier same-commit writes), matching FDB's
// server-side application. Versionstamp mutations are stamped here (SimFDB plays the server
// role) and their write conflict range is re-added at the STAMPED key (the client suppressed
// the placeholder-key range via NextWriteNoWriteConflictRange, so without the re-add a
// versionstamped write would conflict against nothing — RFC-199 §Tier 1).
func (db *SimDB) applyMutations(tx *simTxn, cv int64) []keyRange {
	stamp := versionstampBytes(cv, 0)
	wcr := append([]keyRange(nil), tx.writeConflicts...)
	for _, m := range tx.buffer {
		switch m.kind {
		case mutSet:
			db.store.put(m.key, m.value, cv)
		case mutClear:
			db.store.put(m.key, nil, cv)
		case mutClearRange:
			db.store.clearRange(m.key, m.end, cv)
		case mutAtomic:
			existing := db.store.valueAt(m.key, cv)
			nv, clear := applyAtomic(m.op, existing, m.value)
			if clear {
				db.store.put(m.key, nil, cv)
			} else {
				db.store.put(m.key, nv, cv)
			}
		case mutVersionstampedKey:
			stampedKey := stampVersionstamp(m.key, stamp)
			db.store.put(stampedKey, m.value, cv)
			// Server re-adds the write conflict range at the finished key.
			wcr = append(wcr, keyRange{begin: stampedKey, end: keyAfter(stampedKey)})
		case mutVersionstampedValue:
			db.store.put(m.key, stampVersionstamp(m.value, stamp), cv)
		}
	}
	return wcr
}

// checkSizeLimits enforces the terminal key/value/transaction size limits.
func checkSizeLimits(tx *simTxn) error {
	var total int64
	for _, m := range tx.buffer {
		if len(m.key) > keySizeLimit || len(m.end) > keySizeLimit {
			return fdb.Error{Code: 2102} // key_too_large
		}
		if len(m.value) > valueSizeLimit {
			return fdb.Error{Code: 2103} // value_too_large
		}
		total += int64(len(m.key) + len(m.end) + len(m.value))
	}
	if total > txnSizeLimit {
		return fdb.Error{Code: 2101} // transaction_too_large
	}
	return nil
}

// rangesOverlap reports whether two half-open [begin,end) ranges intersect.
func rangesOverlap(a, b keyRange) bool {
	return bytes.Compare(a.begin, b.end) < 0 && bytes.Compare(b.begin, a.end) < 0
}

// versionstampBytes builds the 10-byte transaction version: 8-byte big-endian commit version
// followed by the 2-byte big-endian batch order (0 for one-transaction-per-commit). The 2-byte
// user version that completes a 12-byte versionstamp is client tuple data and is never written
// here (RFC-199 versionstamp anatomy).
func versionstampBytes(commitVersion int64, batchOrder uint16) []byte {
	b := make([]byte, 10)
	binary.BigEndian.PutUint64(b[0:8], uint64(commitVersion))
	binary.BigEndian.PutUint16(b[8:10], batchOrder)
	return b
}

// stampVersionstamp overwrites the 10-byte 0xFF placeholder inside a versionstamped key or
// value with the transaction version, and strips the trailing 4-byte little-endian offset that
// marks the placeholder position (apiVersion >= 520 — the record layer runs at 730). Returns
// the finished bytes; a buffer too short to carry an offset is returned unchanged.
func stampVersionstamp(buf, stamp []byte) []byte {
	if len(buf) < 4 {
		return append([]byte(nil), buf...)
	}
	offset := binary.LittleEndian.Uint32(buf[len(buf)-4:])
	out := append([]byte(nil), buf[:len(buf)-4]...)
	if int(offset)+len(stamp) <= len(out) {
		copy(out[int(offset):int(offset)+len(stamp)], stamp)
	}
	return out
}
