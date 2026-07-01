package client

import (
	"encoding/binary"
	"errors"
	"sync"
	"testing"
	"time"

	"fdb.dev/pkg/fdbgo/transport"
	"fdb.dev/pkg/fdbgo/wire"
	"fdb.dev/pkg/fdbgo/wire/types"
)

// ============================================================================
// buildCommitTransactionRequest — round-trip via types.CommitTransactionRequest.
// ============================================================================

func TestBuildCommitTransactionRequest_Plain(t *testing.T) {
	t.Parallel()
	tx := newTestTx()
	tx.tenantId = NoTenantID
	tx.readVersion = 100
	tx.mutations = []Mutation{
		{Type: MutSetValue, Key: []byte("k"), Value: []byte("v")},
	}
	tx.readConflicts = []KeyRange{{Begin: []byte("rk"), End: []byte("rk\x00")}}
	tx.writeConflicts = []KeyRange{{Begin: []byte("wk"), End: []byte("wk\x00")}}

	body, bufp := buildCommitTransactionRequest(tx, transport.UID{First: 1, Second: 2}, tx.mutations, tx.writeConflicts)
	defer marshalBufPool.Put(bufp)

	var req types.CommitTransactionRequest
	if err := req.UnmarshalFDB(body); err != nil {
		t.Fatalf("UnmarshalFDB: %v", err)
	}
	if req.Flags != 0 {
		t.Errorf("Flags: got %#x, want 0 (lockAware was false)", req.Flags)
	}
	if req.Transaction.Lock_aware {
		t.Error("Transaction.Lock_aware: got true, want false")
	}
	if req.Transaction.ReadSnapshot != 100 {
		t.Errorf("ReadSnapshot: got %d, want 100", req.Transaction.ReadSnapshot)
	}
	if len(req.Transaction.Mutations) != 1 {
		t.Fatalf("Mutations: got %d, want 1", len(req.Transaction.Mutations))
	}
	if string(req.Transaction.Mutations[0].Param1) != "k" {
		t.Errorf("mutation key: got %q, want \"k\"", req.Transaction.Mutations[0].Param1)
	}
	if len(req.Transaction.ReadConflictRanges) != 1 {
		t.Fatalf("ReadConflictRanges: got %d, want 1", len(req.Transaction.ReadConflictRanges))
	}
	if string(req.Transaction.ReadConflictRanges[0].Begin) != "rk" {
		t.Errorf("read conflict begin: got %q, want \"rk\"", req.Transaction.ReadConflictRanges[0].Begin)
	}
	if req.TenantInfo.TenantId != NoTenantID {
		t.Errorf("TenantId: got %d, want %d (NoTenantID)", req.TenantInfo.TenantId, NoTenantID)
	}
}

func TestBuildCommitTransactionRequest_LockAware(t *testing.T) {
	t.Parallel()
	tx := newTestTx()
	tx.tenantId = NoTenantID
	tx.lockAware = true
	tx.mutations = []Mutation{
		{Type: MutSetValue, Key: []byte("k"), Value: []byte("v")},
	}
	tx.writeConflicts = []KeyRange{{Begin: []byte("k"), End: []byte("k\x00")}}

	body, bufp := buildCommitTransactionRequest(tx, transport.UID{First: 1, Second: 2}, tx.mutations, tx.writeConflicts)
	defer marshalBufPool.Put(bufp)

	var req types.CommitTransactionRequest
	if err := req.UnmarshalFDB(body); err != nil {
		t.Fatalf("UnmarshalFDB: %v", err)
	}
	if req.Flags&0x1 == 0 {
		t.Errorf("Flags: got %#x, FLAG_IS_LOCK_AWARE not set", req.Flags)
	}
	// A lock-aware commit carries ONLY the flag — NOT the CommitTransactionRef.lock_aware field.
	// libfdb_c sets only FLAG_IS_LOCK_AWARE (NativeAPI.actor.cpp:6878) and the commit proxy
	// re-derives transaction.lock_aware from it (CommitProxyServer.actor.cpp:221). Sending the field
	// made Go's commit bytes diverge from libfdb_c (an extra lock_aware=true scalar). Decoding the
	// omitted field yields false; with the field set it would decode true.
	if req.Transaction.Lock_aware {
		t.Error("Transaction.Lock_aware: got true, want false (only the flag conveys lock-awareness; libfdb_c omits the field)")
	}
}

func TestBuildCommitTransactionRequest_TenantPrefix(t *testing.T) {
	t.Parallel()
	const tenantID int64 = 42
	tx := newTestTx()
	tx.tenantId = tenantID
	tx.mutations = []Mutation{
		{Type: MutSetValue, Key: []byte("k"), Value: []byte("v")},
		{Type: MutClearRange, Key: []byte("a"), Value: []byte("z")},
	}
	tx.readConflicts = []KeyRange{{Begin: []byte("rk"), End: []byte("rk\x00")}}
	tx.writeConflicts = []KeyRange{{Begin: []byte("wk"), End: []byte("wk\x00")}}

	body, bufp := buildCommitTransactionRequest(tx, transport.UID{First: 1, Second: 2}, tx.mutations, tx.writeConflicts)
	defer marshalBufPool.Put(bufp)

	var req types.CommitTransactionRequest
	if err := req.UnmarshalFDB(body); err != nil {
		t.Fatalf("UnmarshalFDB: %v", err)
	}

	var prefix [8]byte
	binary.BigEndian.PutUint64(prefix[:], uint64(tenantID))

	// Mutation 0: SetValue. Param1 (key) prefixed; Param2 (value) is NOT
	// prefixed (only keys are tenant-scoped).
	got := req.Transaction.Mutations[0].Param1
	want := append(append([]byte{}, prefix[:]...), 'k')
	if !bytesEqual(got, want) {
		t.Errorf("set key: got %x, want %x", got, want)
	}

	// Mutation 1: ClearRange. Both Param1 (begin) AND Param2 (end) prefixed.
	got = req.Transaction.Mutations[1].Param1
	want = append(append([]byte{}, prefix[:]...), 'a')
	if !bytesEqual(got, want) {
		t.Errorf("clear begin: got %x, want %x", got, want)
	}
	got = req.Transaction.Mutations[1].Param2
	want = append(append([]byte{}, prefix[:]...), 'z')
	if !bytesEqual(got, want) {
		t.Errorf("clear end: got %x, want %x", got, want)
	}

	// Read + write conflict ranges: BOTH ends prefixed.
	got = req.Transaction.ReadConflictRanges[0].Begin
	want = append(append([]byte{}, prefix[:]...), 'r', 'k')
	if !bytesEqual(got, want) {
		t.Errorf("rc begin: got %x, want %x", got, want)
	}
	got = req.Transaction.WriteConflictRanges[0].End
	want = append(append([]byte{}, prefix[:]...), 'w', 'k', 0)
	if !bytesEqual(got, want) {
		t.Errorf("wc end: got %x, want %x", got, want)
	}

	if req.TenantInfo.TenantId != tenantID {
		t.Errorf("TenantId: got %d, want %d", req.TenantInfo.TenantId, tenantID)
	}
}

func TestBuildCommitTransactionRequest_TenantSkipsMetadataVersionKey(t *testing.T) {
	t.Parallel()
	tx := newTestTx()
	tx.tenantId = 7
	// metadataVersionKey is exempt from tenant prefix (matches C++).
	tx.mutations = []Mutation{
		{Type: MutSetValue, Key: append([]byte{}, metadataVersionKey...), Value: []byte("v")},
	}
	tx.writeConflicts = []KeyRange{
		{Begin: append([]byte{}, metadataVersionKey...), End: []byte("\xff/metadataVersion\x00")},
	}

	body, bufp := buildCommitTransactionRequest(tx, transport.UID{First: 1, Second: 2}, tx.mutations, tx.writeConflicts)
	defer marshalBufPool.Put(bufp)

	var req types.CommitTransactionRequest
	if err := req.UnmarshalFDB(body); err != nil {
		t.Fatalf("UnmarshalFDB: %v", err)
	}
	if !bytesEqual(req.Transaction.Mutations[0].Param1, metadataVersionKey) {
		t.Errorf("metadataVersionKey was prefixed: got %x", req.Transaction.Mutations[0].Param1)
	}
}

func TestBuildCommitTransactionRequest_TenantAdjustsVersionstampOffset(t *testing.T) {
	t.Parallel()
	const tenantID int64 = 5
	tx := newTestTx()
	tx.tenantId = tenantID

	// SET_VERSIONSTAMPED_KEY: last 4 bytes are LE offset of where the
	// 10-byte versionstamp goes within the key. After tenant-prefix prepend,
	// the offset must shift by 8 (prefix length).
	key := []byte("xxxxxxxxxxOFFS") // 14 bytes: 10 placeholder + 4 offset
	binary.LittleEndian.PutUint32(key[10:], 0)
	tx.mutations = []Mutation{
		{Type: MutSetVersionstampedKey, Key: key, Value: []byte("v")},
	}

	body, bufp := buildCommitTransactionRequest(tx, transport.UID{First: 1, Second: 2}, tx.mutations, tx.writeConflicts)
	defer marshalBufPool.Put(bufp)

	var req types.CommitTransactionRequest
	if err := req.UnmarshalFDB(body); err != nil {
		t.Fatalf("UnmarshalFDB: %v", err)
	}

	got := req.Transaction.Mutations[0].Param1
	// Expect: 8-byte tenant prefix + original key with the trailing 4-byte
	// offset bumped from 0 to 8.
	wantOffset := uint32(8)
	gotOffset := binary.LittleEndian.Uint32(got[len(got)-4:])
	if gotOffset != wantOffset {
		t.Errorf("versionstamp offset: got %d, want %d (was 0; tenant prefix is 8 bytes)",
			gotOffset, wantOffset)
	}
	// Verify the prefix is at the front.
	var prefix [8]byte
	binary.BigEndian.PutUint64(prefix[:], uint64(tenantID))
	if !bytesEqual(got[:8], prefix[:]) {
		t.Errorf("tenant prefix not at front: got %x", got[:8])
	}
}

// ============================================================================
// parseCommitReply — wire reply → tx.committedVersion / tx.txnBatchId.
//
// The success path round-trip requires ErrorOr<CommitID> wire-marshalling
// infrastructure that the wire package does not expose (only ErrorOrError
// has a public MarshalFDB; ErrorOr-success is server-only). The integration
// tests at //pkg/fdbgo/client/correctness exercise the success path
// end-to-end. Here we pin the failure paths, which are the ones that
// could regress silently.
// ============================================================================

func TestParseCommitReply_ErrorOrError(t *testing.T) {
	t.Parallel()
	// FDB proxy returns ErrorOr<CommitID> with tag=1 (Error). The error
	// code 1020 (not_committed) is a representative retryable commit error.
	errBody := (&types.ErrorOrError{ErrorCode: 1020}).MarshalFDB()

	tx := newTestTx()
	err := tx.parseCommitReply(errBody)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	var fdbErr *wire.FDBError
	if !errors.As(err, &fdbErr) || fdbErr.Code != 1020 {
		t.Errorf("got %v, want FDBError 1020", err)
	}
	// State must NOT be mutated when reply is an error.
	if tx.committedVersion != 0 || tx.txnBatchId != 0 {
		t.Errorf("error reply must not mutate version/batchId: got version=%d batch=%d",
			tx.committedVersion, tx.txnBatchId)
	}
}

func TestParseCommitReply_GarbageBytes(t *testing.T) {
	t.Parallel()
	tx := newTestTx()
	if err := tx.parseCommitReply([]byte{0xFF, 0xFF, 0xFF, 0xFF}); err == nil {
		t.Fatal("expected error on garbage bytes, got nil")
	}
}

func TestParseCommitReply_EmptyBytes(t *testing.T) {
	t.Parallel()
	tx := newTestTx()
	if err := tx.parseCommitReply(nil); err == nil {
		t.Fatal("expected error on nil body, got nil")
	}
}

// ============================================================================
// GetCommittedVersion / GetVersionstamp — pre-commit error contract.
// ============================================================================

func TestGetVersionstamp_FormatBigEndian(t *testing.T) {
	t.Parallel()
	tx := newTestTx()
	tx.hasCommitted = true
	tx.committedVersion = 0x0102030405060708
	tx.txnBatchId = 0x0900

	vs, err := tx.GetVersionstamp()
	if err != nil {
		t.Fatalf("got %v, want nil", err)
	}
	want := []byte{0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08, 0x09, 0x00}
	if !bytesEqual(vs, want) {
		t.Errorf("versionstamp: got %x, want %x", vs, want)
	}
}

// ============================================================================
// MutationRef ↔ Mutation layout — runtime assertion catching what
// compile-time offsetof checks miss (field-type swaps that preserve
// offsets and sizes, e.g. swapping uint8 with int8).
// ============================================================================

func TestMutationLayout_BitIdenticalRoundTrip(t *testing.T) {
	t.Parallel()
	// Construct a Mutation with non-trivial values, build a request through
	// the unsafe-cast path (the production path), unmarshal, and verify the
	// fields round-trip. Catches a field-reorder or type-swap that the
	// compile-time offset assertions in commitpath.go don't catch.
	tx := newTestTx()
	tx.tenantId = NoTenantID
	tx.mutations = []Mutation{
		{Type: MutSetValue, Key: []byte("k1"), Value: []byte("v1")},
		{Type: MutClearRange, Key: []byte("a"), Value: []byte("z")},
		{Type: MutAddValue, Key: []byte("counter"), Value: []byte{1, 0, 0, 0, 0, 0, 0, 0}},
	}
	tx.writeConflicts = []KeyRange{
		{Begin: []byte("k1"), End: []byte("k1\x00")},
		{Begin: []byte("a"), End: []byte("z")},
		{Begin: []byte("counter"), End: []byte("counter\x00")},
	}

	body, bufp := buildCommitTransactionRequest(tx, transport.UID{First: 1, Second: 2}, tx.mutations, tx.writeConflicts)
	defer marshalBufPool.Put(bufp)

	var req types.CommitTransactionRequest
	if err := req.UnmarshalFDB(body); err != nil {
		t.Fatalf("UnmarshalFDB: %v", err)
	}

	want := []struct {
		mutType uint8
		k, v    []byte
	}{
		{uint8(MutSetValue), []byte("k1"), []byte("v1")},
		{uint8(MutClearRange), []byte("a"), []byte("z")},
		{uint8(MutAddValue), []byte("counter"), []byte{1, 0, 0, 0, 0, 0, 0, 0}},
	}
	if len(req.Transaction.Mutations) != len(want) {
		t.Fatalf("count: got %d, want %d", len(req.Transaction.Mutations), len(want))
	}
	for i, w := range want {
		got := req.Transaction.Mutations[i]
		if got.MutType != w.mutType {
			t.Errorf("mut[%d].MutType: got %d, want %d (likely Type field swapped)",
				i, got.MutType, w.mutType)
		}
		if !bytesEqual(got.Param1, w.k) {
			t.Errorf("mut[%d].Param1: got %q, want %q (likely Key field swapped)",
				i, got.Param1, w.k)
		}
		if !bytesEqual(got.Param2, w.v) {
			t.Errorf("mut[%d].Param2: got %q, want %q (likely Value field swapped)",
				i, got.Param2, w.v)
		}
	}
}

// ============================================================================
// jitterBackoff — ±10% jitter on the dummy-transaction retry sleep.
// ============================================================================

func TestJitterBackoff_Bounds(t *testing.T) {
	t.Parallel()
	base := 100 * time.Millisecond
	floor := time.Duration(float64(base) * 0.9)
	ceiling := time.Duration(float64(base)*1.1) + time.Microsecond // tiny float fudge
	// 1000 samples should cover both extremes of the [0.9, 1.1) range.
	var minSeen, maxSeen time.Duration = base * 2, 0
	for i := 0; i < 1000; i++ {
		got := jitterBackoff(base)
		if got < floor {
			t.Errorf("jitterBackoff = %v, below floor %v", got, floor)
		}
		if got > ceiling {
			t.Errorf("jitterBackoff = %v, above ceiling %v", got, ceiling)
		}
		if got < minSeen {
			minSeen = got
		}
		if got > maxSeen {
			maxSeen = got
		}
	}
	// Sanity: spread of 1000 samples should reach BOTH halves of the band.
	// Probability of failure (all samples in just one half) ~= 2^-999.
	if minSeen >= base {
		t.Errorf("1000 samples never went below base — RNG dead? min=%v", minSeen)
	}
	if maxSeen <= base {
		t.Errorf("1000 samples never went above base — RNG dead? max=%v", maxSeen)
	}
}

func TestJitterBackoff_ZeroAndNegative(t *testing.T) {
	t.Parallel()
	// d <= 0 must short-circuit (multiplying by random factor would still
	// give 0, but the explicit guard documents the contract).
	if got := jitterBackoff(0); got != 0 {
		t.Errorf("jitterBackoff(0) = %v, want 0", got)
	}
	if got := jitterBackoff(-1); got != -1 {
		t.Errorf("jitterBackoff(-1) = %v, want -1", got)
	}
}

// ============================================================================
// onErrorRetryable — error-code → onError retry decision (RFC-105 single source).
// ============================================================================

func TestOnErrorRetryable_RetryableSet(t *testing.T) {
	t.Parallel()
	// Every code in this list MUST return true. If a future commit
	// silently drops one (typo in the switch case label), this test
	// catches it.
	retryable := []int{
		ErrTransactionTooOld, ErrFutureVersion,
		ErrNotCommitted, ErrDatabaseLocked, ErrProcessBehind,
		ErrBatchTransactionThrottled, ErrTagThrottled, ErrProxyTagThrottled,
		ErrThrottledHotShard, ErrRangeLocked, ErrBlobGranuleRequestFailed,
		ErrAllProxiesUnreachable, ErrCommitUnknownResult, ErrClusterVersionChanged,
		ErrProxyMemoryLimitExceeded, ErrGrvProxyMemoryLimit,
	}
	for _, code := range retryable {
		if !onErrorRetryable(code) {
			t.Errorf("onErrorRetryable(%d) = false, want true", code)
		}
	}
}

func TestOnErrorRetryable_NotRetryableSet(t *testing.T) {
	t.Parallel()
	// A representative set of non-retryable codes. The function's
	// default case returns false, so we sanity-check that codes NOT
	// in the explicit retryable set are not silently retried.
	notRetryable := []int{
		0,                      // success
		ErrTransactionTimedOut, // user gave up — explicit "NEVER retryable" comment in transaction.go
		ErrInvertedRange,       // begin > end is structural
		ErrOperationFailed,     // endpoint not supported
		ErrWrongShardServer,    // location-cache invalidation, retried elsewhere (NOT here)
		1234567,                // arbitrary unknown code
		-1,                     // negative
	}
	for _, code := range notRetryable {
		if onErrorRetryable(code) {
			t.Errorf("onErrorRetryable(%d) = true, want false", code)
		}
	}
}

// ============================================================================
// clearAndReturn — clears a pooled slice's full backing array before returning
// it, so the pool never retains committed key/value byte slices: mutSlicePool
// (values up to 100KB) and crSlicePool (conflict-range keys). Codex review on #4.
//
// Each subtest uses a LOCAL sync.Pool, not the package-global mutSlicePool /
// crSlicePool: clearAndReturn publishes the slice into the pool, so sharing a
// global pool here would race the parallel tenant-commit tests that borrow from
// it (those Get the pointer and append into the shared backing array).
// ============================================================================

func TestClearAndReturn_ClearsBackingArray(t *testing.T) {
	t.Parallel()

	t.Run("MutationRef", func(t *testing.T) {
		t.Parallel()
		var pool sync.Pool
		// Refs in EVERY slot, returned at a short len (1) over a larger cap (3):
		// the clear must drop refs in all slots, including the two "beyond len"
		// ones a smaller follow-up commit would leave untouched.
		backing := make([]types.MutationRef, 3)
		for i := range backing {
			backing[i] = types.MutationRef{MutType: uint8(i + 1), Param1: []byte{'k', byte(i)}, Param2: []byte{'v', byte(i)}}
		}
		s := backing[:1] // len 1, cap 3
		clearAndReturn(&pool, &s)
		for i := range backing {
			if backing[i].MutType != 0 || backing[i].Param1 != nil || backing[i].Param2 != nil {
				t.Errorf("slot %d not cleared (pool would retain its byte slices): %+v", i, backing[i])
			}
		}
		if len(s) != 0 || cap(s) != 3 {
			t.Errorf("released slice len=%d cap=%d, want 0/3 (capacity preserved)", len(s), cap(s))
		}
	})

	t.Run("KeyRangeRef", func(t *testing.T) {
		t.Parallel()
		var pool sync.Pool
		backing := make([]types.KeyRangeRef, 3)
		for i := range backing {
			backing[i] = types.KeyRangeRef{Begin: []byte{'b', byte(i)}, End: []byte{'e', byte(i)}}
		}
		s := backing[:1]
		clearAndReturn(&pool, &s)
		for i := range backing {
			if backing[i].Begin != nil || backing[i].End != nil {
				t.Errorf("slot %d not cleared (pool would retain conflict-range keys): %+v", i, backing[i])
			}
		}
	})
}

// ============================================================================
// Helpers.
// ============================================================================

func bytesEqual(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
