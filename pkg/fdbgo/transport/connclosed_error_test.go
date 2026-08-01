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

// TestConn_AbruptServerCloseDeliversCodedError proves the DELIVERY path: when
// the peer closes mid-RPC, the readLoop's raw I/O error is coded by
// failConnection before fan-out, so the in-flight caller observes the
// request_maybe_delivered teardown — not a bare "EOF"/"use of closed network
// connection" the retry machinery cannot classify. Revert-proof: fan out the
// raw readLoop error (or a bare errors.New sentinel) and the errors.As check
// goes red.
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

	_, rerr := c.SendAndWait(ctx, UID{First: 3}, []byte("req"))
	if rerr == nil {
		t.Fatal("SendAndWait should fail when the server closes mid-RPC")
	}
	if !errors.Is(rerr, ErrConnClosed) {
		t.Fatalf("mid-RPC teardown must match ErrConnClosed via errors.Is; got %v", rerr)
	}
	var fe *wire.FDBError
	if !errors.As(rerr, &fe) || fe.Code != codeRequestMaybeDelivered {
		t.Fatalf("DIVERGENCE from C++ (fdbrpc.h:794-799, waitValueOrSignal on disconnect): "+
			"a mid-RPC teardown must surface FDB code 1030 request_maybe_delivered, "+
			"not an uncoded transport error; got %v", rerr)
	}
}
