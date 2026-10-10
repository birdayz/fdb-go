package client

import (
	"bytes"
	"context"
	"fmt"
	"testing"
	"time"

	"fdb.dev/pkg/fdbgo/wire"
)

func assertConflictRanges(t *testing.T, got []KeyRange, want [][2][]byte) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("conflict ranges: got %d, want %d", len(got), len(want))
	}
	for i, w := range want {
		if !bytes.Equal(got[i].Begin, w[0]) || !bytes.Equal(got[i].End, w[1]) {
			t.Fatalf("conflict range %d: bytes changed (begin %d bytes, end %d bytes)", i, len(got[i].Begin), len(got[i].End))
		}
	}
}

// overwriteFreeConflictBuf hands out and scribbles over the rest of the
// transaction's own buffer, the way the next carve would.
func overwriteFreeConflictBuf(tx *Transaction) {
	tx.conflictMu.Lock()
	fill := tx.conflictBufAlloc(cap(tx.conflictBuf) - len(tx.conflictBuf))
	tx.conflictMu.Unlock()
	for i := range fill {
		fill[i] = 0xEE
	}
}

func rangeOf(b0, b1 byte, n int) [2][]byte {
	return [2][]byte{bytes.Repeat([]byte{b0}, n), bytes.Repeat([]byte{b1}, n)}
}

// A single-key read needs one small conflict range; a fresh transaction must
// not allocate a bulk-sized buffer for it.
func TestConflictBufAlloc_SmallFirstRequestUsesSmallBuffer(t *testing.T) {
	t.Parallel()
	tx := &Transaction{}
	tx.conflictMu.Lock()
	buf := tx.conflictBufAlloc(100)
	got := cap(tx.conflictBuf)
	tx.conflictMu.Unlock()
	if len(buf) != 100 || got < 100 {
		t.Fatalf("len=%d cap=%d, want 100 and >= 100", len(buf), got)
	}
	if got > 512 {
		t.Fatalf("CONFLICTBUF_SMALL_TIER: a fresh 100-byte request took a %d-byte buffer, want <= 512", got)
	}
}

// Overflowing the small buffer goes straight to the bulk tier and leaves the
// ranges already carved from the small buffer intact.
func TestConflictBufAlloc_SmallOverflowPromotesToBulkAndKeepsRanges(t *testing.T) {
	t.Parallel()
	tx := &Transaction{}
	first, second := rangeOf('a', 'b', 100), rangeOf('c', 'd', 300)
	tx.addReadConflicts([][2][]byte{first})
	smallOwner := tx.conflictBufOwner
	tx.addReadConflicts([][2][]byte{second}) // 200 + 600 bytes overflows 512
	if tx.conflictBufOwner == nil || tx.conflictBufOwner == smallOwner {
		t.Fatal("promotion did not transfer ownership to the bulk buffer")
	}
	if got := cap(tx.conflictBuf); got < 32768 {
		t.Fatalf("CONFLICTBUF_PROMOTION: overflow grew to a %d-byte buffer, want >= 32768", got)
	}
	want := [][2][]byte{first, second}
	assertConflictRanges(t, tx.readConflicts, want)
	overwriteFreeConflictBuf(tx)
	assertConflictRanges(t, tx.readConflicts, want)
}

// Growing past the bulk tier keeps every range carved before the growth.
func TestConflictBufAlloc_BulkGrowthKeepsPriorRanges(t *testing.T) {
	t.Parallel()
	tx := &Transaction{conflictBuf: make([]byte, 0, 32768)}
	var want [][2][]byte
	for i := 0; i < 40; i++ { // 40 * 1200 bytes passes 32768
		r := rangeOf(byte(2*i+1), byte(2*i+2), 600)
		want = append(want, r)
		tx.addReadConflicts([][2][]byte{r})
	}
	if got := cap(tx.conflictBuf); got < 40*1200 {
		t.Fatalf("cap=%d, want >= %d", got, 40*1200)
	}
	assertConflictRanges(t, tx.readConflicts, want)
	overwriteFreeConflictBuf(tx)
	assertConflictRanges(t, tx.readConflicts, want)
}

func TestConflictBufAlloc_SmallThenOversizedRequest(t *testing.T) {
	t.Parallel()
	tx := &Transaction{}
	first := rangeOf('a', 'b', 50)
	tx.addReadConflicts([][2][]byte{first})
	second := rangeOf('c', 'd', 50000)
	tx.addReadConflicts([][2][]byte{second})
	if len(tx.conflictBuf) != 100100 || cap(tx.conflictBuf) < 100100 {
		t.Fatalf("oversized promotion: len=%d cap=%d, need 100100 bytes", len(tx.conflictBuf), cap(tx.conflictBuf))
	}
	assertConflictRanges(t, tx.readConflicts, [][2][]byte{first, second})
	overwriteFreeConflictBuf(tx)
	assertConflictRanges(t, tx.readConflicts, [][2][]byte{first, second})
}

// Retry reset lets us overwrite owned storage deterministically without racing
// other users of the pool, unlike postCommitReset.
func TestCaptureCommit_IndependentOfConflictBufReuse(t *testing.T) {
	t.Parallel()
	tx := &Transaction{}
	read := [][2][]byte{rangeOf('a', 'b', 100), rangeOf('c', 'd', 300)} // spans the 512-byte tier
	tx.addReadConflicts(read)
	key := bytes.Repeat([]byte{'w'}, 300)
	tx.addWriteConflictForKey(key)
	write := [][2][]byte{{key, append(append([]byte(nil), key...), 0)}}
	muts := []Mutation{{Type: MutSetValue, Key: key, Value: []byte("v")}}

	lease := tx.enterState()
	input := tx.captureCommit(muts, tx.writeConflicts)
	lease.release()

	tx.reset(false)
	overwriteFreeConflictBuf(tx)

	assertConflictRanges(t, input.readConflicts, read)
	assertConflictRanges(t, input.writeConflicts, write)
	if len(input.muts) != 1 || !bytes.Equal(input.muts[0].Key, key) || string(input.muts[0].Value) != "v" {
		t.Fatalf("captured mutation changed: %+v", input.muts)
	}
}

// commit_unknown_result turns the write conflicts into the retry's read
// conflicts; they must survive the retry carving over the reused buffer.
func TestOnError1021_SelfConflictsSurviveConflictBufReuse(t *testing.T) {
	t.Parallel()
	tx := &Transaction{}
	var want [][2][]byte
	for i := 0; i < 3; i++ { // 3 * 601 bytes overflows 512
		key := bytes.Repeat([]byte{byte('a' + i)}, 300)
		tx.addWriteConflictForKey(key)
		want = append(want, [2][]byte{key, append(append([]byte(nil), key...), 0)})
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := tx.OnError(ctx, &wire.FDBError{Code: 1021}); err != nil {
		t.Fatalf("OnError(1021): %v", err)
	}
	overwriteFreeConflictBuf(tx)
	assertConflictRanges(t, tx.readConflicts, want)
}

func TestConflictBufCancelRetainsOwnedStorage(t *testing.T) {
	t.Parallel()
	tx := &Transaction{}
	key := []byte("cancel-conflict")
	tx.addReadConflictForKey(key)
	owner, backing := tx.conflictBufOwner, &tx.conflictBuf[0]
	tx.Cancel()
	if tx.conflictBufOwner != owner || len(tx.conflictBuf) == 0 || &tx.conflictBuf[0] != backing {
		t.Fatal("Cancel released storage that pending operations may still reference")
	}
	assertConflictRanges(t, tx.readConflicts, [][2][]byte{{key, []byte("cancel-conflict\x00")}})
}

func TestConflictBufPoolSizeClasses(t *testing.T) {
	t.Parallel()
	for _, capacity := range []int{0, 100, 512} {
		if conflictBufPoolFor(capacity) != &smallConflictBufPool {
			t.Errorf("capacity %d did not route to the small pool", capacity)
		}
	}
	for _, capacity := range []int{513, 4096, 32768} {
		if conflictBufPoolFor(capacity) != &conflictBufPool {
			t.Errorf("capacity %d did not route to the bulk pool", capacity)
		}
	}
	for _, capacity := range []int{32769, 65536, 16 << 20} {
		if conflictBufPoolFor(capacity) != nil {
			t.Errorf("CONFLICTBUF_RETENTION: oversized capacity %d must not enter a shared pool", capacity)
		}
	}
}

func TestConflictBufPostCommitResetReleasesBothTiers(t *testing.T) {
	t.Parallel()
	for _, size := range []int{100, 40000} {
		t.Run(fmt.Sprintf("bytes-%d", size), func(t *testing.T) {
			t.Parallel()
			tx := &Transaction{}
			tx.conflictMu.Lock()
			tx.conflictBufAlloc(size)
			tx.conflictMu.Unlock()
			tx.postCommitReset()
			if tx.conflictBuf != nil || tx.conflictBufOwner != nil {
				t.Fatal("committed transaction retained pooled storage")
			}
			tx.addReadConflictForKey([]byte("small"))
			if cap(tx.conflictBuf) > 512 {
				t.Fatalf("CONFLICTBUF_SMALL_TIER: fresh operation after commit took %d bytes", cap(tx.conflictBuf))
			}
		})
	}
}
