package foundationdb_test

import (
	"context"
	"testing"
	"time"

	gofdb "github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb"
	foundationdb "github.com/birdayz/fdb-record-layer-go/pkg/testcontainers/foundationdb"
)

func TestFoundationDBDatabaseConnection(t *testing.T) {
	setupCtx, setupCancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer setupCancel()

	container, err := foundationdb.Run(setupCtx, "")
	if err != nil {
		t.Fatalf("Failed to start container: %v", err)
	}
	defer container.Terminate(context.Background())

	if err := container.InitializeDatabase(setupCtx); err != nil {
		t.Fatal(err)
	}

	path, err := container.ClusterFilePath(setupCtx)
	if err != nil {
		t.Fatal(err)
	}

	gofdb.MustAPIVersion(730)
	db, err := gofdb.OpenDatabase(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	_, err = db.Transact(func(tr gofdb.Transaction) (any, error) {
		tr.Get(gofdb.Key("test_key")).MustGet()
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
