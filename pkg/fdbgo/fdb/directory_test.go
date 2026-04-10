package fdb_test

import (
	"testing"

	gofdb "github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb/directory"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb/tuple"
)

// TestDirectoryLayerBasic verifies that the Apple Go directory layer works
// with our pure Go FDB client. This is critical for compatibility with Java
// Record Layer applications that use KeySpace/DirectoryLayerDirectory.
func TestDirectoryLayerBasic(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)

	dir := directory.Root()

	// Create a directory.
	ds, err := dir.CreateOrOpen(db, []string{"test", "myapp"}, nil)
	if err != nil {
		t.Fatalf("CreateOrOpen: %v", err)
	}
	t.Logf("directory prefix: %v", ds.Bytes())

	// Write data in the directory subspace.
	_, err = db.Transact(func(tr gofdb.Transaction) (any, error) {
		tr.Set(ds.Pack(tuple.Tuple{"key1"}), []byte("value1"))
		return nil, nil
	})
	if err != nil {
		t.Fatalf("Set: %v", err)
	}

	// Read it back.
	result, err := db.Transact(func(tr gofdb.Transaction) (any, error) {
		return tr.Get(ds.Pack(tuple.Tuple{"key1"})).MustGet(), nil
	})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if string(result.([]byte)) != "value1" {
		t.Errorf("got %q, want %q", result, "value1")
	}

	// List should show "test".
	dirs, err := dir.List(db, nil)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	found := false
	for _, d := range dirs {
		if d == "test" {
			found = true
		}
	}
	if !found {
		t.Errorf("directory 'test' not found in list: %v", dirs)
	}

	// Exists.
	exists, err := dir.Exists(db, []string{"test", "myapp"})
	if err != nil {
		t.Fatalf("Exists: %v", err)
	}
	if !exists {
		t.Error("directory should exist")
	}

	// Remove.
	_, err = dir.Remove(db, []string{"test"})
	if err != nil {
		t.Fatalf("Remove: %v", err)
	}

	exists, err = dir.Exists(db, []string{"test"})
	if err != nil {
		t.Fatalf("Exists after remove: %v", err)
	}
	if exists {
		t.Error("directory should not exist after remove")
	}
}
