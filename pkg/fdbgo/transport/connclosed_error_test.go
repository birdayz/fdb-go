package transport

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"fdb.dev/pkg/fdbgo/wire"
)

// TestConnClosedError_CarriesRequestMaybeDelivered pins the coded shape of the
// teardown error. C++ delivers request_maybe_delivered (1030,
// flow/error_definitions.h:57) to an in-flight unreliable RPC whose peer
// connection dies (fdbrpc.h tryGetReply waits on
// IFailureMonitor::onDisconnectOrFailure and completes with
// request_maybe_delivered(), fdbrpc.h:794-799); a bare uncoded sentinel is the
// divergence this repo shipped — the record layer's OnError/Transact loop
// cannot classify it and a transient teardown flattens to a terminal error.
func TestConnClosedError_CarriesRequestMaybeDelivered(t *testing.T) {
	t.Parallel()

	// The shared no-cause value: sentinel identity AND FDB code 1030.
	if !errors.Is(errConnClosed, ErrConnClosed) {
		t.Fatalf("errConnClosed must match the ErrConnClosed sentinel via errors.Is")
	}
	var fe *wire.FDBError
	if !errors.As(errConnClosed, &fe) || fe.Code != codeRequestMaybeDelivered {
		t.Fatalf("DIVERGENCE from C++ (fdbrpc.h:794-799): the teardown error must carry "+
			"FDB code 1030 request_maybe_delivered; got %v", errConnClosed)
	}

	// A raw teardown cause (readLoop I/O error) gets coded, keeps the cause in
	// the message, and does NOT expose the cause in the Unwrap chain (a teardown
	// must never spuriously match probes aimed at its cause).
	coded := codedConnClosed(io.ErrUnexpectedEOF)
	if !errors.Is(coded, ErrConnClosed) {
		t.Fatalf("coded teardown must keep sentinel identity; got %v", coded)
	}
	fe = nil
	if !errors.As(coded, &fe) || fe.Code != codeRequestMaybeDelivered {
		t.Fatalf("coded teardown must carry 1030; got %v", coded)
	}
	if errors.Is(coded, io.ErrUnexpectedEOF) {
		t.Fatalf("the raw cause must be message-only, not in the Unwrap chain: %v", coded)
	}

	// Idempotent: an already-coded teardown passes through unchanged.
	if again := codedConnClosed(coded); again != coded {
		t.Fatalf("codedConnClosed must be idempotent; %v != %v", again, coded)
	}
	if codedConnClosed(nil) != errConnClosed {
		t.Fatal("codedConnClosed(nil) must return the shared errConnClosed value")
	}
}

// TestConn_AbruptServerCloseDeliversCodedError proves the DELIVERY (fan-out)
// path deterministically: when the peer closes mid-RPC, the readLoop's raw I/O
// error is coded by failConnection before failAllPending fans it out, so the
// in-flight reply channel observes the request_maybe_delivered teardown — not
// a bare "EOF"/"use of closed network connection" the retry machinery cannot
// classify.
//
// The test reads the REPLY CHANNEL directly (PrepareReply + SendFrame) rather
// than going through SendAndWait: SendAndWait races its replyCh arm against
// its conn-ctx.Done arm, and the ctx.Done arm returns the shared (always
// coded) errConnClosed — so via SendAndWait a fan-out regression hides behind
// whichever arm wins the race. The reply channel has exactly one writer for
// this token (failAllPending), so what arrives here IS the fan-out value.
// Revert-proof: fan out the raw readLoop error (failAllPending(err) without
// codedConnClosed) and the errors.As check goes red every run.
func TestConn_AbruptServerCloseDeliversCodedError(t *testing.T) {
	t.Parallel()
	s := newSimServer(t)
	go func() {
		if err := s.handshake(); err != nil {
			return
		}
		// Read the request frame, then close abruptly without replying.
		var fr FrameReader
		_, _, _ = fr.Read(s.srv, false)
		s.srv.Close()
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, err := dialWith(ctx, "sim", s.dialFunc(), nil, withMonitorCadence(time.Hour, time.Hour))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()

	_, replyCh, replyHandle := c.PrepareReply()
	// Not-received outcomes must Cancel() before Release() (see ReplyHandle);
	// on the received path failAllPending already removed the pending token.
	received := false
	defer func() {
		if !received {
			replyHandle.Cancel()
		}
		replyHandle.Release()
	}()
	// SendFrame may itself lose a race against the teardown (its errCh arm vs
	// the conn-ctx.Done arm) and report the teardown instead of nil — both are
	// fine here: the reply token is already pending, so failAllPending fans out
	// to replyCh either way, and replyCh is the assertion target.
	if err := c.SendFrame(UID{First: 3}, []byte("req")); err != nil && !errors.Is(err, ErrConnClosed) {
		t.Fatalf("SendFrame: %v", err)
	}

	select {
	case resp := <-replyCh:
		received = true
		if resp.Err == nil {
			t.Fatalf("got a reply body %q; expected a teardown error", resp.Body)
		}
		if !errors.Is(resp.Err, ErrConnClosed) {
			t.Fatalf("fan-out teardown must match ErrConnClosed via errors.Is; got %v", resp.Err)
		}
		var fe *wire.FDBError
		if !errors.As(resp.Err, &fe) || fe.Code != codeRequestMaybeDelivered {
			t.Fatalf("DIVERGENCE from C++ (fdbrpc.h:794-799, waitValueOrSignal on disconnect): "+
				"the fan-out must deliver FDB code 1030 request_maybe_delivered, "+
				"not an uncoded transport error; got %v", resp.Err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("in-flight reply was not failed after abrupt server close")
	}
}
