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
	// mvccWindow = MAX_WRITE_TRANSACTION_LIFE_VERSIONS = 5 * VERSIONS_PER_SECOND. A read
	// version older than (latestCommit - mvccWindow) yields transaction_too_old(1007). SimFDB
	// never actually GCs history (retaining it is strictly safer); the window is pure version
	// arithmetic here.
	mvccWindow = 5_000_000
)

// commit resolves tx under serializable snapshot isolation, applies its writes, and assigns a
// commit version. Serialized by db.mu so exactly one transaction commits at a time — any serial
// order is a valid batch order, so SimFDB needs none of FDB's intra-batch MiniConflictSet path
// (RFC-179 Tier 1 item 1). Returns an fdb.Error for a conflict/limit/too-old; nil on success.
func (db *SimDB) commit(tx *simTxn) error {
	tx.ensureReadVersion()

	db.mu.Lock()
	defer db.mu.Unlock()

	if db.closed {
		return fdb.Error{Code: 1025} // transaction_cancelled-ish: db gone
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
	// deterministically (RFC-179 Tier 1 item 7). Conflict/too-old fire BEFORE apply (nothing
	// committed); commit_unknown fires AFTER apply (the write is durable but the caller must
	// treat the outcome as ambiguous — the non-idempotent-Add surface).
	if db.env.Fault("simfdb.commit.conflict") {
		return fdb.Error{Code: 1020}
	}
	if db.env.Fault("simfdb.commit.too_old") {
		return fdb.Error{Code: 1007}
	}

	cv := db.lastVersion + 1
	wcr := db.applyMutations(tx, cv)
	db.recentWrites = append(db.recentWrites, committedWrites{version: cv, ranges: wcr})
	db.lastVersion = cv
	tx.committed = true
	tx.committedVersion = cv
	tx.versionstamp = versionstampBytes(cv, 0)

	if db.env.Fault("simfdb.commit.unknown") {
		return fdb.Error{Code: 1021} // commit_unknown_result — data applied, outcome "unknown"
	}
	return nil
}

// applyMutations applies tx's buffered writes to the store at commit version cv, in issue
// order, and returns the write conflict ranges to log for this commit. Atomics apply against
// the value already in the store at cv (including earlier same-commit writes), matching FDB's
// server-side application. Versionstamp mutations are stamped here (SimFDB plays the server
// role) and their write conflict range is re-added at the STAMPED key (the client suppressed
// the placeholder-key range via NextWriteNoWriteConflictRange — RFC-179 FDB-C-dev finding).
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
// here (RFC-179 versionstamp anatomy).
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
