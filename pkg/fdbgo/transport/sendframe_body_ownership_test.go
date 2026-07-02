package transport

import (
	"context"
	"testing"
	"time"
	"unsafe"
)

// TestSendFrame_PostEnqueueCtxDone_TransportStillOwnsBody pins the transport contract that the
// client-side buffer-pool fix depends on: when SendFrame's FIRST select enqueues the
// frame onto writeCh but its SECOND select then returns via ctx.Done (conn.go:454) instead of errCh,
// the returned error does NOT mean the transport is done with `body` — the enqueued writeReq (which
// writeLoop's WriteFrame reads) still references the SAME backing array. So a caller that owns a
// POOLED body and returns it to the pool on a SendFrame error would hand a still-referenced buffer
// back for reuse → a data race against writeLoop's WriteFrame read. The client fix is: on a SendFrame
// error, DROP the pooled buffer, never Put it (commitpath.go + readpath.go SendFrame callers).
//
// Deterministic: writeCh is buffered with NO writeLoop draining it, so the frame stays enqueued and
// errCh never fires; once the frame is enqueued (len(writeCh)==1) we cancel ctx, forcing the
// post-enqueue ctx.Done return. No writeLoop, no race in the test itself.
func TestSendFrame_PostEnqueueCtxDone_TransportStillOwnsBody(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	c := &Conn{
		ctx:     ctx,
		cancel:  cancel,
		writeCh: make(chan writeReq, 1), // buffered; nothing drains it, so errCh never fires
		pending: make(map[UID]chan Response),
	}
	body := []byte("frame-body-sentinel-owned-by-transport")

	done := make(chan error, 1)
	go func() { done <- c.SendFrame(UID{First: 1, Second: 2}, body) }()

	// Wait until SendFrame's first select has enqueued the frame (writeCh holds it) and it is now
	// parked on the errCh/ctx.Done select. Only then cancel — so we exercise the POST-enqueue ctx.Done
	// path (conn.go:454), not the never-enqueued one (conn.go:440).
	deadline := time.Now().Add(5 * time.Second)
	for len(c.writeCh) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("SendFrame never enqueued the frame")
		}
		time.Sleep(time.Millisecond)
	}
	cancel()

	select {
	case err := <-done:
		if err != errConnClosed {
			t.Fatalf("post-enqueue ctx.Done must return errConnClosed, got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("SendFrame did not return after ctx cancel")
	}

	// The frame is STILL enqueued, and its body is the SAME backing array the caller passed — proving
	// the transport (writeLoop, once it runs) would still read `body` after SendFrame returned the
	// error. Reusing/Put-ting `body` now is exactly the audit-#15 hazard.
	select {
	case req := <-c.writeCh:
		if len(req.body) == 0 || &req.body[0] != &body[0] {
			t.Fatalf("enqueued writeReq must still reference the caller's body backing array (ptr %p vs %p)",
				unsafe.Pointer(&req.body[0]), unsafe.Pointer(&body[0]))
		}
	default:
		t.Fatal("frame was not left enqueued on writeCh — the transport released body before SendFrame returned, " +
			"which would make the client Put-on-error safe (it is not)")
	}
}
