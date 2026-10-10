package client

import (
	"bytes"
	"fmt"
	"sync"
	"testing"
	"unsafe"

	"fdb.dev/pkg/fdbgo/transport"
)

var borrowReplyToken = transport.UID{First: 7, Second: 9}

// borrowTx buffers every operand kind; 50 Sets promote it to the bulk slab.
func borrowTx(n int, fill byte) *Transaction {
	tx := &Transaction{}
	tx.tenantId = NoTenantID
	tx.readVersion = 42
	for i := range n {
		tx.Set([]byte(fmt.Sprintf("borrow/key/%04d", i)), bytes.Repeat([]byte{fill}, 100))
	}
	tx.Clear([]byte("borrow/clear"))
	if err := tx.ClearRange([]byte("borrow/range/a"), []byte("borrow/range/z")); err != nil {
		panic(err)
	}
	tx.Atomic(MutAddValue, []byte("borrow/counter"), []byte{fill, 0, 0, 0})
	tx.AddReadConflictKey([]byte("borrow/read"))
	return tx
}

func captureUnderLease(tx *Transaction) *commitInput {
	lease := tx.enterState()
	defer lease.release()
	tx.conflictMu.Lock()
	muts, writes := tx.mutations, tx.writeConflicts
	tx.conflictMu.Unlock()
	return tx.captureCommit(muts, writes)
}

// deepCopyInput is the pre-borrow capture: every operand in fresh storage.
func deepCopyInput(in *commitInput) *commitInput {
	out := *in
	out.slab = nil
	out.muts = make([]Mutation, len(in.muts))
	for i, m := range in.muts {
		out.muts[i] = Mutation{Type: m.Type, Key: bytes.Clone(m.Key), Value: bytes.Clone(m.Value)}
	}
	cloneRanges := func(rs []KeyRange) []KeyRange {
		c := make([]KeyRange, len(rs))
		for i, r := range rs {
			c[i] = KeyRange{Begin: bytes.Clone(r.Begin), End: bytes.Clone(r.End)}
		}
		return c
	}
	out.readConflicts = cloneRanges(in.readConflicts)
	out.writeConflicts = cloneRanges(in.writeConflicts)
	return &out
}

func commitFrame(in *commitInput) []byte {
	body, poolBuf := buildCommitInputRequest(in, borrowReplyToken)
	frame := bytes.Clone(body)
	marshalBufPool.Put(poolBuf)
	return frame
}

func TestCaptureCommitBorrowsSlabInsteadOfCopying(t *testing.T) {
	t.Parallel()
	tx := borrowTx(50, 'a')
	defer tx.Cancel()
	input := captureUnderLease(tx)
	if input.slab == nil || input.slab != tx.conflictBufOwner || input.slab.borrows.Load() != 1 {
		t.Fatal("capture did not borrow the current slab")
	}
	for i, m := range input.muts {
		own := tx.mutations[i]
		if unsafe.SliceData(m.Key) != unsafe.SliceData(own.Key) || unsafe.SliceData(m.Value) != unsafe.SliceData(own.Value) {
			t.Fatalf("mutation %d was copied instead of borrowed", i)
		}
	}
	if &input.muts[0] == &tx.mutations[0] || &input.writeConflicts[0] == &tx.writeConflicts[0] {
		t.Fatal("capture shares a vector a retry refills in place")
	}
	if owned := tx.captureCommitVectors(input.muts, true, nil); &owned.muts[0] != &input.muts[0] {
		t.Fatal("an owned mutation vector was copied again")
	} else {
		owned.release()
	}
	owner := input.slab
	input.release()
	input.release()
	if owner.borrows.Load() != 0 || input.slab != nil {
		t.Fatalf("release left %d borrows", owner.borrows.Load())
	}
}

// A retry may carve over its own slab only when no captured commit reads it.
func TestRetryReusesSlabOnlyWhenSoleHolder(t *testing.T) {
	t.Parallel()
	for _, released := range []bool{false, true} {
		t.Run(fmt.Sprintf("released=%t", released), func(t *testing.T) {
			t.Parallel()
			tx := borrowTx(50, 'a')
			defer tx.Cancel()
			backing := unsafe.SliceData(tx.conflictBuf)
			input := captureUnderLease(tx)
			want := commitFrame(deepCopyInput(input))
			if released {
				input.release()
			}
			tx.reset(false)
			for i := range 50 {
				tx.Set([]byte(fmt.Sprintf("borrow/key/%04d", i)), bytes.Repeat([]byte{'z'}, 100))
			}
			reused := unsafe.SliceData(tx.conflictBuf) == backing
			if reused != released {
				t.Fatalf("retry reused the slab=%t, want %t", reused, released)
			}
			if released {
				return
			}
			if got := commitFrame(input); !bytes.Equal(got, want) {
				t.Fatal("COMMIT_CAPTURE_MUTATED: captured frame changed after retry carved new writes")
			}
			input.release()
		})
	}
}

// Reset checks the borrow on the handle, so capturing an ownerless slab must
// install one; otherwise post-commit reset would pool it under a fresh handle.
func TestCaptureCommitTracksOwnerlessSlab(t *testing.T) {
	t.Parallel()
	tx := &Transaction{}
	defer tx.Cancel()
	tx.conflictBuf = make([]byte, 0, 4096)
	tx.Set([]byte("ownerless"), []byte("value"))
	input := captureUnderLease(tx)
	defer input.release()
	tx.conflictMu.Lock()
	defer tx.conflictMu.Unlock()
	if input.slab == nil || tx.conflictBufOwner != input.slab {
		t.Fatal("capture of an ownerless slab did not record its borrow")
	}
	if tx.dropConflictBufLocked() {
		t.Fatal("COMMIT_CAPTURE_POOLED: post-commit reset pooled the borrowed ownerless slab")
	}
}

// Post-commit reset pools the slab only once no captured commit reads it.
func TestPostCommitResetPoolsSlabOnlyWhenSoleHolder(t *testing.T) {
	t.Parallel()
	for _, released := range []bool{false, true} {
		tx := borrowTx(50, 'a')
		input := captureUnderLease(tx)
		if released {
			input.release()
		}
		tx.conflictMu.Lock()
		pooled := tx.dropConflictBufLocked()
		tx.conflictMu.Unlock()
		if pooled != released {
			t.Fatalf("COMMIT_CAPTURE_POOLED: released=%t pooled=%t", released, pooled)
		}
		input.release()
		tx.Cancel()
	}
}

// Captured bounds are capacity-capped: an append to one must reallocate
// rather than overwrite its neighbour in the shared slab.
func TestConflictBoundsAreCapacityCapped(t *testing.T) {
	t.Parallel()
	tx := borrowTx(2, 'a')
	defer tx.Cancel()
	_ = tx.AddReadConflictRange([]byte("cap/a"), []byte("cap/z"))
	for _, rs := range [][]KeyRange{tx.readConflicts, tx.writeConflicts} {
		for i, r := range rs {
			if cap(r.Begin) != len(r.Begin) || cap(r.End) != len(r.End) {
				t.Fatalf("range %d: begin %d/%d end %d/%d (len/cap)", i, len(r.Begin), cap(r.Begin), len(r.End), cap(r.End))
			}
		}
	}
}

// Resets race the commit's encoding as a caller may during the RPC; under -race
// any write to bytes the commit still reads is reported, not only a mismatch.
func TestConcurrentResetDuringCommitKeepsCapturedBytes(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"Reset", "retry", "postcommit"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			tx := borrowTx(50, 'a')
			input := captureUnderLease(tx)
			want := commitFrame(deepCopyInput(input))
			var wg sync.WaitGroup
			wg.Add(2)
			go func() {
				defer wg.Done()
				for range 20 {
					if got := commitFrame(input); !bytes.Equal(got, want) {
						t.Error("COMMIT_CAPTURE_MUTATED: frame changed during concurrent reset")
						return
					}
				}
			}()
			go func() {
				defer wg.Done()
				switch mode {
				case "Reset":
					tx.Reset()
				case "retry":
					tx.reset(false)
				case "postcommit":
					tx.postCommitReset()
				}
				for i := range 50 {
					tx.Set([]byte(fmt.Sprintf("borrow/key/%04d", i)), bytes.Repeat([]byte{'z'}, 100))
				}
			}()
			wg.Wait()
			input.release()
			tx.Cancel()
		})
	}
}

// commitAdmitted hands a coalesced vector to the capture uncopied, so coalescing
// must never return the transaction's own op-log vector, which a retry refills.
func TestCoalescedMutationsNeverAliasTheOpLog(t *testing.T) {
	t.Parallel()
	for _, n := range []int{1, 2, 50} {
		tx := borrowTx(n, 'a')
		for _, muts := range [][]Mutation{tx.mutations, tx.mutations[:n]} {
			ship, _, coalesced := coalesceCommitVectors(muts, tx.writeConflicts, false)
			if !coalesced || len(ship) == 0 {
				t.Fatalf("n=%d: precondition: want a coalesced vector", n)
			}
			if unsafe.SliceData(ship) == unsafe.SliceData(muts) {
				t.Fatalf("n=%d: coalesced mutations alias the op-log vector", n)
			}
		}
		tx.Cancel()
	}
}
