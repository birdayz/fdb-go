package fdb_test

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"fdb.dev/pkg/fdbgo/fdb"
	tcfdb "fdb.dev/pkg/testcontainers/foundationdb"
	. "github.com/onsi/gomega"
)

// openTestDBWithTenants starts a container with tenant_mode=optional_experimental.
func openTestDBWithTenants(t *testing.T) (fdb.Database, *tcfdb.Container) {
	t.Helper()
	fdb.MustAPIVersion(730)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	container, err := tcfdb.Run(ctx, "")
	if err != nil {
		t.Fatalf("start FDB container: %v", err)
	}

	err = container.InitializeDatabase(ctx)
	if err != nil {
		t.Fatalf("init db: %v", err)
	}

	path, err := container.ClusterFilePath(ctx)
	if err != nil {
		t.Fatalf("cluster file: %v", err)
	}

	db, err := fdb.OpenDatabase(path)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db, container
}

// TestTenant_CreateTransaction_AppliesDatabaseDefaults verifies a tenant-scoped
// MANUALLY-created transaction inherits database-level option defaults — the same
// applyTxDefaults parity the Database facade has across Transact/ReadTransact/
// CreateTransaction. Regression for the divergence where the whole Tenant facade
// skipped applyTxDefaults, so tenant transactions ignored SetTransactionTimeout.
func TestTenant_CreateTransaction_AppliesDatabaseDefaults(t *testing.T) {
	t.Parallel()
	db, _ := openTestDBWithTenants(t)

	name := fdb.Key(t.Name())
	if err := db.CreateTenant(name); err != nil {
		t.Fatalf("CreateTenant: %v", err)
	}
	tenant, err := db.OpenTenant(name)
	if err != nil {
		t.Fatalf("OpenTenant: %v", err)
	}

	// 1ms database-level timeout — must apply to a tenant-scoped manual transaction.
	if err := db.Options().SetTransactionTimeout(1); err != nil {
		t.Fatalf("SetTransactionTimeout: %v", err)
	}
	defer db.Options().SetTransactionTimeout(0)

	is1031 := func(err error) bool {
		fe, ok := err.(fdb.Error)
		return ok && fe.Code == 1031 // transaction_timed_out
	}
	var timedOut bool
	for i := 0; i < 100; i++ {
		tr, err := tenant.CreateTransaction()
		if err != nil {
			t.Fatalf("tenant.CreateTransaction: %v", err)
		}
		_, rerr := tr.Get(fdb.Key("k")).Get()
		cerr := tr.Commit().Get()
		if is1031(rerr) || is1031(cerr) {
			timedOut = true
			break
		}
	}
	if !timedOut {
		t.Fatal("tenant CreateTransaction did not inherit the 1ms database timeout (1031 expected) — applyTxDefaults not applied")
	}
}

// TestTenant_Reset_PreservesTenantScoping verifies a tenant transaction stays
// scoped to its tenant after Reset() — C++ reset() preserves the tenant. The
// facade's prior fresh-inner Reset silently dropped tenant scoping, so a reset
// tenant transaction wrote the DEFAULT keyspace (data corruption). Regression for
// that wrong-keyspace divergence (Torvalds).
func TestTenant_Reset_PreservesTenantScoping(t *testing.T) {
	t.Parallel()
	db, _ := openTestDBWithTenants(t)

	name := fdb.Key(t.Name())
	if err := db.CreateTenant(name); err != nil {
		t.Fatalf("CreateTenant: %v", err)
	}
	tenant, err := db.OpenTenant(name)
	if err != nil {
		t.Fatalf("OpenTenant: %v", err)
	}

	key := fdb.Key("scoped-key")
	want := []byte("tenant-value")

	// Write via a tenant tx that is Reset() BEFORE the write — the write must still
	// land in the tenant's keyspace, not the default one.
	tr, err := tenant.CreateTransaction()
	if err != nil {
		t.Fatalf("tenant.CreateTransaction: %v", err)
	}
	tr.Reset()
	tr.Set(key, want)
	if err := tr.Commit().Get(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	// Read back THROUGH THE TENANT — must see it.
	got, err := tenant.ReadTransact(func(rt fdb.ReadTransaction) (any, error) {
		return rt.Get(key).MustGet(), nil
	})
	if err != nil {
		t.Fatalf("tenant read: %v", err)
	}
	if gb, _ := got.([]byte); string(gb) != string(want) {
		t.Fatalf("tenant read = %q, want %q — Reset dropped tenant scoping?", gb, want)
	}

	// The DEFAULT keyspace must NOT have it — proves the reset tenant tx stayed
	// tenant-scoped instead of leaking the write to the default keyspace.
	leaked, err := db.ReadTransact(func(rt fdb.ReadTransaction) (any, error) {
		return rt.Get(key).MustGet(), nil
	})
	if err != nil {
		t.Fatalf("default read: %v", err)
	}
	if lb, _ := leaked.([]byte); lb != nil {
		t.Fatalf("default keyspace has %q at the tenant key — reset tenant tx leaked to default (wrong-keyspace write)", lb)
	}
}

// TestTenant_Transact_AppliesDatabaseDefaults closes the dimensional-coverage gap
// @claude flagged: Tenant.Transact (→ TransactCtx → applyTxDefaults, a different
// call site than Tenant.CreateTransaction) also inherits DB-level option defaults.
// A 1ms database timeout must time out a tenant Transact.
func TestTenant_Transact_AppliesDatabaseDefaults(t *testing.T) {
	t.Parallel()
	db, _ := openTestDBWithTenants(t)

	name := fdb.Key(t.Name())
	if err := db.CreateTenant(name); err != nil {
		t.Fatalf("CreateTenant: %v", err)
	}
	tenant, err := db.OpenTenant(name)
	if err != nil {
		t.Fatalf("OpenTenant: %v", err)
	}

	if err := db.Options().SetTransactionTimeout(1); err != nil {
		t.Fatalf("SetTransactionTimeout: %v", err)
	}
	defer db.Options().SetTransactionTimeout(0)

	// The inherited 1ms timeout normally surfaces as transaction_timed_out (1031),
	// but under heavy parallel coverage load a degraded connection can surface the
	// same blown 1ms deadline as a raw context.DeadlineExceeded from the timeout-
	// bounded read ctx rather than a clean wire 1031 — and a bare `err.(fdb.Error)`
	// type assertion misses both a wrapped 1031 and the DeadlineExceeded form, so all
	// 100 iterations could fail to match and flake the test red (nightly coverage).
	// Use errors.As + errors.Is; both forms prove the timeout was inherited. If it
	// were NOT inherited the read would simply succeed (no error), so neither branch
	// can pass spuriously.
	inheritedTimeout := func(err error) bool {
		var fe fdb.Error
		if errors.As(err, &fe) && fe.Code == 1031 { // transaction_timed_out
			return true
		}
		return errors.Is(err, context.DeadlineExceeded)
	}
	var timedOut bool
	for i := 0; i < 100; i++ {
		_, err := tenant.Transact(func(tr fdb.WritableTransaction) (any, error) {
			tr.Get(fdb.Key("k")).MustGet() // GRV round-trip (>1ms) trips the inherited timeout
			return nil, nil
		})
		if inheritedTimeout(err) {
			timedOut = true
			break
		}
	}
	if !timedOut {
		t.Fatal("tenant.Transact did not inherit the 1ms database timeout (1031 expected) — TransactCtx applyTxDefaults not applied")
	}
}

func TestTenantCRUD(t *testing.T) {
	t.Parallel()
	db, _ := openTestDBWithTenants(t)

	name := fdb.Key("test-tenant-crud")

	// Create
	err := db.CreateTenant(name)
	if err != nil {
		t.Fatalf("CreateTenant: %v", err)
	}

	// List — should contain our tenant
	tenants, err := db.ListTenants()
	if err != nil {
		t.Fatalf("ListTenants: %v", err)
	}
	found := false
	for _, tn := range tenants {
		if string(tn) == string(name) {
			found = true
		}
	}
	if !found {
		t.Fatalf("tenant %q not in list: %v", name, tenants)
	}

	// Open — should return a valid tenant handle
	tenant, err := db.OpenTenant(name)
	if err != nil {
		t.Fatalf("OpenTenant: %v", err)
	}

	// Write+read through tenant
	_, err = tenant.Transact(func(tr fdb.WritableTransaction) (any, error) {
		tr.Set(fdb.Key("tenant-key"), []byte("tenant-value"))
		return nil, nil
	})
	if err != nil {
		t.Fatalf("tenant Set: %v", err)
	}

	result, err := tenant.Transact(func(tr fdb.WritableTransaction) (any, error) {
		return tr.Get(fdb.Key("tenant-key")).MustGet(), nil
	})
	if err != nil {
		t.Fatalf("tenant Get: %v", err)
	}
	if string(result.([]byte)) != "tenant-value" {
		t.Fatalf("got %q, want %q", result, "tenant-value")
	}

	// GetRange through tenant
	rangeResult, err := tenant.Transact(func(tr fdb.WritableTransaction) (any, error) {
		rr := tr.GetRange(fdb.KeyRange{Begin: fdb.Key(""), End: fdb.Key("\xff")}, fdb.RangeOptions{Limit: 10})
		return rr.GetSliceWithError()
	})
	if err != nil {
		t.Fatalf("tenant GetRange: %v", err)
	}
	kvs := rangeResult.([]fdb.KeyValue)
	if len(kvs) != 1 || string(kvs[0].Key) != "tenant-key" {
		t.Fatalf("tenant GetRange: got %d keys, want 1 (tenant-key)", len(kvs))
	}

	// Duplicate create should fail
	err = db.CreateTenant(name)
	if err == nil {
		t.Fatal("expected error on duplicate CreateTenant")
	}

	// Delete with data should fail (tenant_not_empty)
	err = db.DeleteTenant(name)
	if err == nil {
		t.Fatal("expected tenant_not_empty error")
	}

	// Clear tenant data first
	_, err = tenant.Transact(func(tr fdb.WritableTransaction) (any, error) {
		tr.ClearRange(fdb.KeyRange{Begin: fdb.Key(""), End: fdb.Key("\xff")})
		return nil, nil
	})
	if err != nil {
		t.Fatalf("clear tenant data: %v", err)
	}

	// Now delete should succeed
	err = db.DeleteTenant(name)
	if err != nil {
		t.Fatalf("DeleteTenant: %v", err)
	}

	// Delete again should fail
	err = db.DeleteTenant(name)
	if err == nil {
		t.Fatal("expected error on double DeleteTenant")
	}
}

// TestTenantIsolation verifies that data written in one tenant is invisible
// from another tenant. This is a fundamental FDB tenant guarantee.
func TestTenantIsolation(t *testing.T) {
	t.Parallel()
	db, _ := openTestDBWithTenants(t)

	tenantA := fdb.Key("isolation-tenant-a")
	tenantB := fdb.Key("isolation-tenant-b")

	if err := db.CreateTenant(tenantA); err != nil {
		t.Fatalf("create tenant A: %v", err)
	}
	t.Cleanup(func() {
		tA, _ := db.OpenTenant(tenantA)
		tA.Transact(func(tr fdb.WritableTransaction) (any, error) {
			tr.ClearRange(fdb.KeyRange{Begin: fdb.Key(""), End: fdb.Key("\xff")})
			return nil, nil
		})
		db.DeleteTenant(tenantA)
	})

	if err := db.CreateTenant(tenantB); err != nil {
		t.Fatalf("create tenant B: %v", err)
	}
	t.Cleanup(func() {
		tB, _ := db.OpenTenant(tenantB)
		tB.Transact(func(tr fdb.WritableTransaction) (any, error) {
			tr.ClearRange(fdb.KeyRange{Begin: fdb.Key(""), End: fdb.Key("\xff")})
			return nil, nil
		})
		db.DeleteTenant(tenantB)
	})

	tA, err := db.OpenTenant(tenantA)
	if err != nil {
		t.Fatalf("open tenant A: %v", err)
	}
	tB, err := db.OpenTenant(tenantB)
	if err != nil {
		t.Fatalf("open tenant B: %v", err)
	}

	// Write to tenant A.
	_, err = tA.Transact(func(tr fdb.WritableTransaction) (any, error) {
		tr.Set(fdb.Key("shared-key"), []byte("from-A"))
		tr.Set(fdb.Key("only-in-A"), []byte("secret"))
		return nil, nil
	})
	if err != nil {
		t.Fatalf("write to A: %v", err)
	}

	// Write to tenant B — same key name, different value.
	_, err = tB.Transact(func(tr fdb.WritableTransaction) (any, error) {
		tr.Set(fdb.Key("shared-key"), []byte("from-B"))
		return nil, nil
	})
	if err != nil {
		t.Fatalf("write to B: %v", err)
	}

	// Read from tenant A — should see A's value.
	result, err := tA.Transact(func(tr fdb.WritableTransaction) (any, error) {
		return tr.Get(fdb.Key("shared-key")).MustGet(), nil
	})
	if err != nil {
		t.Fatalf("read A: %v", err)
	}
	if string(result.([]byte)) != "from-A" {
		t.Errorf("tenant A shared-key: got %q, want %q", result, "from-A")
	}

	// Read from tenant B — should see B's value, not A's.
	result, err = tB.Transact(func(tr fdb.WritableTransaction) (any, error) {
		return tr.Get(fdb.Key("shared-key")).MustGet(), nil
	})
	if err != nil {
		t.Fatalf("read B: %v", err)
	}
	if string(result.([]byte)) != "from-B" {
		t.Errorf("tenant B shared-key: got %q, want %q", result, "from-B")
	}

	// Tenant B should NOT see "only-in-A".
	result, err = tB.Transact(func(tr fdb.WritableTransaction) (any, error) {
		return tr.Get(fdb.Key("only-in-A")).MustGet(), nil
	})
	if err != nil {
		t.Fatalf("read B only-in-A: %v", err)
	}
	if result.([]byte) != nil {
		t.Errorf("tenant B sees tenant A's key: got %q, want nil", result)
	}

	// GetRange in tenant B — should only see B's key.
	rangeResult, err := tB.Transact(func(tr fdb.WritableTransaction) (any, error) {
		rr := tr.GetRange(fdb.KeyRange{Begin: fdb.Key(""), End: fdb.Key("\xff")}, fdb.RangeOptions{})
		return rr.GetSliceWithError()
	})
	if err != nil {
		t.Fatalf("range B: %v", err)
	}
	kvs := rangeResult.([]fdb.KeyValue)
	if len(kvs) != 1 {
		t.Errorf("tenant B GetRange: got %d keys, want 1", len(kvs))
	}
}

// twoTenants creates two tenants with cleanup. Returns the open tenant handles.
func twoTenants(t *testing.T, db fdb.Database, nameA, nameB string) (fdb.Tenant, fdb.Tenant) {
	t.Helper()
	g := NewWithT(t)

	g.Expect(db.CreateTenant(fdb.Key(nameA))).To(Succeed())
	t.Cleanup(func() {
		tA, _ := db.OpenTenant(fdb.Key(nameA))
		tA.Transact(func(tr fdb.WritableTransaction) (any, error) {
			tr.ClearRange(fdb.KeyRange{Begin: fdb.Key(""), End: fdb.Key("\xff")})
			return nil, nil
		})
		db.DeleteTenant(fdb.Key(nameA))
	})

	g.Expect(db.CreateTenant(fdb.Key(nameB))).To(Succeed())
	t.Cleanup(func() {
		tB, _ := db.OpenTenant(fdb.Key(nameB))
		tB.Transact(func(tr fdb.WritableTransaction) (any, error) {
			tr.ClearRange(fdb.KeyRange{Begin: fdb.Key(""), End: fdb.Key("\xff")})
			return nil, nil
		})
		db.DeleteTenant(fdb.Key(nameB))
	})

	tA, err := db.OpenTenant(fdb.Key(nameA))
	g.Expect(err).NotTo(HaveOccurred())
	tB, err := db.OpenTenant(fdb.Key(nameB))
	g.Expect(err).NotTo(HaveOccurred())
	return tA, tB
}

// TestTenantClearRangeIsolation verifies that ClearRange in one tenant does NOT
// affect keys in another tenant. A cross-tenant ClearRange leak is a data breach.
func TestTenantClearRangeIsolation(t *testing.T) {
	t.Parallel()
	g := NewWithT(t)
	db, _ := openTestDBWithTenants(t)

	tA, tB := twoTenants(t, db, "clear-range-a", "clear-range-b")

	// Populate both tenants with keys in the same range.
	for _, tenant := range []fdb.Tenant{tA, tB} {
		_, err := tenant.Transact(func(tr fdb.WritableTransaction) (any, error) {
			for i := range 10 {
				tr.Set(fdb.Key(fmt.Sprintf("key-%03d", i)), []byte(fmt.Sprintf("val-%d", i)))
			}
			return nil, nil
		})
		g.Expect(err).NotTo(HaveOccurred())
	}

	// ClearRange ALL keys in tenant A.
	_, err := tA.Transact(func(tr fdb.WritableTransaction) (any, error) {
		tr.ClearRange(fdb.KeyRange{Begin: fdb.Key(""), End: fdb.Key("\xff")})
		return nil, nil
	})
	g.Expect(err).NotTo(HaveOccurred())

	// Tenant A should be empty.
	result, err := tA.Transact(func(tr fdb.WritableTransaction) (any, error) {
		rr := tr.GetRange(fdb.KeyRange{Begin: fdb.Key(""), End: fdb.Key("\xff")}, fdb.RangeOptions{})
		return rr.GetSliceWithError()
	})
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.([]fdb.KeyValue)).To(BeEmpty(), "tenant A should be empty after ClearRange")

	// Tenant B MUST still have all 10 keys.
	result, err = tB.Transact(func(tr fdb.WritableTransaction) (any, error) {
		rr := tr.GetRange(fdb.KeyRange{Begin: fdb.Key(""), End: fdb.Key("\xff")}, fdb.RangeOptions{})
		return rr.GetSliceWithError()
	})
	g.Expect(err).NotTo(HaveOccurred())
	kvs := result.([]fdb.KeyValue)
	g.Expect(kvs).To(HaveLen(10), "tenant B must retain all keys after tenant A ClearRange")

	// Verify the actual key-value pairs in B are intact.
	for i, kv := range kvs {
		g.Expect(string(kv.Key)).To(Equal(fmt.Sprintf("key-%03d", i)))
		g.Expect(string(kv.Value)).To(Equal(fmt.Sprintf("val-%d", i)))
	}

	// Partial ClearRange: add keys back to A, then clear a sub-range.
	_, err = tA.Transact(func(tr fdb.WritableTransaction) (any, error) {
		for i := range 10 {
			tr.Set(fdb.Key(fmt.Sprintf("key-%03d", i)), []byte("new-a"))
		}
		return nil, nil
	})
	g.Expect(err).NotTo(HaveOccurred())

	// Clear only key-003 through key-006 in tenant A.
	_, err = tA.Transact(func(tr fdb.WritableTransaction) (any, error) {
		tr.ClearRange(fdb.KeyRange{Begin: fdb.Key("key-003"), End: fdb.Key("key-007")})
		return nil, nil
	})
	g.Expect(err).NotTo(HaveOccurred())

	// Tenant A should have 6 keys remaining (0-2, 7-9).
	result, err = tA.Transact(func(tr fdb.WritableTransaction) (any, error) {
		rr := tr.GetRange(fdb.KeyRange{Begin: fdb.Key(""), End: fdb.Key("\xff")}, fdb.RangeOptions{})
		return rr.GetSliceWithError()
	})
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.([]fdb.KeyValue)).To(HaveLen(6))

	// Tenant B MUST still have exactly 10 keys, unmodified.
	result, err = tB.Transact(func(tr fdb.WritableTransaction) (any, error) {
		rr := tr.GetRange(fdb.KeyRange{Begin: fdb.Key(""), End: fdb.Key("\xff")}, fdb.RangeOptions{})
		return rr.GetSliceWithError()
	})
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.([]fdb.KeyValue)).To(HaveLen(10), "tenant B must be unaffected by partial ClearRange in A")
}

// TestTenantAtomicMutationIsolation verifies that atomic ADD in one tenant does
// NOT affect the same key in another tenant. Atomic mutation cross-talk is a
// severe isolation failure.
func TestTenantAtomicMutationIsolation(t *testing.T) {
	t.Parallel()
	g := NewWithT(t)
	db, _ := openTestDBWithTenants(t)

	tA, tB := twoTenants(t, db, "atomic-a", "atomic-b")

	counterKey := fdb.Key("counter")

	encodeInt64 := func(v int64) []byte {
		buf := make([]byte, 8)
		binary.LittleEndian.PutUint64(buf, uint64(v))
		return buf
	}
	decodeInt64 := func(b []byte) int64 {
		if len(b) == 0 {
			return 0
		}
		return int64(binary.LittleEndian.Uint64(b))
	}

	// Initialize counters to different values.
	_, err := tA.Transact(func(tr fdb.WritableTransaction) (any, error) {
		tr.Set(counterKey, encodeInt64(100))
		return nil, nil
	})
	g.Expect(err).NotTo(HaveOccurred())

	_, err = tB.Transact(func(tr fdb.WritableTransaction) (any, error) {
		tr.Set(counterKey, encodeInt64(200))
		return nil, nil
	})
	g.Expect(err).NotTo(HaveOccurred())

	// Perform 50 atomic ADDs in tenant A.
	for i := range 50 {
		_, err = tA.Transact(func(tr fdb.WritableTransaction) (any, error) {
			tr.Add(counterKey, encodeInt64(1))
			return nil, nil
		})
		g.Expect(err).NotTo(HaveOccurred(), "ADD #%d in A failed", i)
	}

	// Perform 10 atomic ADDs in tenant B.
	for i := range 10 {
		_, err = tB.Transact(func(tr fdb.WritableTransaction) (any, error) {
			tr.Add(counterKey, encodeInt64(1))
			return nil, nil
		})
		g.Expect(err).NotTo(HaveOccurred(), "ADD #%d in B failed", i)
	}

	// Read counter from tenant A — should be 100 + 50 = 150.
	result, err := tA.Transact(func(tr fdb.WritableTransaction) (any, error) {
		return tr.Get(counterKey).MustGet(), nil
	})
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(decodeInt64(result.([]byte))).To(Equal(int64(150)),
		"tenant A counter should be 100+50=150, not contaminated by B's adds")

	// Read counter from tenant B — should be 200 + 10 = 210.
	result, err = tB.Transact(func(tr fdb.WritableTransaction) (any, error) {
		return tr.Get(counterKey).MustGet(), nil
	})
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(decodeInt64(result.([]byte))).To(Equal(int64(210)),
		"tenant B counter should be 200+10=210, not contaminated by A's adds")

	// Also test other atomic mutations: Max in A should not affect B.
	_, err = tA.Transact(func(tr fdb.WritableTransaction) (any, error) {
		tr.Max(counterKey, encodeInt64(9999))
		return nil, nil
	})
	g.Expect(err).NotTo(HaveOccurred())

	// A should now be 9999.
	result, err = tA.Transact(func(tr fdb.WritableTransaction) (any, error) {
		return tr.Get(counterKey).MustGet(), nil
	})
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(decodeInt64(result.([]byte))).To(Equal(int64(9999)))

	// B must still be 210.
	result, err = tB.Transact(func(tr fdb.WritableTransaction) (any, error) {
		return tr.Get(counterKey).MustGet(), nil
	})
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(decodeInt64(result.([]byte))).To(Equal(int64(210)),
		"tenant B counter must not be affected by MAX in tenant A")
}

// TestTenantGetRangeIsolation verifies that GetRange with a wide open range
// in one tenant returns ONLY that tenant's keys. This is especially critical
// when both tenants have many keys with overlapping key names.
func TestTenantGetRangeIsolation(t *testing.T) {
	t.Parallel()
	g := NewWithT(t)
	db, _ := openTestDBWithTenants(t)

	tA, tB := twoTenants(t, db, "getrange-a", "getrange-b")

	// Write 100 keys to tenant A with prefix "data-".
	_, err := tA.Transact(func(tr fdb.WritableTransaction) (any, error) {
		for i := range 100 {
			tr.Set(fdb.Key(fmt.Sprintf("data-%04d", i)), []byte(fmt.Sprintf("A-%d", i)))
		}
		return nil, nil
	})
	g.Expect(err).NotTo(HaveOccurred())

	// Write 50 keys to tenant B with the SAME prefix "data-".
	_, err = tB.Transact(func(tr fdb.WritableTransaction) (any, error) {
		for i := range 50 {
			tr.Set(fdb.Key(fmt.Sprintf("data-%04d", i)), []byte(fmt.Sprintf("B-%d", i)))
		}
		return nil, nil
	})
	g.Expect(err).NotTo(HaveOccurred())

	// Full range scan in tenant A — must return exactly 100 keys.
	result, err := tA.Transact(func(tr fdb.WritableTransaction) (any, error) {
		rr := tr.GetRange(fdb.KeyRange{Begin: fdb.Key(""), End: fdb.Key("\xff")}, fdb.RangeOptions{})
		return rr.GetSliceWithError()
	})
	g.Expect(err).NotTo(HaveOccurred())
	kvsA := result.([]fdb.KeyValue)
	g.Expect(kvsA).To(HaveLen(100), "tenant A GetRange must return exactly 100 keys")

	// Every value must start with "A-" (not "B-").
	for _, kv := range kvsA {
		g.Expect(string(kv.Value)).To(HavePrefix("A-"),
			"tenant A key %q has value %q — leaked from B?", kv.Key, kv.Value)
	}

	// Full range scan in tenant B — must return exactly 50 keys.
	result, err = tB.Transact(func(tr fdb.WritableTransaction) (any, error) {
		rr := tr.GetRange(fdb.KeyRange{Begin: fdb.Key(""), End: fdb.Key("\xff")}, fdb.RangeOptions{})
		return rr.GetSliceWithError()
	})
	g.Expect(err).NotTo(HaveOccurred())
	kvsB := result.([]fdb.KeyValue)
	g.Expect(kvsB).To(HaveLen(50), "tenant B GetRange must return exactly 50 keys")

	// Every value must start with "B-".
	for _, kv := range kvsB {
		g.Expect(string(kv.Value)).To(HavePrefix("B-"),
			"tenant B key %q has value %q — leaked from A?", kv.Key, kv.Value)
	}

	// Prefix range scan: only "data-005*" keys in A (should be 10: data-0050..data-0059).
	result, err = tA.Transact(func(tr fdb.WritableTransaction) (any, error) {
		rr := tr.GetRange(fdb.KeyRange{
			Begin: fdb.Key("data-005"),
			End:   fdb.Key("data-006"),
		}, fdb.RangeOptions{})
		return rr.GetSliceWithError()
	})
	g.Expect(err).NotTo(HaveOccurred())
	subRange := result.([]fdb.KeyValue)
	g.Expect(subRange).To(HaveLen(10))

	// Same sub-range in B — B has keys 0000-0049, so use [data-002, data-003)
	// which captures data-0020 through data-0029 (10 keys).
	result, err = tB.Transact(func(tr fdb.WritableTransaction) (any, error) {
		rr := tr.GetRange(fdb.KeyRange{
			Begin: fdb.Key("data-002"),
			End:   fdb.Key("data-003"),
		}, fdb.RangeOptions{})
		return rr.GetSliceWithError()
	})
	g.Expect(err).NotTo(HaveOccurred())
	subRangeB := result.([]fdb.KeyValue)
	g.Expect(subRangeB).To(HaveLen(10))
	for _, kv := range subRangeB {
		g.Expect(string(kv.Value)).To(HavePrefix("B-"))
	}

	// Verify that a range existing only in A returns 0 in B.
	result, err = tB.Transact(func(tr fdb.WritableTransaction) (any, error) {
		rr := tr.GetRange(fdb.KeyRange{
			Begin: fdb.Key("data-005"),
			End:   fdb.Key("data-006"),
		}, fdb.RangeOptions{})
		return rr.GetSliceWithError()
	})
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.([]fdb.KeyValue)).To(BeEmpty(),
		"B must see 0 keys in range that only exists in A")

	// Reverse scan in A — still 100 keys, last key first.
	result, err = tA.Transact(func(tr fdb.WritableTransaction) (any, error) {
		rr := tr.GetRange(fdb.KeyRange{Begin: fdb.Key(""), End: fdb.Key("\xff")},
			fdb.RangeOptions{Reverse: true})
		return rr.GetSliceWithError()
	})
	g.Expect(err).NotTo(HaveOccurred())
	reverseA := result.([]fdb.KeyValue)
	g.Expect(reverseA).To(HaveLen(100))
	g.Expect(string(reverseA[0].Key)).To(Equal("data-0099"), "reverse scan should start with last key")
}

// TestTenantConflictIsolation verifies that concurrent writes to the SAME
// key name in DIFFERENT tenants do NOT conflict. Tenant isolation means their
// key spaces are entirely separate — there is no shared conflict domain.
func TestTenantConflictIsolation(t *testing.T) {
	t.Parallel()
	g := NewWithT(t)
	db, _ := openTestDBWithTenants(t)

	tA, tB := twoTenants(t, db, "conflict-a", "conflict-b")

	sharedKeyName := fdb.Key("hotkey")

	// Initialize both tenants with the same key.
	for _, tenant := range []fdb.Tenant{tA, tB} {
		_, err := tenant.Transact(func(tr fdb.WritableTransaction) (any, error) {
			tr.Set(sharedKeyName, []byte("initial"))
			return nil, nil
		})
		g.Expect(err).NotTo(HaveOccurred())
	}

	// Run 20 concurrent writes to the same key name in both tenants simultaneously.
	// If there were cross-tenant conflicts, we'd see retry storms or failures.
	const concurrency = 20
	var wg sync.WaitGroup
	errorsA := make([]error, concurrency)
	errorsB := make([]error, concurrency)

	for i := range concurrency {
		wg.Add(2)
		go func(idx int) {
			defer wg.Done()
			_, errorsA[idx] = tA.Transact(func(tr fdb.WritableTransaction) (any, error) {
				// Read-then-write creates a conflict window within the tenant.
				val := tr.Get(sharedKeyName).MustGet()
				tr.Set(sharedKeyName, []byte(fmt.Sprintf("A-%d-%s", idx, val)))
				return nil, nil
			})
		}(i)
		go func(idx int) {
			defer wg.Done()
			_, errorsB[idx] = tB.Transact(func(tr fdb.WritableTransaction) (any, error) {
				val := tr.Get(sharedKeyName).MustGet()
				tr.Set(sharedKeyName, []byte(fmt.Sprintf("B-%d-%s", idx, val)))
				return nil, nil
			})
		}(i)
	}
	wg.Wait()

	// All transactions must have eventually succeeded (Transact retries on conflict).
	for i, err := range errorsA {
		g.Expect(err).NotTo(HaveOccurred(), "tenant A write %d failed", i)
	}
	for i, err := range errorsB {
		g.Expect(err).NotTo(HaveOccurred(), "tenant B write %d failed", i)
	}

	// Final values should differ — tenant A's value must not contain "B-" and vice versa.
	result, err := tA.Transact(func(tr fdb.WritableTransaction) (any, error) {
		return tr.Get(sharedKeyName).MustGet(), nil
	})
	g.Expect(err).NotTo(HaveOccurred())
	valA := string(result.([]byte))
	g.Expect(valA).To(HavePrefix("A-"), "tenant A final value should start with A-, got %q", valA)

	result, err = tB.Transact(func(tr fdb.WritableTransaction) (any, error) {
		return tr.Get(sharedKeyName).MustGet(), nil
	})
	g.Expect(err).NotTo(HaveOccurred())
	valB := string(result.([]byte))
	g.Expect(valB).To(HavePrefix("B-"), "tenant B final value should start with B-, got %q", valB)

	// Cross-check: use CreateTransaction to manually create overlapping transactions
	// and verify they commit without conflict.
	txA, err := tA.CreateTransaction()
	g.Expect(err).NotTo(HaveOccurred())
	txB, err := tB.CreateTransaction()
	g.Expect(err).NotTo(HaveOccurred())

	// Both read and write the same key name in their respective tenants.
	_ = txA.Get(sharedKeyName).MustGet()
	_ = txB.Get(sharedKeyName).MustGet()
	txA.Set(sharedKeyName, []byte("manual-A"))
	txB.Set(sharedKeyName, []byte("manual-B"))

	// Both commits must succeed — no cross-tenant conflict.
	g.Expect(txA.Commit().Get()).To(Succeed())
	g.Expect(txB.Commit().Get()).To(Succeed())

	// Verify final values.
	result, err = tA.Transact(func(tr fdb.WritableTransaction) (any, error) {
		return tr.Get(sharedKeyName).MustGet(), nil
	})
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(string(result.([]byte))).To(Equal("manual-A"))

	result, err = tB.Transact(func(tr fdb.WritableTransaction) (any, error) {
		return tr.Get(sharedKeyName).MustGet(), nil
	})
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(string(result.([]byte))).To(Equal("manual-B"))
}
