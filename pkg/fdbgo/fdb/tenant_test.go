package fdb_test

import (
	"context"
	"testing"
	"time"

	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb"
	tcfdb "github.com/birdayz/fdb-record-layer-go/pkg/testcontainers/foundationdb"
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
	_, err = tenant.Transact(func(tr fdb.Transaction) (any, error) {
		tr.Set(fdb.Key("tenant-key"), []byte("tenant-value"))
		return nil, nil
	})
	if err != nil {
		t.Fatalf("tenant Set: %v", err)
	}

	result, err := tenant.Transact(func(tr fdb.Transaction) (any, error) {
		return tr.Get(fdb.Key("tenant-key")).MustGet(), nil
	})
	if err != nil {
		t.Fatalf("tenant Get: %v", err)
	}
	if string(result.([]byte)) != "tenant-value" {
		t.Fatalf("got %q, want %q", result, "tenant-value")
	}

	// GetRange through tenant
	rangeResult, err := tenant.Transact(func(tr fdb.Transaction) (any, error) {
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
	_, err = tenant.Transact(func(tr fdb.Transaction) (any, error) {
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
		tA.Transact(func(tr fdb.Transaction) (any, error) {
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
		tB.Transact(func(tr fdb.Transaction) (any, error) {
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
	_, err = tA.Transact(func(tr fdb.Transaction) (any, error) {
		tr.Set(fdb.Key("shared-key"), []byte("from-A"))
		tr.Set(fdb.Key("only-in-A"), []byte("secret"))
		return nil, nil
	})
	if err != nil {
		t.Fatalf("write to A: %v", err)
	}

	// Write to tenant B — same key name, different value.
	_, err = tB.Transact(func(tr fdb.Transaction) (any, error) {
		tr.Set(fdb.Key("shared-key"), []byte("from-B"))
		return nil, nil
	})
	if err != nil {
		t.Fatalf("write to B: %v", err)
	}

	// Read from tenant A — should see A's value.
	result, err := tA.Transact(func(tr fdb.Transaction) (any, error) {
		return tr.Get(fdb.Key("shared-key")).MustGet(), nil
	})
	if err != nil {
		t.Fatalf("read A: %v", err)
	}
	if string(result.([]byte)) != "from-A" {
		t.Errorf("tenant A shared-key: got %q, want %q", result, "from-A")
	}

	// Read from tenant B — should see B's value, not A's.
	result, err = tB.Transact(func(tr fdb.Transaction) (any, error) {
		return tr.Get(fdb.Key("shared-key")).MustGet(), nil
	})
	if err != nil {
		t.Fatalf("read B: %v", err)
	}
	if string(result.([]byte)) != "from-B" {
		t.Errorf("tenant B shared-key: got %q, want %q", result, "from-B")
	}

	// Tenant B should NOT see "only-in-A".
	result, err = tB.Transact(func(tr fdb.Transaction) (any, error) {
		return tr.Get(fdb.Key("only-in-A")).MustGet(), nil
	})
	if err != nil {
		t.Fatalf("read B only-in-A: %v", err)
	}
	if result.([]byte) != nil {
		t.Errorf("tenant B sees tenant A's key: got %q, want nil", result)
	}

	// GetRange in tenant B — should only see B's key.
	rangeResult, err := tB.Transact(func(tr fdb.Transaction) (any, error) {
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
