package foundationdb_test

import (
	"context"
	"testing"
	"time"

	gofdb "fdb.dev/pkg/fdbgo/fdb"
	foundationdb "fdb.dev/pkg/testcontainers/foundationdb"
)

func TestFoundationDBDatabaseConnection(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	container, err := foundationdb.Run(ctx, "")
	if err != nil {
		t.Fatalf("Failed to start container: %v", err)
	}
	defer container.Terminate(ctx)

	path, err := container.ClusterFilePath(ctx)
	if err != nil {
		t.Fatal(err)
	}

	gofdb.MustAPIVersion(730)
	db, err := gofdb.OpenDatabase(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	// Verify we can execute a transaction.
	_, err = db.Transact(func(tr gofdb.WritableTransaction) (any, error) {
		tr.Set(gofdb.Key("test_key"), []byte("test_value"))
		return nil, nil
	})
	if err != nil {
		t.Fatalf("write transaction: %v", err)
	}

	// Read it back.
	result, err := db.Transact(func(tr gofdb.WritableTransaction) (any, error) {
		return tr.Get(gofdb.Key("test_key")).MustGet(), nil
	})
	if err != nil {
		t.Fatalf("read transaction: %v", err)
	}
	if string(result.([]byte)) != "test_value" {
		t.Fatalf("expected 'test_value', got %q", result)
	}
}
