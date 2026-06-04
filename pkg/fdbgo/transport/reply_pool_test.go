package transport

import (
	"testing"
)

// observePool swaps the package-level putReplyChannel seam for the duration of a
// test, recording every channel returned to the pool, and restores it on cleanup.
// Tests using it deliberately do NOT call t.Parallel(): they mutate shared
// package state, so they must run serially (the only exception to the
// project-wide t.Parallel() rule — they touch no FDB/Docker and are microseconds).
func observePool(t *testing.T) *[]chan Response {
	t.Helper()
	orig := putReplyChannel
	pooled := &[]chan Response{}
	putReplyChannel = func(ch chan Response) {
		*pooled = append(*pooled, ch)
		orig(ch)
	}
	t.Cleanup(func() { putReplyChannel = orig })
	return pooled
}

func newTestConn() *Conn {
	return &Conn{pending: make(map[UID]chan Response)}
}

// TestReplyHandle_Cancel_WonRace_PoolsChannel: when Cancel runs before readLoop
// delivers (token still pending), the channel is clean and returned to the pool.
func TestReplyHandle_Cancel_WonRace_PoolsChannel(t *testing.T) {
	pooled := observePool(t)
	c := newTestConn()

	_, _, h := c.PrepareReply()
	bidi := h.ch // capture before Cancel nils it
	h.Cancel()

	if len(*pooled) != 1 || (*pooled)[0] != bidi {
		t.Fatalf("Cancel (won race) must pool the channel exactly once; pooled=%d", len(*pooled))
	}
	if len(c.pending) != 0 {
		t.Errorf("Cancel must remove the pending token; pending=%d", len(c.pending))
	}
	if h.ch != nil {
		t.Errorf("Cancel must nil h.ch to mark it handled")
	}
	h.Release()
	if len(*pooled) != 1 {
		t.Errorf("Release after Cancel must NOT re-pool the channel; pooled=%d", len(*pooled))
	}
}

// TestReplyHandle_Cancel_LostRace_LeaksChannel: when readLoop already delivered
// (token gone, value buffered), Cancel must NOT pool the channel — a future
// request must never read its stale buffered value.
func TestReplyHandle_Cancel_LostRace_LeaksChannel(t *testing.T) {
	pooled := observePool(t)
	c := newTestConn()

	token, _, h := c.PrepareReply()
	bidi := h.ch
	// Simulate readLoop winning: delete the token and deliver a (stale) reply.
	c.pendingMu.Lock()
	delete(c.pending, token)
	c.pendingMu.Unlock()
	bidi <- Response{Body: []byte("stale")}

	h.Cancel()
	if len(*pooled) != 0 {
		t.Fatalf("Cancel (lost race) must NOT pool the channel (stale buffered value); pooled=%d", len(*pooled))
	}
	if h.ch != nil {
		t.Errorf("Cancel must nil h.ch even on the lost race")
	}
	h.Release()
	if len(*pooled) != 0 {
		t.Errorf("Release after a lost-race Cancel must NOT pool the channel; pooled=%d", len(*pooled))
	}
}

// TestReplyHandle_Release_Success_PoolsChannel: on the success path (reply
// received, no Cancel), Release returns the channel to the pool — the leak the
// false "readLoop returns it" comment hid.
func TestReplyHandle_Release_Success_PoolsChannel(t *testing.T) {
	pooled := observePool(t)
	c := newTestConn()

	token, ch, h := c.PrepareReply()
	bidi := h.ch
	// Simulate readLoop delivering, then the caller receiving its reply.
	c.pendingMu.Lock()
	delete(c.pending, token)
	c.pendingMu.Unlock()
	bidi <- Response{Body: []byte("ok")}
	<-ch // caller receives via the returned receive-only view

	h.Release()
	if len(*pooled) != 1 || (*pooled)[0] != bidi {
		t.Fatalf("Release on the success path must pool the channel exactly once; pooled=%d", len(*pooled))
	}
	if h.conn != nil || h.ch != nil {
		t.Errorf("Release must reset the handle")
	}
}

// TestConn_cancelPending_Discipline: SendAndWait's timeout path (cancelPending)
// follows the same won/lost-race discipline — pool only when it beat delivery.
func TestConn_cancelPending_Discipline(t *testing.T) {
	pooled := observePool(t)
	c := newTestConn()

	// Won race: token still pending → pool it.
	token, _, h := c.PrepareReply()
	c.cancelPending(token, h.ch)
	if len(*pooled) != 1 {
		t.Fatalf("cancelPending (won) must pool once; pooled=%d", len(*pooled))
	}

	// Lost race: token already gone (readLoop delivered) → do NOT pool.
	token2, _, h2 := c.PrepareReply()
	c.pendingMu.Lock()
	delete(c.pending, token2)
	c.pendingMu.Unlock()
	h2.ch <- Response{Body: []byte("stale")}
	c.cancelPending(token2, h2.ch)
	if len(*pooled) != 1 {
		t.Errorf("cancelPending (lost) must NOT pool the stale channel; pooled=%d", len(*pooled))
	}
}
