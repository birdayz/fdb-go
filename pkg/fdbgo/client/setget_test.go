package client

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"

	tcfdb "github.com/birdayz/fdb-record-layer-go/pkg/testcontainers/foundationdb"
)

// TestSetGet is the minimal end-to-end test: write a key, read it back.
// Pure Go client, no C bindings. Talks to a real FDB 7.3.75 testcontainer.
func TestSetGet(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	db := openTestDB(t, ctx)
	defer db.Close()

	// Write
	_, err := db.Transact(ctx, func(tx *Transaction) (interface{}, error) {
		tx.Set([]byte("hello"), []byte("world"))
		return nil, nil
	})
	if err != nil {
		t.Fatalf("Set: %v", err)
	}

	// Read
	result, err := db.Transact(ctx, func(tx *Transaction) (interface{}, error) {
		return tx.Get(ctx, []byte("hello"))
	})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if string(result.([]byte)) != "world" {
		t.Fatalf("Get: got %q, want %q", result, "world")
	}
}

// openTestDB starts an FDB testcontainer and returns a connected Database.
func openTestDB(t *testing.T, ctx context.Context) *Database {
	t.Helper()

	container, err := tcfdb.Run(ctx, "", tcfdb.WithVersion("7.3.75"))
	if err != nil {
		t.Fatalf("start FDB container: %v", err)
	}
	t.Cleanup(func() { container.Terminate(ctx) })

	connStr, err := container.ClusterFile(ctx)
	if err != nil {
		t.Fatalf("get cluster file: %v", err)
	}

	cf, err := ParseClusterString(connStr)
	if err != nil {
		t.Fatalf("parse cluster string: %v", err)
	}

	// Configure cluster
	exitCode, _, _ := container.Exec(ctx, []string{"fdbcli", "--exec", "configure new single ssd"})
	if exitCode != 0 {
		t.Fatalf("fdbcli configure exit: %d", exitCode)
	}
	time.Sleep(2 * time.Second)

	// Read internal cluster file for correct cluster key
	_, internalReader, err := container.Exec(ctx, []string{"cat", "/var/fdb/fdb.cluster"})
	if err != nil {
		t.Fatalf("read internal cluster file: %v", err)
	}
	internalBytes, _ := io.ReadAll(internalReader)
	internalStr := string(internalBytes)
	if idx := strings.Index(internalStr, cf.Description); idx >= 0 {
		internalStr = internalStr[idx:]
	}
	internalCF, err := ParseClusterString(strings.TrimSpace(internalStr))
	if err != nil {
		t.Fatalf("parse internal cluster: %v", err)
	}

	connectCF := &ClusterFile{
		Description:  internalCF.Description,
		ID:           internalCF.ID,
		Coordinators: cf.Coordinators,
	}
	connectCF.InternalKey = internalCF.Description + ":" + internalCF.ID + "@"
	for i, a := range internalCF.Coordinators {
		if i > 0 {
			connectCF.InternalKey += ","
		}
		connectCF.InternalKey += a
	}

	cluster := NewClusterFromConfig(connectCF)
	t.Cleanup(func() { cluster.Close() })

	if err := cluster.Connect(ctx); err != nil {
		t.Fatalf("Connect: %v", err)
	}

	return &Database{
		cluster:       cluster,
		grvBatcher:    NewGRVBatcher(cluster),
		locationCache: NewLocationCache(cluster),
	}
}
