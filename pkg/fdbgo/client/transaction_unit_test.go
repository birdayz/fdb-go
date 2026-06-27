package client

import (
	"context"
	"encoding/binary"
	"errors"
	"testing"
	"time"

	"fdb.dev/pkg/fdbgo/wire"
)

// ============================================================================
// validateVersionstampOffset — pure parser, atomicOp validation gate.
// ============================================================================

func TestValidateVersionstampOffset_TooShort(t *testing.T) {
	t.Parallel()
	for _, n := range []int{0, 1, 2, 3} {
		data := make([]byte, n)
		err := validateVersionstampOffset(data)
		var fdbErr *wire.FDBError
		if !errors.As(err, &fdbErr) || fdbErr.Code != 2000 {
			t.Errorf("len=%d: got %v, want FDBError 2000 (client_invalid_operation)", n, err)
		}
	}
}

func TestValidateVersionstampOffset_OffsetPlusTenExceedsBody(t *testing.T) {
	t.Parallel()
	// 14-byte buffer = 10-byte body + 4-byte LE offset.
	// Only valid offset is 0. Any offset > 0 means versionstamp would exceed body.
	data := make([]byte, 14)
	binary.LittleEndian.PutUint32(data[10:], 1) // offset=1, but only 10 body bytes available
	err := validateVersionstampOffset(data)
	var fdbErr *wire.FDBError
	if !errors.As(err, &fdbErr) || fdbErr.Code != 2000 {
		t.Errorf("offset=1 with 10-byte body: got %v, want FDBError 2000", err)
	}
}

func TestValidateVersionstampOffset_NegativeOffset(t *testing.T) {
	t.Parallel()
	// LE-encoded -1 == 0xFFFFFFFF. C++ rejects negative offsets.
	data := make([]byte, 14)
	binary.LittleEndian.PutUint32(data[10:], 0xFFFFFFFF)
	err := validateVersionstampOffset(data)
	var fdbErr *wire.FDBError
	if !errors.As(err, &fdbErr) || fdbErr.Code != 2000 {
		t.Errorf("negative offset (LE -1): got %v, want FDBError 2000", err)
	}
}

func TestValidateVersionstampOffset_ExactlyAtBoundary(t *testing.T) {
	t.Parallel()
	// 14-byte buffer = 10-byte body + 4-byte offset suffix. offset=0 means
	// the 10-byte versionstamp occupies bytes [0..10). Tightest valid case.
	data := make([]byte, 14)
	binary.LittleEndian.PutUint32(data[10:], 0)
	if err := validateVersionstampOffset(data); err != nil {
		t.Errorf("offset=0 with body=10: got %v, want nil", err)
	}
}

func TestValidateVersionstampOffset_ValidMidBuffer(t *testing.T) {
	t.Parallel()
	// 24-byte buffer = 20-byte body + 4-byte offset. offset=5 means versionstamp
	// occupies bytes [5..15) — fully inside the 20-byte body.
	data := make([]byte, 24)
	binary.LittleEndian.PutUint32(data[20:], 5)
	if err := validateVersionstampOffset(data); err != nil {
		t.Errorf("offset=5 in 20-byte body: got %v, want nil", err)
	}
}

func TestValidateVersionstampOffset_OffsetEqualsBodyMinusTen(t *testing.T) {
	t.Parallel()
	// 24-byte buffer, offset=10 → versionstamp spans [10..20), exactly the
	// last 10 bytes of the 20-byte body.
	data := make([]byte, 24)
	binary.LittleEndian.PutUint32(data[20:], 10)
	if err := validateVersionstampOffset(data); err != nil {
		t.Errorf("offset=body-10: got %v, want nil", err)
	}
}

// ============================================================================
// keyAfterBytes — pure helper, must always allocate.
// ============================================================================

func TestKeyAfterBytes_AppendsZero(t *testing.T) {
	t.Parallel()
	got := keyAfterBytes([]byte("abc"))
	want := []byte{'a', 'b', 'c', 0}
	if !bytesEqual(got, want) {
		t.Errorf("keyAfterBytes(%q) = %v, want %v", "abc", got, want)
	}
}

func TestKeyAfterBytes_EmptyKey(t *testing.T) {
	t.Parallel()
	got := keyAfterBytes([]byte{})
	if len(got) != 1 || got[0] != 0 {
		t.Errorf("keyAfterBytes(empty) = %v, want [0]", got)
	}
}

func TestKeyAfterBytes_NilKey(t *testing.T) {
	t.Parallel()
	got := keyAfterBytes(nil)
	if len(got) != 1 || got[0] != 0 {
		t.Errorf("keyAfterBytes(nil) = %v, want [0]", got)
	}
}

func TestKeyAfterBytes_ReturnsCopy(t *testing.T) {
	t.Parallel()
	// Mutating the input must not leak into the result. The doc-comment
	// promises "always allocates — safe for storing in conflict ranges".
	src := []byte{1, 2, 3}
	got := keyAfterBytes(src)
	src[0] = 0xFF
	if got[0] != 1 {
		t.Errorf("keyAfterBytes did not copy: input mutation leaked, got[0]=%d, want 1", got[0])
	}
}

// ============================================================================
// isSpecialKey — \xff\xff (special key space) prefix check.
// ============================================================================

func TestIsSpecialKey(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		key  []byte
		want bool
	}{
		{"empty", []byte{}, false},
		{"nil", nil, false},
		{"single 0xff", []byte{0xff}, false},
		{"two 0xff", []byte{0xff, 0xff}, true},
		{"two 0xff plus suffix", []byte{0xff, 0xff, '/', 'x'}, true},
		{"0xff then 0x00", []byte{0xff, 0x00}, false},
		{"0x00 then 0xff", []byte{0x00, 0xff}, false},
		{"regular key", []byte("hello"), false},
		{"single \\xff/metadataVersion-style", []byte("\xff/m"), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isSpecialKey(tt.key); got != tt.want {
				t.Errorf("isSpecialKey(%q) = %v, want %v", tt.key, got, tt.want)
			}
		})
	}
}

// ============================================================================
// checkTimeout — deadline enforcement.
// ============================================================================

func TestCheckTimeout_DisabledWhenZero(t *testing.T) {
	t.Parallel()
	tx := &Transaction{
		timeout:  0,
		deadline: time.Now().Add(-time.Hour), // long past
	}
	if err := tx.checkTimeout(); err != nil {
		t.Errorf("timeout=0 must always return nil, got %v", err)
	}
}

func TestCheckTimeout_NotExpired(t *testing.T) {
	t.Parallel()
	tx := &Transaction{
		timeout:  5 * time.Second,
		deadline: time.Now().Add(time.Hour),
	}
	if err := tx.checkTimeout(); err != nil {
		t.Errorf("future deadline: got %v, want nil", err)
	}
}

func TestCheckTimeout_Expired(t *testing.T) {
	t.Parallel()
	tx := &Transaction{
		timeout:  5 * time.Second,
		deadline: time.Now().Add(-time.Second),
	}
	err := tx.checkTimeout()
	var fdbErr *wire.FDBError
	if !errors.As(err, &fdbErr) || fdbErr.Code != ErrTransactionTimedOut {
		t.Errorf("expired deadline: got %v, want FDBError %d", err, ErrTransactionTimedOut)
	}
}

// ============================================================================
// conflictBufAlloc — pooled bumper allocator.
// ============================================================================

func TestConflictBufAlloc_AllocatesOnFreshTransaction(t *testing.T) {
	t.Parallel()
	// First call on a fresh transaction either pulls a buffer from the
	// sync.Pool (most common) or grows from zero. Either way, the returned
	// slice must have len==n and the backing buffer must have cap >= n.
	tx := &Transaction{}
	tx.conflictMu.Lock()
	buf := tx.conflictBufAlloc(100)
	tx.conflictMu.Unlock()
	if len(buf) != 100 {
		t.Errorf("len: got %d, want 100", len(buf))
	}
	if cap(tx.conflictBuf) < 100 {
		t.Errorf("backing cap: got %d, want >=100", cap(tx.conflictBuf))
	}
}

func TestConflictBufAlloc_BumpsWithinExistingCapacity(t *testing.T) {
	t.Parallel()
	tx := &Transaction{}
	tx.conflictMu.Lock()
	a := tx.conflictBufAlloc(50)
	prevCap := cap(tx.conflictBuf)
	b := tx.conflictBufAlloc(50)
	tx.conflictMu.Unlock()
	if cap(tx.conflictBuf) != prevCap {
		t.Errorf("cap should not change for in-capacity bump, prev=%d now=%d", prevCap, cap(tx.conflictBuf))
	}
	// Disjointness check: write distinct sentinels into a and b. If the
	// allocator handed back overlapping regions, the second write would
	// stomp the first.
	for i := range a {
		a[i] = 0xAA
	}
	for i := range b {
		b[i] = 0xBB
	}
	for i, v := range a {
		if v != 0xAA {
			t.Fatalf("a[%d]=0x%x — second alloc overlapped first", i, v)
		}
	}
}

func TestConflictBufAlloc_GrowthPathBeyondPoolCapacity(t *testing.T) {
	t.Parallel()
	// First call requests > 32K so the pool's default 32K-cap buffer is rejected
	// and the growth path triggers (newCap = max(2*cap, len+n), min 4096).
	tx := &Transaction{}
	tx.conflictMu.Lock()
	const huge = 64 * 1024
	buf := tx.conflictBufAlloc(huge)
	tx.conflictMu.Unlock()
	if len(buf) != huge {
		t.Errorf("len: got %d, want %d", len(buf), huge)
	}
	if cap(tx.conflictBuf) < huge {
		t.Errorf("growth-path cap: got %d, want >=%d", cap(tx.conflictBuf), huge)
	}
}

func TestConflictBufAlloc_GrowsWhenExistingTooSmall(t *testing.T) {
	t.Parallel()
	tx := &Transaction{}
	tx.conflictMu.Lock()
	tx.conflictBufAlloc(100) // seed with small alloc
	prevCap := cap(tx.conflictBuf)
	// Request a single chunk larger than what's left, but still within typical pool size.
	tx.conflictBufAlloc(prevCap * 4)
	tx.conflictMu.Unlock()
	if cap(tx.conflictBuf) <= prevCap {
		t.Errorf("must grow when existing buffer too small: prev=%d now=%d", prevCap, cap(tx.conflictBuf))
	}
}

// ============================================================================
// addReadConflict / addWriteConflict — slice append + gating flags.
// ============================================================================

func TestAddReadConflictForKey_EncodesHalfOpenRange(t *testing.T) {
	t.Parallel()
	tx := &Transaction{}
	tx.addReadConflictForKey([]byte("k"))
	if len(tx.readConflicts) != 1 {
		t.Fatalf("readConflicts len: got %d, want 1", len(tx.readConflicts))
	}
	r := tx.readConflicts[0]
	if string(r.Begin) != "k" || string(r.End) != "k\x00" {
		t.Errorf("range: got [%q,%q), want [k,k\\x00)", r.Begin, r.End)
	}
}

func TestAddReadConflict_AppendsExplicitRange(t *testing.T) {
	t.Parallel()
	tx := &Transaction{}
	tx.addReadConflict([]byte("aa"), []byte("zz"))
	if len(tx.readConflicts) != 1 {
		t.Fatalf("readConflicts len: got %d, want 1", len(tx.readConflicts))
	}
	r := tx.readConflicts[0]
	if string(r.Begin) != "aa" || string(r.End) != "zz" {
		t.Errorf("range: got [%q,%q), want [aa,zz)", r.Begin, r.End)
	}
}

func TestAddReadConflictForKey_MultipleAppendsShareBackingBuffer(t *testing.T) {
	t.Parallel()
	tx := &Transaction{}
	for _, k := range []string{"a", "bb", "ccc"} {
		tx.addReadConflictForKey([]byte(k))
	}
	if len(tx.readConflicts) != 3 {
		t.Fatalf("readConflicts len: got %d, want 3", len(tx.readConflicts))
	}
	wantBegins := []string{"a", "bb", "ccc"}
	wantEnds := []string{"a\x00", "bb\x00", "ccc\x00"}
	for i, r := range tx.readConflicts {
		if string(r.Begin) != wantBegins[i] || string(r.End) != wantEnds[i] {
			t.Errorf("range[%d]: got [%q,%q), want [%q,%q)", i, r.Begin, r.End, wantBegins[i], wantEnds[i])
		}
	}
}

func TestAddWriteConflictForKey_DisabledWhenWriteConflictsDisabled(t *testing.T) {
	t.Parallel()
	tx := &Transaction{writeConflictsDisabled: true}
	tx.addWriteConflictForKey([]byte("k"))
	if len(tx.writeConflicts) != 0 {
		t.Errorf("writeConflictsDisabled=true must skip append, got len=%d", len(tx.writeConflicts))
	}
}

func TestAddWriteConflictForKey_ConsumesNextWriteNoConflict(t *testing.T) {
	t.Parallel()
	tx := &Transaction{nextWriteNoConflict: true}
	tx.addWriteConflictForKey([]byte("k"))
	if len(tx.writeConflicts) != 0 {
		t.Errorf("nextWriteNoConflict must skip first append, got len=%d", len(tx.writeConflicts))
	}
	if tx.nextWriteNoConflict {
		t.Error("nextWriteNoConflict must auto-reset to false after one mutation")
	}
	// Subsequent mutation MUST add a conflict (auto-reset semantics).
	tx.addWriteConflictForKey([]byte("k2"))
	if len(tx.writeConflicts) != 1 {
		t.Errorf("after auto-reset, second mutation must append, got len=%d", len(tx.writeConflicts))
	}
}

func TestAddWriteConflict_ExplicitRange_DisabledFlags(t *testing.T) {
	t.Parallel()
	t.Run("disabled", func(t *testing.T) {
		t.Parallel()
		tx := &Transaction{writeConflictsDisabled: true}
		tx.addWriteConflict([]byte("a"), []byte("z"))
		if len(tx.writeConflicts) != 0 {
			t.Errorf("disabled flag must skip explicit range, got len=%d", len(tx.writeConflicts))
		}
	})
	t.Run("nextWriteNoConflict", func(t *testing.T) {
		t.Parallel()
		tx := &Transaction{nextWriteNoConflict: true}
		tx.addWriteConflict([]byte("a"), []byte("z"))
		if len(tx.writeConflicts) != 0 {
			t.Errorf("nextWriteNoConflict must skip explicit range, got len=%d", len(tx.writeConflicts))
		}
		if tx.nextWriteNoConflict {
			t.Error("nextWriteNoConflict must auto-reset")
		}
	})
}

// ============================================================================
// EnsureMutationCapacity — preallocate without losing existing entries.
// ============================================================================

func TestEnsureMutationCapacity_GrowsWhenSmaller(t *testing.T) {
	t.Parallel()
	tx := &Transaction{}
	tx.mutations = []Mutation{{Type: MutSetValue, Key: []byte("k1"), Value: []byte("v1")}}
	tx.writeConflicts = []KeyRange{{Begin: []byte("k1"), End: []byte("k1\x00")}}
	tx.EnsureMutationCapacity(100)
	if cap(tx.mutations) < 100 {
		t.Errorf("mutations cap: got %d, want >=100", cap(tx.mutations))
	}
	if cap(tx.writeConflicts) < 100 {
		t.Errorf("writeConflicts cap: got %d, want >=100", cap(tx.writeConflicts))
	}
	if len(tx.mutations) != 1 || string(tx.mutations[0].Key) != "k1" {
		t.Error("existing mutation lost during cap grow")
	}
	if len(tx.writeConflicts) != 1 || string(tx.writeConflicts[0].Begin) != "k1" {
		t.Error("existing writeConflict lost during cap grow")
	}
}

func TestEnsureMutationCapacity_NoOpWhenSufficient(t *testing.T) {
	t.Parallel()
	tx := &Transaction{}
	tx.mutations = make([]Mutation, 0, 200)
	tx.writeConflicts = make([]KeyRange, 0, 200)
	preMut := cap(tx.mutations)
	preWC := cap(tx.writeConflicts)
	tx.EnsureMutationCapacity(50)
	if cap(tx.mutations) != preMut || cap(tx.writeConflicts) != preWC {
		t.Errorf("must not shrink or reallocate when already sufficient: mut %d→%d, wc %d→%d",
			preMut, cap(tx.mutations), preWC, cap(tx.writeConflicts))
	}
}

// ============================================================================
// postCommitReset — keeps committed identity, drops in-flight state.
// ============================================================================

func TestPostCommitReset_ClearsTransactionalState(t *testing.T) {
	t.Parallel()
	tx := newTestTx()
	tx.state.Store(int32(txStateCommitted))
	tx.hasReadVersion = true
	tx.userSetReadVersion = true
	tx.readVersion = 12345
	tx.mutations = []Mutation{{Type: MutSetValue, Key: []byte("k"), Value: []byte("v")}}
	tx.addReadConflictForKey([]byte("rk"))
	tx.addWriteConflictForKey([]byte("wk"))

	tx.postCommitReset()

	if got := txState(tx.state.Load()); got != txStateActive {
		t.Errorf("state: got %d, want txStateActive", got)
	}
	if tx.hasReadVersion || tx.userSetReadVersion || tx.readVersion != 0 {
		t.Errorf("readVersion fields not cleared: has=%v userSet=%v ver=%d",
			tx.hasReadVersion, tx.userSetReadVersion, tx.readVersion)
	}
	if len(tx.mutations) != 0 || len(tx.readConflicts) != 0 || len(tx.writeConflicts) != 0 {
		t.Errorf("slices not cleared: muts=%d rc=%d wc=%d",
			len(tx.mutations), len(tx.readConflicts), len(tx.writeConflicts))
	}
}

func TestPostCommitReset_PreservesCommittedIdentity(t *testing.T) {
	t.Parallel()
	tx := newTestTx()
	tx.committedVersion = 999
	tx.txnBatchId = 42
	tx.hasCommitted = true

	tx.postCommitReset()

	if tx.committedVersion != 999 {
		t.Errorf("committedVersion clobbered: got %d, want 999", tx.committedVersion)
	}
	if tx.txnBatchId != 42 {
		t.Errorf("txnBatchId clobbered: got %d, want 42", tx.txnBatchId)
	}
	if !tx.hasCommitted {
		t.Error("hasCommitted clobbered: postCommitReset must keep it true so GetCommittedVersion works")
	}
}

func TestPostCommitReset_ReturnsConflictBufferToPool(t *testing.T) {
	t.Parallel()
	tx := newTestTx()
	tx.addReadConflictForKey([]byte("k"))
	if tx.conflictBuf == nil {
		t.Fatal("setup: conflictBuf should be non-nil after a conflict append")
	}
	tx.postCommitReset()
	if tx.conflictBuf != nil {
		t.Errorf("conflictBuf must be nil after pool return, got len=%d cap=%d", len(tx.conflictBuf), cap(tx.conflictBuf))
	}
	if tx.conflictBufOwner != nil {
		t.Error("conflictBufOwner must be nil after pool return")
	}
}

// ============================================================================
// reset (internal OnError reset) — full reset minus persistent options.
// ============================================================================

func TestReset_ClearsCommittedIdentity(t *testing.T) {
	t.Parallel()
	tx := newTestTx()
	tx.committedVersion = 999
	tx.txnBatchId = 42
	tx.hasCommitted = true
	tx.nextWriteNoConflict = true

	tx.reset()

	if tx.committedVersion != 0 || tx.hasCommitted || tx.txnBatchId != 0 {
		t.Errorf("internal reset must clear committed identity: ver=%d has=%v batch=%d",
			tx.committedVersion, tx.hasCommitted, tx.txnBatchId)
	}
	if tx.nextWriteNoConflict {
		t.Error("internal reset must clear nextWriteNoConflict (C++ creates fresh state)")
	}
}

func TestReset_PreservesPersistentOptions(t *testing.T) {
	t.Parallel()
	tx := newTestTx()
	tx.priority = PriorityBatch
	tx.causalReadRisky = true
	tx.lockAware = true
	tx.readLockAware = true
	tx.sizeLimit = 5_000_000
	tx.maxRetryDelay = 10 * time.Second
	tx.rywDisabled = true
	tx.snapshotRYWDisableCount = 1
	tx.tenantId = 7
	tx.tags = []string{"a", "b"}
	tx.retryCount = 3
	tx.backoff = 200 * time.Millisecond

	tx.reset()

	if tx.priority != PriorityBatch || !tx.causalReadRisky || !tx.lockAware || !tx.readLockAware {
		t.Error("reset clobbered priority / causalReadRisky / lockAware / readLockAware")
	}
	if tx.sizeLimit != 5_000_000 || tx.maxRetryDelay != 10*time.Second {
		t.Error("reset clobbered sizeLimit or maxRetryDelay")
	}
	if !tx.rywDisabled || tx.snapshotRYWDisableCount != 1 {
		t.Error("reset clobbered RYW disable flags (snapshotRYWDisableCount must be preserved across reset — persistent option)")
	}
	if tx.tenantId != 7 {
		t.Errorf("reset clobbered tenantId: got %d, want 7", tx.tenantId)
	}
	if len(tx.tags) != 2 || tx.tags[0] != "a" || tx.tags[1] != "b" {
		t.Errorf("reset clobbered tags: got %v, want [a b]", tx.tags)
	}
	// retryCount + backoff are explicitly NOT cleared by internal reset (only
	// user-facing Reset clears them).
	if tx.retryCount != 3 {
		t.Errorf("internal reset must keep retryCount, got %d, want 3", tx.retryCount)
	}
	if tx.backoff != 200*time.Millisecond {
		t.Errorf("internal reset must keep backoff, got %v, want 200ms", tx.backoff)
	}
}

func TestReset_ClearsAccumulatedTagThrottle(t *testing.T) {
	t.Parallel()
	tx := newTestTx()
	tx.proxyTagThrottledDuration = 1.5
	tx.reset()
	if tx.proxyTagThrottledDuration != 0 {
		t.Errorf("reset must clear proxyTagThrottledDuration, got %v", tx.proxyTagThrottledDuration)
	}
}

func TestReset_ReAppliesTimeoutFromCreationTime(t *testing.T) {
	t.Parallel()
	creation := time.Now().Add(-2 * time.Second) // 2s in the past
	tx := newTestTx()
	tx.timeout = 5 * time.Second
	tx.creationTime = creation
	tx.deadline = time.Time{} // wipe to verify reset re-applies it

	tx.reset()

	want := creation.Add(5 * time.Second)
	if !tx.deadline.Equal(want) {
		t.Errorf("deadline: got %v, want %v (creationTime + timeout)", tx.deadline, want)
	}
	// Critical invariant: internal reset does NOT advance creationTime, so the
	// timeout budget is shared across retries (C++ semantics).
	if !tx.creationTime.Equal(creation) {
		t.Errorf("internal reset must NOT advance creationTime: got %v, want %v", tx.creationTime, creation)
	}
}

func TestReset_NoTimeoutLeavesDeadlineUntouched(t *testing.T) {
	t.Parallel()
	tx := newTestTx()
	tx.timeout = 0
	originalDeadline := time.Now().Add(time.Hour) // arbitrary sentinel
	tx.deadline = originalDeadline

	tx.reset()

	if !tx.deadline.Equal(originalDeadline) {
		t.Errorf("timeout=0: deadline must not be touched, got %v want %v", tx.deadline, originalDeadline)
	}
}

func TestUserFacingReset_ClearsRetryStateAndBumpsCreationTime(t *testing.T) {
	t.Parallel()
	tx := newTestTx()
	old := time.Now().Add(-time.Hour)
	tx.creationTime = old
	tx.retryCount = 5
	tx.backoff = time.Second

	tx.Reset()

	if tx.retryCount != 0 || tx.backoff != 0 {
		t.Errorf("user-facing Reset must clear retry state, got retryCount=%d backoff=%v",
			tx.retryCount, tx.backoff)
	}
	if !tx.creationTime.After(old) {
		t.Errorf("user-facing Reset must advance creationTime past old value: got %v want > %v",
			tx.creationTime, old)
	}
}

// ============================================================================
// Cancel — sets terminal state and cancels watch context.
// ============================================================================

func TestCancel_SetsCancelledState(t *testing.T) {
	t.Parallel()
	tx := newTestTx()
	tx.state.Store(int32(txStateActive))
	tx.Cancel()
	if got := txState(tx.state.Load()); got != txStateCancelled {
		t.Errorf("state: got %d, want txStateCancelled", got)
	}
}

func TestCancel_CancelsActiveWatchContext(t *testing.T) {
	t.Parallel()
	tx := newTestTx()
	parent := context.Background()
	wctx := tx.getWatchCtx(parent)
	if err := wctx.Err(); err != nil {
		t.Fatalf("setup: watch ctx should be live, got %v", err)
	}
	tx.Cancel()
	if wctx.Err() == nil {
		t.Error("Cancel must cancel the watch context, but ctx.Err() is still nil")
	}
	// After cancel, getWatchCtx must mint a fresh context (non-nil parent ctx).
	if tx.watchCtx != nil || tx.watchCancel != nil {
		t.Error("Cancel must clear watchCtx/watchCancel so a future Watch can mint a fresh one")
	}
}

func TestGetWatchCtx_LazyAndIdempotent(t *testing.T) {
	t.Parallel()
	tx := newTestTx()
	if tx.watchCtx != nil {
		t.Fatal("setup: watchCtx must start nil")
	}
	a := tx.getWatchCtx(context.Background())
	b := tx.getWatchCtx(context.Background())
	if a != b {
		t.Error("repeated getWatchCtx must return the same context until reset")
	}
}

// ============================================================================
// Helpers — tiny utilities for these tests only. (bytesEqual lives in
// commitpath_unit_test.go and is shared across all client_test files.)
// ============================================================================

// newTestTx returns a Transaction with state.Store explicitly initialised.
// txStateActive is the zero value, so this is a documentation aid more than
// a behaviour aid — it pins the expected initial state at the call site.
func newTestTx() *Transaction {
	tx := &Transaction{}
	tx.state.Store(int32(txStateActive))
	return tx
}
