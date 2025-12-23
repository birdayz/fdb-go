package foundationdb

import (
	"context"
	"testing"

	"github.com/apple/foundationdb/bindings/go/src/fdb"
)

func TestFoundationDBDatabaseConnection(t *testing.T) {
	ctx := context.Background()

	container, err := Run(ctx, "",
		WithDatabase("database_connection_test"),
		WithAPIVersion(720),
	)
	if err != nil {
		t.Fatalf("Failed to start container: %v", err)
	}
	defer container.Terminate(ctx)

	// Initialize database
	err = container.InitializeDatabase(ctx)
	if err != nil {
		t.Fatalf("Failed to initialize database: %v", err)
	}

	// Get database connection
	db, err := container.GetFDBDatabase(ctx)
	if err != nil {
		t.Fatalf("Failed to get FDB database: %v", err)
	}

	// Try a simple transaction
	_, err = db.Transact(func(tr fdb.Transaction) (interface{}, error) {
		// Just do a simple get operation
		tr.Get(fdb.Key("test_key")).MustGet()
		return "success", nil
	})

	if err != nil {
		t.Fatalf("Failed to execute transaction: %v", err)
	}

	t.Log("Successfully connected to FoundationDB and executed a transaction")
}
