package fdb

import (
	"errors"
	"testing"

	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/client"
)

// newBareOptionsTx builds a facade transaction over a bare client transaction.
// The option setters only mutate the inner transaction's fields, so no database
// or container is needed to exercise them.
func newBareOptionsTx() (Transaction, *client.Transaction) {
	inner := &client.Transaction{}
	// A bare &client.Transaction{} zero-values tenantId to 0 — a VALID tenant id, not "no
	// tenant". Production CreateTransaction sets NoTenantID (-1); mirror that so option setters
	// behave as on a real non-tenant transaction (e.g. SetAccessSystemKeys must not be rejected).
	inner.SetTenantId(client.NoTenantID)
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

// TestSystemKeyOptions_RejectedOnTenant pins the C++ invalid_option (2007) throw when
// READ_SYSTEM_KEYS / ACCESS_SYSTEM_KEYS is set on a tenant transaction
// (NativeAPI.actor.cpp:7163-7170): system-key access implies raw access, which can't be
// tenant-scoped. Go previously set the flags unconditionally — a behavioral divergence. The flags
// must NOT be mutated when rejected, and a non-tenant transaction must still accept the options.
func TestSystemKeyOptions_RejectedOnTenant(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		set  func(o TransactionOptions) error
	}{
		{"access_system_keys", func(o TransactionOptions) error { return o.SetAccessSystemKeys() }},
		{"read_system_keys", func(o TransactionOptions) error { return o.SetReadSystemKeys() }},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tr, inner := newBareOptionsTx()
			inner.SetTenantId(7) // scope to a tenant

			err := tc.set(tr.Options())
			if err == nil {
				t.Fatalf("%s on a tenant transaction = nil, want invalid_option", tc.name)
			}
			var toe *TenantOptionError
			if !errors.As(err, &toe) {
				t.Fatalf("got %T (%v), want *TenantOptionError", err, err)
			}
			if toe.FDBCode() != 2007 {
				t.Errorf("FDBCode = %d, want 2007 (invalid_option)", toe.FDBCode())
			}

			// A non-tenant transaction still accepts the option.
			trNo, _ := newBareOptionsTx() // tenantId = NoTenantID
			if err := tc.set(trNo.Options()); err != nil {
				t.Errorf("%s on a non-tenant transaction = %v, want nil", tc.name, err)
			}
		})
	}
}
