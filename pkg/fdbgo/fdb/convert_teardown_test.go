package fdb

import (
	"errors"
	"testing"

	"fdb.dev/pkg/fdbgo/transport"
	"fdb.dev/pkg/fdbgo/wire"
)

// TestConvertError_ConnTeardownShape pins the EXACT shape a transport
// connection teardown has after crossing the fdb facade: convertError rebuilds
// any error carrying a *wire.FDBError as the VALUE type Error{Code}, so the
// coded teardown (ConnClosedError, request_maybe_delivered 1030) becomes
// Error{Code: 1030} — and the wrap chain, including the errors.Is identity to
// transport.ErrConnClosed, does NOT survive. Downstream classifiers (the
// conformance isConnTeardown arm) depend on both halves of this fact: the 1030
// code is the only structural handle post-facade, and sentinel identity alone
// is unsatisfiable there. If convertError ever starts preserving the chain (or
// stops coding the teardown), this test fails and the classifier contract must
// be revisited together with it.
func TestConvertError_ConnTeardownShape(t *testing.T) {
	t.Parallel()

	// The real transport teardown type, exactly as failAllPending delivers it
	// to an in-flight RPC (transport/conn.go) — not a synthetic stand-in.
	teardown := error(&transport.ConnClosedError{})
	if !errors.Is(teardown, transport.ErrConnClosed) {
		t.Fatalf("precondition: transport teardown must match its sentinel; got %v", teardown)
	}

	converted := convertError(teardown)
	fe, ok := converted.(Error)
	if !ok {
		t.Fatalf("convertError must rebuild a coded teardown as the value type fdb.Error; got %T: %v", converted, converted)
	}
	if fe.Code != 1030 {
		t.Fatalf("post-facade teardown must carry request_maybe_delivered (1030); got %d", fe.Code)
	}
	// The facade drops the wrap chain — pin it, because the conformance
	// classifier's post-facade arm exists precisely because of this loss.
	if errors.Is(converted, transport.ErrConnClosed) {
		t.Fatalf("facade conversion preserved sentinel identity — the classifier's post-facade arm doc is now stale: %v", converted)
	}
	var wireErr *wire.FDBError
	if errors.As(converted, &wireErr) {
		t.Fatalf("facade conversion preserved *wire.FDBError — the classifier's post-facade arm doc is now stale: %v", converted)
	}
}
