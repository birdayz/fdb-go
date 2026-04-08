package foundationdb_test

import (
	"context"
	"testing"

	gofdb "github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb"
	foundationdb "github.com/birdayz/fdb-record-layer-go/pkg/testcontainers/foundationdb"
	"github.com/birdayz/fdb-record-layer-go/pkg/testcontainers/foundationdb/gofdbhelper"
)

func TestFoundationDBDatabaseConnection(t *testing.T) {
	ctx := context.Background()

	container, err := foundationdb.Run(ctx, "")
	if err != nil {
		t.Fatalf("Failed to start container: %v", err)
	}
	defer container.Terminate(ctx)

	err = container.InitializeDatabase(ctx)
	if err != nil {
		t.Fatalf("Failed to initialize database: %v", err)
	}

	gofdb.MustAPIVersion(730)
	db, err := gofdbhelper.OpenDatabase(ctx, container)
	if err != nil {
		t.Fatalf("Failed to open database: %v", err)
	}
	defer db.Close()

	_, err = db.Transact(func(tr gofdb.Transaction) (any, error) {
		tr.Get(gofdb.Key("test_key")).MustGet()
		return "success", nil
	})
	if err != nil {
		t.Fatalf("Failed to execute transaction: %v", err)
	}
}
