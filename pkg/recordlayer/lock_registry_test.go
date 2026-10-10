// Portions derived from FoundationDB Record Layer (LockRegistryTest.java),
// Copyright 2015-2026 Apple Inc. and the FoundationDB project authors
// Licensed under the Apache License, Version 2.0; translated to Go and modified.

package recordlayer

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestLockRegistryRemovesCompletedLocks pins Java 4.14's lock cleanup (#4545,
// LockRegistryTest): a key's entry is gone once its last holder releases, and
// not before. Releasing the newest reader keeps an older reader's lock, and a
// queued writer still excludes a reader that registers after the entry would
// otherwise have been dropped.
func TestLockRegistryRemovesCompletedLocks(t *testing.T) {
	t.Parallel()
	var r lockRegistry

	r.WriteLock("a")
	r.WriteUnlock("a")
	r.ReadLock("a")
	r.ReadUnlock("a")
	if n := r.size(); n != 0 {
		t.Fatalf("%d entries after every lock was released, want 0", n)
	}

	// Two readers: releasing the newer keeps the older one's entry.
	r.ReadLock("k")
	r.ReadLock("k")
	r.ReadUnlock("k")
	if n := r.size(); n != 1 {
		t.Fatalf("%d entries with one reader still holding, want 1", n)
	}

	// A writer queued behind that reader holds the entry too: a reader that
	// arrives after the old reader releases waits for the writer.
	var writerHeld, lateReaderIn atomic.Bool
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		r.WriteLock("k")
		writerHeld.Store(true)
		time.Sleep(20 * time.Millisecond)
		if lateReaderIn.Load() {
			t.Error("a reader entered while the writer held the lock")
		}
		writerHeld.Store(false)
		r.WriteUnlock("k")
	}()
	waitFor(t, func() bool { return lockRefs(&r, "k") == 2 })
	r.ReadUnlock("k")
	waitFor(t, writerHeld.Load)
	r.ReadLock("k")
	lateReaderIn.Store(true)
	if writerHeld.Load() {
		t.Error("the late reader got the lock while the writer held it")
	}
	r.ReadUnlock("k")
	wg.Wait()
	if n := r.size(); n != 0 {
		t.Fatalf("%d entries after the writer and readers released, want 0", n)
	}
}

func lockRefs(r *lockRegistry, key string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	if e, ok := r.locks[key]; ok {
		return e.refs
	}
	return 0
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not reached")
		}
		time.Sleep(time.Millisecond)
	}
}
