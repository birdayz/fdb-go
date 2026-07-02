package client

// Deterministic -race regression tests for the "methods safe for concurrent
// use" contract (RFC-049 / RFC-010). These construct a Transaction
// directly (no FDB cluster) and hammer the conflict/mutation buffers from
// multiple goroutines so `go test -race` flags any unsynchronized access.
//
// Each test FAILS on master under -race (the readers/clears it exercises were
// unprotected) and passes after the fix. They are pure-CPU and fast.

import (
	"bytes"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"

	"fdb.dev/pkg/fdbgo/transport"
	"fdb.dev/pkg/fdbgo/wire/types"
)

// TestConcurrent_ConflictReaders_NoRace models the realistic contract case: a
// Get future resolving on one goroutine appends a read conflict (and the caller
// issues Sets) while OTHER goroutines read the same buffers via the public
// GetApproximateSize() and the commit-path marshal. On master these readers
// touch mutations/readConflicts/writeConflicts without conflictMu → data race.
func TestConcurrent_ConflictReaders_NoRace(t *testing.T) {
	t.Parallel()
	tx := newTestTx()
	tx.tenantId = NoTenantID
	tx.rywDisabled = true // isolate the conflict-buffer race from RYW (out of scope, lost-update documented)

	var stop atomic.Bool
	var wg sync.WaitGroup
	val := []byte("v") // read-only, shared safely

	// Writer: read-future role (addReadConflictForKey) + caller role (Set →
	// mutation + write conflict). Both append under conflictMu.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 6000; i++ {
			k := []byte(fmt.Sprintf("k%06d", i))
			tx.addReadConflictForKey(k)
			tx.Set(k, val)
		}
		stop.Store(true)
	}()

	// Readers: the two unprotected-on-master readers, in a tight loop.
	for r := 0; r < 3; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for !stop.Load() {
				_ = tx.GetApproximateSize()
				// Mimic Commit: snapshot the validated mutation set under the
				// lock, then marshal exactly it.
				tx.conflictMu.Lock()
				muts := tx.mutations
				wc := tx.writeConflicts
				tx.conflictMu.Unlock()
				_, bufp := buildCommitTransactionRequest(tx, transport.UID{First: 1, Second: 2}, muts, wc)
				marshalBufPool.Put(bufp)
			}
		}()
	}
	wg.Wait()
}

// TestConcurrent_NextWriteNoConflict_NoRace pins Fix #6: nextWriteNoConflict is
// read AND cleared on the Set path (addWriteConflict*), so two concurrent Sets
// race on it once the one-shot flag is armed. The flag is armed single-threaded
// BEFORE the goroutines launch (it is configure-before-use, out of the operation
// contract), so only the op-vs-op read+write is exercised here.
func TestConcurrent_NextWriteNoConflict_NoRace(t *testing.T) {
	t.Parallel()
	val := []byte("v")
	for round := 0; round < 3000; round++ {
		tx := newTestTx()
		tx.rywDisabled = true
		tx.nextWriteNoConflict = true // armed before any concurrency — no setter race

		var wg sync.WaitGroup
		for g := 0; g < 4; g++ {
			wg.Add(1)
			go func(n int) {
				defer wg.Done()
				tx.Set([]byte(fmt.Sprintf("k%d", n)), val)
			}(g)
		}
		wg.Wait()

		// Exactly one of the concurrent Sets must have consumed the one-shot
		// flag; the rest add a write conflict. So 4 mutations, 3 write conflicts.
		if len(tx.mutations) != 4 {
			t.Fatalf("round %d: mutations=%d, want 4", round, len(tx.mutations))
		}
		if len(tx.writeConflicts) != 3 {
			t.Fatalf("round %d: writeConflicts=%d, want 3 (one Set consumed the no-conflict flag)", round, len(tx.writeConflicts))
		}
		if tx.nextWriteNoConflict {
			t.Fatalf("round %d: nextWriteNoConflict still armed after a Set consumed it", round)
		}
	}
}

// TestConcurrent_ResetWhileSizing_NoRace pins the "[:0] clears moved inside
// conflictMu" change: postCommitReset/reset clear mutations while a concurrent
// Set appends and GetApproximateSize reads. On master the mutations[:0] clear
// sat ABOVE the lock → race. (This is the in-contract Set-vs-Commit-auto-reset /
// Watch-append case; it deliberately does NOT race Reset against a live Commit
// snapshot — that is the out-of-contract case per RFC-049.)
func TestConcurrent_ResetWhileSizing_NoRace(t *testing.T) {
	t.Parallel()
	tx := newTestTx()
	tx.tenantId = NoTenantID
	tx.rywDisabled = true
	val := []byte("v")

	var stop atomic.Bool
	var wg sync.WaitGroup

	// Appender + sizer: keep mutations/conflicts churning.
	for w := 0; w < 2; w++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for i := 0; !stop.Load(); i++ {
				k := []byte(fmt.Sprintf("w%dk%06d", id, i))
				tx.addReadConflictForKey(k)
				tx.Set(k, val)
				_ = tx.GetApproximateSize()
			}
		}(w)
	}

	// Resetter: alternate the two reset paths, each clears all three buffers.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 4000; i++ {
			if i%2 == 0 {
				tx.postCommitReset()
			} else {
				tx.reset(false)
			}
		}
		stop.Store(true)
	}()
	wg.Wait()
}

// TestConcurrent_SetIsAtomic pins that a Set must publish its
// mutation and its write-conflict range as ONE atomic unit. With plain Sets
// (no flags), each Set adds exactly one mutation and one write conflict, so an
// observer snapshotting both counts under conflictMu must NEVER see them differ.
// On the pre-fix code Set appended the mutation and the conflict in two separate
// critical sections, so the observer caught len(mutations)==len(writeConflicts)+1
// mid-Set — meaning a Commit snapshot there would ship a mutation WITHOUT its
// write conflict (a missed conflict). This is a logical-invariant test (no -race
// needed), and the window is hit reliably across 20k Sets.
func TestConcurrent_SetIsAtomic(t *testing.T) {
	t.Parallel()
	tx := newTestTx()
	tx.rywDisabled = true
	val := []byte("v")

	var stop atomic.Bool
	var mismatch atomic.Int64
	var wg sync.WaitGroup

	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 20000; i++ {
			tx.Set([]byte(fmt.Sprintf("k%06d", i)), val)
		}
		stop.Store(true)
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		for !stop.Load() {
			tx.conflictMu.Lock()
			nm := len(tx.mutations)
			nc := len(tx.writeConflicts)
			tx.conflictMu.Unlock()
			if nm != nc {
				mismatch.Add(1)
			}
		}
	}()
	wg.Wait()

	if mismatch.Load() > 0 {
		t.Fatalf("observed %d snapshots where len(mutations) != len(writeConflicts) — "+
			"Set is not atomic; a Commit could ship a mutation without its write-conflict range", mismatch.Load())
	}
}

// TestBuildCommitRequest_TenantNoAlias is the FDB-C reviewer's ask: after the
// snapshot-under-lock change, the tenant path must STILL copy mutation headers
// into a scratch slice before prefixing — it must never write the tenant prefix
// THROUGH the alias into tx.mutations' backing array (RFC-010 #4). Build twice
// and assert the persistent buffer is untouched and not double-prefixed.
func TestBuildCommitRequest_TenantNoAlias(t *testing.T) {
	t.Parallel()
	const tenantID = 7
	tx := newTestTx()
	tx.tenantId = tenantID
	origKey := []byte("appkey")
	origVal := []byte("appval")
	tx.mutations = []Mutation{{Type: MutSetValue, Key: origKey, Value: origVal}}
	tx.writeConflicts = []KeyRange{{Begin: []byte("wk"), End: []byte("wk\x00")}}

	var prefix [8]byte
	prefix[7] = tenantID // 8-byte big-endian tenant id

	for attempt := 0; attempt < 2; attempt++ {
		body, bufp := buildCommitTransactionRequest(tx, transport.UID{First: 1, Second: 2}, tx.mutations, tx.writeConflicts)

		// tx.mutations backing array must be byte-for-byte the originals — the
		// prefix must NOT have been written through the zero-copy alias.
		if !bytes.Equal(tx.mutations[0].Key, origKey) {
			t.Fatalf("attempt %d: tx.mutations[0].Key mutated in place: got %q, want %q", attempt, tx.mutations[0].Key, origKey)
		}
		if !bytes.Equal(tx.mutations[0].Value, origVal) {
			t.Fatalf("attempt %d: tx.mutations[0].Value mutated in place: got %q", attempt, tx.mutations[0].Value)
		}

		// The marshaled request must carry exactly ONE tenant prefix (not zero,
		// not two from a rebuild double-prefixing the aliased buffer).
		var req types.CommitTransactionRequest
		if err := req.UnmarshalFDB(body); err != nil {
			t.Fatalf("attempt %d: UnmarshalFDB: %v", attempt, err)
		}
		marshalBufPool.Put(bufp)
		want := append(append([]byte{}, prefix[:]...), origKey...)
		if len(req.Transaction.Mutations) != 1 || !bytes.Equal(req.Transaction.Mutations[0].Param1, want) {
			t.Fatalf("attempt %d: marshaled key: got %q, want %q (single tenant prefix)", attempt, req.Transaction.Mutations[0].Param1, want)
		}
	}
}

// TestBuildCommitRequest_MarshalsValidatedSnapshot pins the validation-consistency
// fix (FDB-C reviewer): the marshal ships EXACTLY the validated mutation snapshot
// Commit threads in — never a live re-read of tx.mutations. Models a Set that
// landed on another goroutine AFTER Commit validated but BEFORE the marshal: the
// validated snapshot has one mutation, tx.mutations has grown to two; the shipped
// request must carry only the validated one, so a racing unvalidated mutation can
// never reach the commit proxy. (On the pre-fix code the marshal re-snapshotted
// tx.mutations and would ship both.)
func TestBuildCommitRequest_MarshalsValidatedSnapshot(t *testing.T) {
	t.Parallel()
	tx := newTestTx()
	tx.tenantId = NoTenantID
	validated := []Mutation{{Type: MutSetValue, Key: []byte("validated"), Value: []byte("v")}}
	// tx.mutations has grown past the validated snapshot (a concurrent Set landed).
	tx.mutations = append(append([]Mutation{}, validated...),
		Mutation{Type: MutSetValue, Key: []byte("racing-unvalidated"), Value: []byte("v")})

	body, bufp := buildCommitTransactionRequest(tx, transport.UID{First: 1, Second: 2}, validated, tx.writeConflicts)
	defer marshalBufPool.Put(bufp)

	var req types.CommitTransactionRequest
	if err := req.UnmarshalFDB(body); err != nil {
		t.Fatalf("UnmarshalFDB: %v", err)
	}
	if len(req.Transaction.Mutations) != 1 {
		t.Fatalf("marshaled %d mutations, want 1 (only the validated snapshot, not the racing append)", len(req.Transaction.Mutations))
	}
	if string(req.Transaction.Mutations[0].Param1) != "validated" {
		t.Fatalf("marshaled key %q, want \"validated\"", req.Transaction.Mutations[0].Param1)
	}
}
