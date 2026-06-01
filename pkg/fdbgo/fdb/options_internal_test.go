package fdb

import (
	"testing"

	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/client"
)

// newBareOptionsTx builds a facade transaction over a bare client transaction.
// The option setters only mutate the inner transaction's fields, so no database
// or container is needed to exercise them.
func newBareOptionsTx() (Transaction, *client.Transaction) {
	inner := &client.Transaction{}
	return Transaction{t: &transaction{inner: inner}}, inner
}

// TestSetAccessSystemKeys_DoesNotImplyLockAware pins RFC-010 #10: ACCESS_SYSTEM_KEYS
// must NOT imply lock-aware. In the C client it sets only rawAccess
// (NativeAPI.actor.cpp / ReadYourWrites.actor.cpp); LOCK_AWARE and READ_LOCK_AWARE
// are independent options a caller sets explicitly. The facade previously
// auto-set lock-aware here, diverging from C — this test fails if that coupling
// is reintroduced.
func TestSetAccessSystemKeys_DoesNotImplyLockAware(t *testing.T) {
	t.Parallel()
	tr, inner := newBareOptionsTx()

	if err := tr.Options().SetAccessSystemKeys(); err != nil {
		t.Fatalf("SetAccessSystemKeys: %v", err)
	}
	if inner.LockAware() {
		t.Error("SetAccessSystemKeys set commit lock-aware — diverges from C (ACCESS_SYSTEM_KEYS sets only rawAccess)")
	}
	if inner.ReadLockAware() {
		t.Error("SetAccessSystemKeys set read lock-aware — diverges from C")
	}

	// The two options remain independently settable on the same transaction.
	if err := tr.Options().SetLockAware(); err != nil {
		t.Fatalf("SetLockAware: %v", err)
	}
	if !inner.LockAware() {
		t.Error("SetLockAware() did not set commit lock-aware")
	}
}

// TestSetReadLockAware_Independent confirms READ_LOCK_AWARE affects only the read
// path, never the commit flag — matching C++ (req.options.lockAware on reads,
// tr.lock_aware unchanged on commit).
func TestSetReadLockAware_Independent(t *testing.T) {
	t.Parallel()
	tr, inner := newBareOptionsTx()

	if err := tr.Options().SetReadLockAware(); err != nil {
		t.Fatalf("SetReadLockAware: %v", err)
	}
	if !inner.ReadLockAware() {
		t.Error("SetReadLockAware() did not set read lock-aware")
	}
	if inner.LockAware() {
		t.Error("SetReadLockAware() set commit lock-aware — it must affect reads only")
	}
}
