package client

import (
	"context"
	"testing"
	"time"

	"fdb.dev/pkg/fdbgo/transport"
)

// newAbortTestRead is a sent read whose reply never comes; cancelling its ctx
// is the terminal outcome that completes it.
func newAbortTestRead() (*PendingGet, context.CancelFunc) {
	ctx, cancel := context.WithCancel(context.Background())
	return &PendingGet{
		tx:          &Transaction{},
		flushed:     true, // skip conn.Flush
		ctx:         ctx,
		replyCh:     make(chan transport.Response),
		replyHandle: &transport.ReplyHandle{},
		timer:       getTimer(time.Hour),
	}, cancel
}

// TestPendingGet_AbortHandsOffTheRead pins that an aborted wait returns at once
// while the read still completes, as a cancelled C++ read stays in RYW's
// `reading` (ReadYourWrites.actor.cpp:1690-1691).
func TestPendingGet_AbortHandsOffTheRead(t *testing.T) {
	t.Parallel()
	p, complete := newAbortTestRead()
	abort := make(chan struct{})
	close(abort)
	if _, _, abandoned := p.ResolveOrAbort(abort); !abandoned {
		t.Fatal("an aborted wait must report abandoned")
	}
	complete()
	done := make(chan struct{})
	go func() {
		_, _ = p.Resolve()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the handed-off read never completed")
	}
	if !p.settledFlag.Load() {
		t.Fatal("read not settled after completion")
	}
}

// TestPendingGet_AbortWhileQueuedOnMu pins that a waiter queued behind another
// resolver still wakes on abort, and later sees the settled read.
func TestPendingGet_AbortWhileQueuedOnMu(t *testing.T) {
	t.Parallel()
	p, complete := newAbortTestRead()
	holder := make(chan struct{})
	go func() {
		close(holder)
		_, _ = p.Resolve() // blocks in the reply wait, holding mu
	}()
	<-holder
	for p.mu.TryLock() { // wait until the holder owns mu
		p.mu.Unlock()
		time.Sleep(time.Millisecond)
	}
	abort := make(chan struct{})
	got := make(chan bool, 1)
	go func() {
		_, _, abandoned := p.ResolveOrAbort(abort)
		got <- abandoned
	}()
	close(abort)
	select {
	case abandoned := <-got:
		if !abandoned {
			t.Fatal("abort while queued must report abandoned")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("a waiter queued on mu did not wake on abort")
	}
	complete()
	if _, _, abandoned := p.ResolveOrAbort(make(chan struct{})); abandoned {
		t.Fatal("a settled read must return its memo, not abandon")
	}
}
