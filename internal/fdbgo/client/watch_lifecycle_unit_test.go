package client

import (
	"context"
	"sync"
	"testing"
)

func TestWatchCommitHandoffPreservesIndependentCancellation(t *testing.T) {
	t.Parallel()
	for _, version := range []int64{-1, 123} {
		tx := &Transaction{}
		parent, cancelParent := context.WithCancel(context.Background())
		a, cancelA := tx.newWatchCtx(parent)
		b, cancelB := tx.newWatchCtx(parent)
		activation := tx.WatchActivationFor(a)
		tx.fireWatchActivation(version, false)
		tx.postCommitReset()
		if activation.abandoned || activation.version != version {
			t.Fatalf("commit activation: abandoned=%t version=%d", activation.abandoned, activation.version)
		}
		tx.watchMu.Lock()
		registered := len(tx.watchCancels)
		tx.watchMu.Unlock()
		if registered != 0 {
			t.Fatalf("commit retained %d cancellation registrations", registered)
		}
		pending, cancelPending := tx.newWatchCtx(parent)
		tx.reset(false)
		if pending.Err() == nil || a.Err() != nil || b.Err() != nil {
			t.Fatalf("retry cancelled wrong generation: pending=%v a=%v b=%v", pending.Err(), a.Err(), b.Err())
		}
		cancelPending()
		cancelA()
		cancelA()
		if a.Err() == nil || b.Err() != nil {
			t.Fatalf("scoped cancel after handoff: a=%v b=%v", a.Err(), b.Err())
		}
		tx.Cancel()
		if b.Err() != nil {
			t.Fatalf("transaction Cancel reached committed watch: %v", b.Err())
		}
		cancelParent()
		if b.Err() == nil {
			t.Fatal("handoff detached the watch from its caller context")
		}
		cancelB()
	}
}

func TestWatchCommitHandoffCancellationRaces(t *testing.T) {
	t.Parallel()
	for range 200 {
		tx := &Transaction{}
		a, cancelA := tx.newWatchCtx(context.Background())
		b, cancelB := tx.newWatchCtx(context.Background())
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); tx.fireWatchActivation(123, false) }()
		go func() { defer wg.Done(); cancelA() }()
		wg.Wait()
		if a.Err() == nil || b.Err() != nil {
			t.Fatalf("handoff vs scoped cancel: a=%v b=%v", a.Err(), b.Err())
		}
		tx.Reset()
		if b.Err() != nil {
			t.Fatal("Reset reached handed-off watch")
		}
		cancelB()
		if b.Err() == nil {
			t.Fatal("scoped cancel lost ownership during handoff")
		}

		tx = &Transaction{}
		watch, cancel := tx.newWatchCtx(context.Background())
		activation := tx.WatchActivationFor(watch)
		wg.Add(2)
		go func() { defer wg.Done(); tx.fireWatchActivation(123, false) }()
		go func() { defer wg.Done(); tx.cancelWatches() }()
		wg.Wait()
		if activation.abandoned != (watch.Err() != nil) {
			t.Fatalf("handoff vs teardown: abandoned=%t error=%v", activation.abandoned, watch.Err())
		}
		cancel()
	}
}
