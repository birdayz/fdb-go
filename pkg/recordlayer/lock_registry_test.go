// Portions derived from FoundationDB Record Layer (LockRegistryTest.java),
// Copyright 2015-2026 Apple Inc. and the FoundationDB project authors
// Licensed under the Apache License, Version 2.0; translated to Go and modified.

package recordlayer

import (
	"sync"
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
	oldReaderHeld := true

	// A failed assertion must release both gates before waiting for workers.
	var (
		wg            sync.WaitGroup
		releaseOnce   sync.Once
		releaseWriter = make(chan struct{})
	)
	release := func() { releaseOnce.Do(func() { close(releaseWriter) }) }
	t.Cleanup(func() {
		if oldReaderHeld {
			r.ReadUnlock("k")
		}
		release()
		select {
		case <-lockRegistryWorkersDone(&wg):
		case <-time.After(5 * time.Second):
			t.Error("lock goroutines still blocked after cleanup: the registry deadlocked")
		}
	})

	if n := r.size(); n != 1 {
		t.Fatalf("%d entries with one reader still holding, want 1", n)
	}
	entry := lockEntryOf(&r, "k")

	// A writer queued behind that reader holds the entry too.
	writerIn := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		r.WriteLock("k")
		close(writerIn)
		<-releaseWriter
		r.WriteUnlock("k")
	}()
	// Stable: the old reader keeps the writer from finishing.
	waitFor(t, func() bool { return lockRefs(&r, "k") == 2 })

	// The old reader releases; the writer gets the same lock, not a new one.
	r.ReadUnlock("k")
	oldReaderHeld = false
	awaitLockRegistry(t, writerIn, "the writer acquiring k after the old reader released")
	if got := lockEntryOf(&r, "k"); got != entry {
		t.Fatalf("k's entry was replaced while the writer waited on it (%p, want %p)", got, entry)
	}

	// A reader that arrives now registers on that entry and waits for the
	// writer.
	lateReaderIn := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		r.ReadLock("k")
		close(lateReaderIn)
		r.ReadUnlock("k")
	}()
	// Stable: the writer holds k until release().
	waitFor(t, func() bool { return lockRefs(&r, "k") == 2 })
	if got := lockEntryOf(&r, "k"); got != entry {
		t.Fatalf("the late reader registered a second lock for k (%p, want %p)", got, entry)
	}
	select {
	case <-lateReaderIn:
		t.Fatal("the late reader got the lock while the writer held it")
	default:
	}

	// Release the writer; the late reader gets in, and both finish.
	release()
	awaitLockRegistry(t, lateReaderIn, "the late reader acquiring k after the writer released")
	awaitLockRegistry(t, lockRegistryWorkersDone(&wg), "the writer and the late reader releasing k")
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

// lockEntryOf is key's current entry, nil when there is none.
func lockEntryOf(r *lockRegistry, key string) *lockEntry {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.locks[key]
}

// A polled condition must stay true until the test changes it; scheduler
// delays can otherwise miss a transient state.
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

// The deadline only turns a missing signal into a failure.
func awaitLockRegistry(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
}

func lockRegistryWorkersDone(wg *sync.WaitGroup) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	return done
}
