package simfdb

import (
	"bytes"
	"encoding/binary"
	"time"

	"fdb.dev/pkg/dst"
	fdb "fdb.dev/pkg/fdbgo/fdb"
)

// FDB limits and the MVCC window (versions), matched to the real cluster so the layer's
// retryable-vs-terminal classification fires where it would against real FDB.
const (
	valueSizeLimit = 100_000    // value_too_large (2103)
	keySizeLimit   = 10_000     // key_too_large (2102) — the ~10KB key limit
	txnSizeLimit   = 10_000_000 // transaction_too_large (2101)

	// versionsPerSecond is FDB's SERVER_KNOBS->VERSIONS_PER_SECOND (1e6): the rate at which the
	// master advances the commit version with the passage of TIME, independent of commit
	// traffic. It is what makes the MVCC window a duration rather than a transaction count — one
	// version per microsecond, so 5 seconds is 5,000,000 versions whether the cluster committed
	// once in that span or a million times.
	versionsPerSecond = 1_000_000

	// mvccWindow = MAX_WRITE_TRANSACTION_LIFE_VERSIONS = 5 * VERSIONS_PER_SECOND. A read
	// version older than (latestVersion - mvccWindow) yields transaction_too_old(1007). SimFDB
	// never actually GCs history (retaining it is strictly safer); the window is pure version
	// arithmetic here.
	mvccWindow = 5 * versionsPerSecond
)

// clockVersion is the version the FDB master would have minted at the simulated clock's current
// reading: microseconds elapsed since dst.Epoch, which at VERSIONS_PER_SECOND = 1e6 is exactly
// one version per microsecond. Epoch is the baseline because that is where NewSim pins a
// SimClock, so a fresh simulated database starts at version 0 and every advance of d translates
// to d.Microseconds() versions.
//
// This is what binds the MVCC window to TIME. Without it SimFDB's versions came only from the
// commit counter, so the 5-second window was really "5,000,000 commits ago" and no amount of
// elapsed simulated time could age a transaction out — transaction_too_old was reachable only by
// a test writing db.lastVersion by hand, which certifies nothing about the modelled system. A
// transaction held open across the window is one of the two ways real code meets 1007 (the other
// being a hand-pinned read version), and it was unreachable.
//
// Returns 0 unless a Clock is actually installed, which keeps this ASYMMETRIC on purpose: a nil
// Env is this repo's only spelling of production (see pkg/dst/env.go), and a production SimDB
// must keep minting versions from the pure logical counter. Binding it to time.Now() there would
// make version assignment — and therefore 1007 — depend on how long the host took to run the
// test, which is precisely the nondeterminism SimFDB exists to remove.
func (db *SimDB) clockVersion() int64 {
	if db.env == nil || db.env.Clock == nil {
		return 0
	}
	elapsed := db.env.Now().Sub(dst.Epoch)
	if elapsed <= 0 {
		return 0
	}
	return int64(elapsed / time.Microsecond)
}

// latestVersion is the version a GRV would return right now: the highest committed version, or
// the clock's version if time has moved further than commits have. The commit counter alone is
// not the cluster's notion of "latest" — a real cluster's version advances with time even while
// idle — and both the read version a transaction pins and the too-old comparison it is later
// judged against have to come from the same notion, or a transaction could be aged out relative
// to a version no GRV would ever have handed it. Caller holds db.mu.
func (db *SimDB) latestVersion() int64 {
	v := db.currentReadVersion()
	if c := db.clockVersion(); c > v {
		return c
	}
	return v
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

	// Size limits (terminal / non-retryable) come FIRST — before transaction_too_old, before
	// conflict resolution. The two verdicts are produced in different places and the order is not
	// a matter of taste:
	//
	//   - 2101/2102/2103 are decided ENTIRELY CLIENT-SIDE, inside Commit, before the commit
	//     request is ever sent (client/transaction.go:1768-1808 — validateMutation then the
	//     approximateCommitSize gate, both ahead of the RPC).
	//   - 1007 is the RESOLVER's answer, and it only exists once the request has reached the
	//     cluster (SkipList.cpp:837). A transaction rejected client-side never gets one.
	//
	// So a transaction that is BOTH too old and too large reports the size error. Reporting 1007
	// instead is not a cosmetic mismatch: 1007 is retryable and the size codes are terminal, so a
	// runner would reset and re-send a transaction that can never commit at any read version, and
	// spin until it exhausts its retry limit. SimFDB checked too-old first and did exactly that.
	if err := checkSizeLimits(tx); err != nil {
		return err
	}

	// transaction_too_old(1007): a read version below the MVCC window. A distinct, earlier
	// verdict than not_committed — and it never fires for a write-only transaction (one with no
	// read conflict ranges), matching FDB (SkipList.cpp:837 gates on read_conflict_ranges.size()).
	//
	// Measured against latestVersion, not the commit counter: the window is a DURATION, and a
	// cluster's version advances with time whether or not anyone commits. Comparing against
	// db.lastVersion made the window "5,000,000 commits wide", so a transaction could be held
	// open for simulated hours and still commit.
	if len(tx.readConflicts) > 0 && tx.readVersion < db.latestVersion()-mvccWindow {
		return fdb.Error{Code: 1007}
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

	// The commit version is the later of "one past the last commit" and the clock's version, so
	// versions track elapsed simulated time while staying STRICTLY increasing: when the clock has
	// not moved (or there is none) successive commits still step by one, and when it has, the
	// jump is exactly the elapsed microseconds. This is the master's rule — it advances the
	// version with time and hands out the next one on demand.
	cv := db.lastVersion + 1
	if c := db.clockVersion(); c > cv {
		cv = c
	}
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

// sizeofMutationRef / sizeofKeyRangeRef are the C++ struct sizes the transaction-size accounting
// charges PER ENTRY, on top of the key and value bytes. They are not padding to be rounded away:
// a transaction of many tiny mutations is dominated by them, so a sim that counts only raw bytes
// certifies commits at a size the real client rejects — and, worse, the record layer's
// "have I filled the transaction yet?" checks against GetApproximateSize come out systematically
// low, so a batch sized against SimFDB overflows against FDB.
//
// The values are the client's, verified byte-exact against libfdb_c
// (client/transaction.go:2492-2504 and its TestDifferential_ApproximateSize /
// TestDifferential_TransactionSizeLimit): flow/Arena.h:370 wraps StringRef in
// `#pragma pack(push, 4)`, so StringRef is 12 bytes, giving
// sizeof(KeyRangeRef) = 2×StringRef = 24 and sizeof(MutationRef) = 44. The natural-alignment
// guesses (32/48) over-count and reject slightly early.
const (
	sizeofMutationRef = 44
	sizeofKeyRangeRef = 24
)

// mutationBytes is one buffered mutation's key+value contribution, expressed in the CLIENT'S
// mutation representation rather than SimFDB's — the accounting has to size the request that
// would go on the wire, not the shape this package happens to buffer:
//
//   - A single-key Clear is rewritten by the client into ClearRange(k, k+\x00), Key=k and
//     Value=k+\x00 (client/transaction.go:1352-1360), so it costs len(k) TWICE plus the trailing
//     zero byte. SimFDB stores it as key-only, which under-counts it by nearly half.
//   - A ClearRange rides as Key=begin, Value=end (client/transaction.go:1393-1397).
//   - Everything else is key + value.
func mutationBytes(m mutation) int64 {
	switch m.kind {
	case mutClear:
		return int64(2*len(m.key) + 1)
	case mutClearRange:
		return int64(len(m.key) + len(m.end))
	default:
		return int64(len(m.key) + len(m.value))
	}
}

// conflictRangeBytes sizes a conflict-range list: its keys plus sizeof(KeyRangeRef) each. Both
// the read and the write list are charged — the commit request carries both
// (client/transaction.go:2565-2578).
func conflictRangeBytes(rs []keyRange) int64 {
	var n int64
	for _, r := range rs {
		n += int64(len(r.begin)+len(r.end)) + sizeofKeyRangeRef
	}
	return n
}

// commitSize is the size the transaction_too_large(2101) gate measures — the client's
// approximateCommitSize (client/transaction.go:2555-2579): every mutation charged
// sizeof(MutationRef), every read AND write conflict range charged sizeof(KeyRangeRef).
//
// One deliberate divergence, called out because it moves the threshold in the UNSAFE direction:
// the client sizes the COALESCED write map it is about to ship (coalesceCommitVectors —
// O(distinct keys)), while SimFDB's buffer is the raw op log, so a transaction that hammers one
// key repeatedly is sized higher here and can trip 2101 where libfdb_c commits. That was already
// true of the byte-only accounting this replaces; closing it means porting the write-map
// coalescing, not adjusting a constant.
func commitSize(tx *simTxn) int64 {
	var n int64
	for _, m := range tx.buffer {
		n += mutationBytes(m) + sizeofMutationRef
	}
	return n + conflictRangeBytes(tx.readConflicts) + conflictRangeBytes(tx.writeConflicts)
}

// checkSizeLimits enforces the terminal key/value/transaction size limits, in the client's own
// order: per-mutation key/value validation first, then the whole-transaction size
// (client/transaction.go:1768-1808), so an oversized key or value that ALSO crosses 10 MB
// reports 2102/2103 rather than 2101.
func checkSizeLimits(tx *simTxn) error {
	for _, m := range tx.buffer {
		if len(m.key) > keySizeLimit || len(m.end) > keySizeLimit {
			return fdb.Error{Code: 2102} // key_too_large
		}
		if len(m.value) > valueSizeLimit {
			return fdb.Error{Code: 2103} // value_too_large
		}
	}
	if commitSize(tx) > txnSizeLimit {
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
