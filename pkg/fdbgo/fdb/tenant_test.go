package fdb_test

import (
	"context"
	"testing"
	"time"

	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb"
	tcfdb "github.com/birdayz/fdb-record-layer-go/pkg/testcontainers/foundationdb"
)

// openTestDBWithTenants starts a container with tenant_mode=optional_experimental.
func openTestDBWithTenants(t *testing.T) fdb.Database {
	t.Helper()
	fdb.MustAPIVersion(730)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	container, err := tcfdb.Run(ctx, "")
	if err != nil {
		t.Fatalf("start FDB container: %v", err)
	}
	t.Cleanup(func() { container.Terminate(context.Background()) })

	// Initialize with tenant mode
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
	return db
}

func TestTenantCRUD(t *testing.T) {
	t.Parallel()
	db := openTestDBWithTenants(t)

	name := fdb.Key("test-tenant-crud")

	// First verify system key reads work
	_, err := db.Transact(func(tr fdb.Transaction) (any, error) {
		v, err := tr.Get(fdb.Key("\xff/tenant/count")).Get()
		t.Logf("system key read: val=%v err=%v", v, err)
		return nil, err
	})
	if err != nil {
		t.Fatalf("system key read: %v", err)
	}

	// Create
	err = db.CreateTenant(name)
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

	// Duplicate create should fail
	err = db.CreateTenant(name)
	if err == nil {
		t.Fatal("expected error on duplicate CreateTenant")
	}

	// Delete
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
