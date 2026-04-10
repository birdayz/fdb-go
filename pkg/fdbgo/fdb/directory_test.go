package fdb_test

import (
	"fmt"
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

// TestDirectoryLayerMultiple tests creating multiple directories and verifying
// they get different prefixes (HCA works).
func TestDirectoryLayerMultiple(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)
	dir := directory.Root()

	// Create 10 directories.
	prefixes := make(map[string]bool)
	for i := 0; i < 10; i++ {
		path := []string{"multi", fmt.Sprintf("dir%d", i)}
		ds, err := dir.CreateOrOpen(db, path, nil)
		if err != nil {
			t.Fatalf("CreateOrOpen %v: %v", path, err)
		}
		prefix := string(ds.Bytes())
		if prefixes[prefix] {
			t.Errorf("duplicate prefix for dir%d: %q", i, prefix)
		}
		prefixes[prefix] = true
	}

	// List should show 10 children under "multi".
	children, err := dir.List(db, []string{"multi"})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(children) != 10 {
		t.Errorf("expected 10 children, got %d: %v", len(children), children)
	}

	// Clean up.
	dir.Remove(db, []string{"multi"})
}

// TestDirectoryLayerMove tests renaming a directory without moving data.
func TestDirectoryLayerMove(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)
	dir := directory.Root()

	// Create and write data.
	ds, err := dir.CreateOrOpen(db, []string{"move_test", "original"}, nil)
	if err != nil {
		t.Fatalf("CreateOrOpen: %v", err)
	}
	_, err = db.Transact(func(tr gofdb.Transaction) (any, error) {
		tr.Set(ds.Pack(tuple.Tuple{"data"}), []byte("hello"))
		return nil, nil
	})
	if err != nil {
		t.Fatalf("Set: %v", err)
	}

	// Move.
	newDs, err := dir.Move(db, []string{"move_test", "original"}, []string{"move_test", "renamed"})
	if err != nil {
		t.Fatalf("Move: %v", err)
	}

	// Old path should not exist.
	exists, err := dir.Exists(db, []string{"move_test", "original"})
	if err != nil {
		t.Fatalf("Exists old: %v", err)
	}
	if exists {
		t.Error("old path should not exist after move")
	}

	// Data should be accessible at new path with same prefix.
	if string(newDs.Bytes()) != string(ds.Bytes()) {
		t.Errorf("move changed prefix: old=%q, new=%q", ds.Bytes(), newDs.Bytes())
	}
	result, err := db.Transact(func(tr gofdb.Transaction) (any, error) {
		return tr.Get(newDs.Pack(tuple.Tuple{"data"})).MustGet(), nil
	})
	if err != nil {
		t.Fatalf("Get after move: %v", err)
	}
	if string(result.([]byte)) != "hello" {
		t.Errorf("data after move: got %q, want %q", result, "hello")
	}

	dir.Remove(db, []string{"move_test"})
}

// TestDirectoryLayerOpenExisting tests that Open fails for non-existent
// directories and succeeds for existing ones with the same prefix.
func TestDirectoryLayerOpenExisting(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)
	dir := directory.Root()

	// Open non-existent should fail.
	_, err := dir.Open(db, []string{"nonexistent_dir_test"}, nil)
	if err == nil {
		t.Fatal("expected error opening non-existent directory")
	}

	// Create, then open — should get same prefix.
	ds1, err := dir.Create(db, []string{"open_test"}, nil)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	ds2, err := dir.Open(db, []string{"open_test"}, nil)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if string(ds1.Bytes()) != string(ds2.Bytes()) {
		t.Errorf("prefix mismatch: create=%q, open=%q", ds1.Bytes(), ds2.Bytes())
	}

	dir.Remove(db, []string{"open_test"})
}

// TestDirectoryLayerSubdirectory tests creating subdirectories through
// a DirectorySubspace (matches Java Record Layer's nested KeySpace paths).
func TestDirectoryLayerSubdirectory(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)
	dir := directory.Root()

	// Create parent.
	parent, err := dir.CreateOrOpen(db, []string{"subdir_test"}, nil)
	if err != nil {
		t.Fatalf("CreateOrOpen parent: %v", err)
	}

	// Create child through parent DirectorySubspace.
	child, err := parent.CreateOrOpen(db, []string{"child1"}, nil)
	if err != nil {
		t.Fatalf("CreateOrOpen child: %v", err)
	}

	// Write data in child.
	_, err = db.Transact(func(tr gofdb.Transaction) (any, error) {
		tr.Set(child.Pack(tuple.Tuple{"x"}), []byte("child_data"))
		return nil, nil
	})
	if err != nil {
		t.Fatalf("write child: %v", err)
	}

	// List children through parent.
	children, err := parent.List(db, nil)
	if err != nil {
		t.Fatalf("List children: %v", err)
	}
	if len(children) != 1 || children[0] != "child1" {
		t.Errorf("expected [child1], got %v", children)
	}

	// Read data back through re-opened child.
	child2, err := parent.Open(db, []string{"child1"}, nil)
	if err != nil {
		t.Fatalf("Open child: %v", err)
	}
	result, err := db.Transact(func(tr gofdb.Transaction) (any, error) {
		return tr.Get(child2.Pack(tuple.Tuple{"x"})).MustGet(), nil
	})
	if err != nil {
		t.Fatalf("read child: %v", err)
	}
	if string(result.([]byte)) != "child_data" {
		t.Errorf("child data: got %q, want %q", result, "child_data")
	}

	dir.Remove(db, []string{"subdir_test"})
}

// TestDirectoryLayerDuplicateCreate tests that creating an already-existing
// directory returns an error.
func TestDirectoryLayerDuplicateCreate(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)
	dir := directory.Root()

	_, err := dir.Create(db, []string{"dup_test"}, nil)
	if err != nil {
		t.Fatalf("first Create: %v", err)
	}

	// Second create should fail.
	_, err = dir.Create(db, []string{"dup_test"}, nil)
	if err == nil {
		t.Fatal("expected error on duplicate Create")
	}

	dir.Remove(db, []string{"dup_test"})
}
