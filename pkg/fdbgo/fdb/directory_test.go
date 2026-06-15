package fdb_test

import (
	"bytes"
	"fmt"
	"testing"

	gofdb "github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb/directory"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb/subspace"
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
	_, err = db.Transact(func(tr gofdb.WritableTransaction) (any, error) {
		tr.Set(ds.Pack(tuple.Tuple{"key1"}), []byte("value1"))
		return nil, nil
	})
	if err != nil {
		t.Fatalf("Set: %v", err)
	}

	// Read it back.
	result, err := db.Transact(func(tr gofdb.WritableTransaction) (any, error) {
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
	_, err = db.Transact(func(tr gofdb.WritableTransaction) (any, error) {
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
	result, err := db.Transact(func(tr gofdb.WritableTransaction) (any, error) {
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
	_, err = db.Transact(func(tr gofdb.WritableTransaction) (any, error) {
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
	result, err := db.Transact(func(tr gofdb.WritableTransaction) (any, error) {
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

// TestDirectoryLayerConcurrent tests that concurrent directory creation
// via the HCA produces unique prefixes without conflicts.
func TestDirectoryLayerConcurrent(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)
	dir := directory.Root()

	const n = 20
	type result struct {
		prefix []byte
		err    error
	}
	results := make(chan result, n)

	for i := 0; i < n; i++ {
		go func(idx int) {
			path := []string{"concurrent_test", fmt.Sprintf("dir_%03d", idx)}
			ds, err := dir.CreateOrOpen(db, path, nil)
			if err != nil {
				results <- result{err: err}
				return
			}
			results <- result{prefix: ds.Bytes()}
		}(i)
	}

	prefixes := make(map[string]int)
	for i := 0; i < n; i++ {
		r := <-results
		if r.err != nil {
			t.Errorf("concurrent create %d: %v", i, r.err)
			continue
		}
		key := string(r.prefix)
		if prev, exists := prefixes[key]; exists {
			t.Errorf("duplicate prefix %x: dirs %d and %d", r.prefix, prev, i)
		}
		prefixes[key] = i
	}

	if len(prefixes) != n {
		t.Errorf("expected %d unique prefixes, got %d", n, len(prefixes))
	}

	dir.Remove(db, []string{"concurrent_test"})
}

// TestDirectoryLayerLayerCheck verifies that opening a directory with a
// mismatched layer returns an error, and that opening with the correct
// layer succeeds.
func TestDirectoryLayerLayerCheck(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)
	dir := directory.Root()

	path := []string{"layer_check_test"}

	// Create with a specific layer.
	ds, err := dir.Create(db, path, []byte("my_layer"))
	if err != nil {
		t.Fatalf("Create with layer: %v", err)
	}
	t.Logf("created directory with prefix %x, layer=%q", ds.Bytes(), ds.GetLayer())

	// Open with wrong layer — should fail.
	_, err = dir.Open(db, path, []byte("wrong_layer"))
	if err == nil {
		t.Fatal("expected error opening directory with wrong layer")
	}
	t.Logf("correctly got error for wrong layer: %v", err)

	// Open with correct layer — should succeed.
	ds2, err := dir.Open(db, path, []byte("my_layer"))
	if err != nil {
		t.Fatalf("Open with correct layer: %v", err)
	}
	if string(ds2.Bytes()) != string(ds.Bytes()) {
		t.Errorf("prefix mismatch: create=%x, open=%x", ds.Bytes(), ds2.Bytes())
	}

	// Open with nil layer (no check) — should also succeed.
	ds3, err := dir.Open(db, path, nil)
	if err != nil {
		t.Fatalf("Open with nil layer: %v", err)
	}
	if string(ds3.Bytes()) != string(ds.Bytes()) {
		t.Errorf("prefix mismatch: create=%x, open-nil=%x", ds.Bytes(), ds3.Bytes())
	}

	dir.Remove(db, path)
}

// TestDirectoryLayerRemoveNonExistent verifies that removing a directory
// path that does not exist returns (false, nil) — not an error.
func TestDirectoryLayerRemoveNonExistent(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)
	dir := directory.Root()

	removed, err := dir.Remove(db, []string{"remove_nonexistent_test", "does_not_exist"})
	if err != nil {
		t.Fatalf("Remove non-existent: unexpected error: %v", err)
	}
	if removed {
		t.Error("Remove non-existent: expected false, got true")
	}
}

// TestDirectoryLayerRecursiveRemove verifies that removing a parent
// directory also removes all children and grandchildren.
func TestDirectoryLayerRecursiveRemove(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)
	dir := directory.Root()

	// Create parent/child/grandchild.
	_, err := dir.CreateOrOpen(db, []string{"recursive_rm_test", "child", "grandchild"}, nil)
	if err != nil {
		t.Fatalf("CreateOrOpen grandchild: %v", err)
	}

	// Also create a sibling child for completeness.
	_, err = dir.CreateOrOpen(db, []string{"recursive_rm_test", "child2"}, nil)
	if err != nil {
		t.Fatalf("CreateOrOpen child2: %v", err)
	}

	// Verify they exist.
	for _, path := range [][]string{
		{"recursive_rm_test"},
		{"recursive_rm_test", "child"},
		{"recursive_rm_test", "child", "grandchild"},
		{"recursive_rm_test", "child2"},
	} {
		exists, err := dir.Exists(db, path)
		if err != nil {
			t.Fatalf("Exists %v: %v", path, err)
		}
		if !exists {
			t.Errorf("directory %v should exist before removal", path)
		}
	}

	// Remove parent — should wipe everything.
	removed, err := dir.Remove(db, []string{"recursive_rm_test"})
	if err != nil {
		t.Fatalf("Remove parent: %v", err)
	}
	if !removed {
		t.Error("Remove parent: expected true, got false")
	}

	// Verify all are gone.
	for _, path := range [][]string{
		{"recursive_rm_test"},
		{"recursive_rm_test", "child"},
		{"recursive_rm_test", "child", "grandchild"},
		{"recursive_rm_test", "child2"},
	} {
		exists, err := dir.Exists(db, path)
		if err != nil {
			t.Fatalf("Exists %v after removal: %v", path, err)
		}
		if exists {
			t.Errorf("directory %v should not exist after parent removal", path)
		}
	}
}

// TestDirectoryLayerDataIsolation verifies that two directories get
// different prefixes and that data written in one is invisible in the other.
func TestDirectoryLayerDataIsolation(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)
	dir := directory.Root()

	dsA, err := dir.CreateOrOpen(db, []string{"isolation_test", "dir_a"}, nil)
	if err != nil {
		t.Fatalf("CreateOrOpen dir_a: %v", err)
	}
	dsB, err := dir.CreateOrOpen(db, []string{"isolation_test", "dir_b"}, nil)
	if err != nil {
		t.Fatalf("CreateOrOpen dir_b: %v", err)
	}

	// Prefixes must differ.
	if bytes.Equal(dsA.Bytes(), dsB.Bytes()) {
		t.Fatalf("directories have same prefix: %x", dsA.Bytes())
	}

	// Write data into both directories.
	_, err = db.Transact(func(tr gofdb.WritableTransaction) (any, error) {
		tr.Set(dsA.Pack(tuple.Tuple{"shared_key"}), []byte("from_a"))
		tr.Set(dsB.Pack(tuple.Tuple{"shared_key"}), []byte("from_b"))
		return nil, nil
	})
	if err != nil {
		t.Fatalf("write data: %v", err)
	}

	// Read from dir_a — should see "from_a".
	result, err := db.Transact(func(tr gofdb.WritableTransaction) (any, error) {
		return tr.Get(dsA.Pack(tuple.Tuple{"shared_key"})).MustGet(), nil
	})
	if err != nil {
		t.Fatalf("read dir_a: %v", err)
	}
	if string(result.([]byte)) != "from_a" {
		t.Errorf("dir_a: got %q, want %q", result, "from_a")
	}

	// Read from dir_b — should see "from_b".
	result, err = db.Transact(func(tr gofdb.WritableTransaction) (any, error) {
		return tr.Get(dsB.Pack(tuple.Tuple{"shared_key"})).MustGet(), nil
	})
	if err != nil {
		t.Fatalf("read dir_b: %v", err)
	}
	if string(result.([]byte)) != "from_b" {
		t.Errorf("dir_b: got %q, want %q", result, "from_b")
	}

	// Read dir_a's key from dir_b's subspace — should be nil (isolated).
	result, err = db.Transact(func(tr gofdb.WritableTransaction) (any, error) {
		val := tr.Get(dsB.Pack(tuple.Tuple{"only_in_a"})).MustGet()
		return val, nil
	})
	if err != nil {
		t.Fatalf("cross-read: %v", err)
	}
	if result.([]byte) != nil {
		t.Errorf("cross-directory read should be nil, got %q", result)
	}

	dir.Remove(db, []string{"isolation_test"})
}

// TestDirectoryLayerNewDirectoryLayer verifies that a custom DirectoryLayer
// with non-default node/content subspaces works independently of the root.
func TestDirectoryLayerNewDirectoryLayer(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)

	// Create a custom directory layer rooted at a unique subspace.
	nodeSS := subspace.Sub("custom_dl_test", "nodes")
	contentSS := subspace.Sub("custom_dl_test", "content")
	customDL := directory.NewDirectoryLayer(nodeSS, contentSS, false)

	// Create a directory in the custom layer.
	ds, err := customDL.CreateOrOpen(db, []string{"app", "data"}, nil)
	if err != nil {
		t.Fatalf("custom DL CreateOrOpen: %v", err)
	}

	// Write and read data.
	_, err = db.Transact(func(tr gofdb.WritableTransaction) (any, error) {
		tr.Set(ds.Pack(tuple.Tuple{"k1"}), []byte("v1"))
		return nil, nil
	})
	if err != nil {
		t.Fatalf("write: %v", err)
	}

	result, err := db.Transact(func(tr gofdb.WritableTransaction) (any, error) {
		return tr.Get(ds.Pack(tuple.Tuple{"k1"})).MustGet(), nil
	})
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(result.([]byte)) != "v1" {
		t.Errorf("got %q, want %q", result, "v1")
	}

	// The custom DL should NOT see directories from the root DL and vice versa.
	rootDir := directory.Root()

	// Create something in root.
	_, err = rootDir.CreateOrOpen(db, []string{"custom_dl_root_check"}, nil)
	if err != nil {
		t.Fatalf("root CreateOrOpen: %v", err)
	}

	// Custom DL should not see root's directory.
	exists, err := customDL.Exists(db, []string{"custom_dl_root_check"})
	if err != nil {
		t.Fatalf("custom Exists: %v", err)
	}
	if exists {
		t.Error("custom DL should not see root DL's directories")
	}

	// Root should not see custom DL's directory.
	exists, err = rootDir.Exists(db, []string{"app", "data"})
	if err != nil {
		t.Fatalf("root Exists: %v", err)
	}
	if exists {
		t.Error("root DL should not see custom DL's directories")
	}

	// Clean up.
	customDL.Remove(db, []string{"app"})
	rootDir.Remove(db, []string{"custom_dl_root_check"})

	// Also clear the custom DL's metadata subspaces.
	_, _ = db.Transact(func(tr gofdb.WritableTransaction) (any, error) {
		tr.ClearRange(nodeSS)
		tr.ClearRange(contentSS)
		return nil, nil
	})
}

// TestDirectoryLayerCreatePrefix verifies that a DirectoryLayer with
// allowManualPrefixes=true accepts a manually specified prefix via
// CreatePrefix, and that the returned DirectorySubspace uses that prefix.
func TestDirectoryLayerCreatePrefix(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)

	// Must use a custom DL with allowManualPrefixes=true.
	// The default root DL does not allow manual prefixes.
	nodeSS := subspace.Sub("prefix_dl_test", "nodes")
	contentSS := subspace.Sub("prefix_dl_test", "content")
	customDL := directory.NewDirectoryLayer(nodeSS, contentSS, true)

	manualPrefix := []byte{0xAB, 0xCD}

	ds, err := customDL.CreatePrefix(db, []string{"manual_prefix_dir"}, nil, manualPrefix)
	if err != nil {
		t.Fatalf("CreatePrefix: %v", err)
	}

	// The returned subspace should use the manual prefix.
	if !bytes.Equal(ds.Bytes(), manualPrefix) {
		t.Errorf("prefix mismatch: got %x, want %x", ds.Bytes(), manualPrefix)
	}

	// Write and read data at the manual prefix to verify it works.
	_, err = db.Transact(func(tr gofdb.WritableTransaction) (any, error) {
		tr.Set(ds.Pack(tuple.Tuple{"test"}), []byte("manual"))
		return nil, nil
	})
	if err != nil {
		t.Fatalf("write: %v", err)
	}

	result, err := db.Transact(func(tr gofdb.WritableTransaction) (any, error) {
		return tr.Get(ds.Pack(tuple.Tuple{"test"})).MustGet(), nil
	})
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(result.([]byte)) != "manual" {
		t.Errorf("got %q, want %q", result, "manual")
	}

	// Verify CreatePrefix on the default root DL fails (manual prefixes not allowed).
	rootDir := directory.Root()
	defer rootDir.Remove(db, []string{"should_fail_prefix"}) // cleanup if CreatePrefix incorrectly succeeds
	_, err = rootDir.CreatePrefix(db, []string{"should_fail_prefix"}, nil, []byte{0xFF})
	if err == nil {
		t.Fatal("expected error using CreatePrefix on default root DL (manual prefixes disallowed)")
	}

	// Clean up.
	customDL.Remove(db, []string{"manual_prefix_dir"})
	_, _ = db.Transact(func(tr gofdb.WritableTransaction) (any, error) {
		tr.ClearRange(nodeSS)
		tr.ClearRange(contentSS)
		return nil, nil
	})
}

// TestDirectoryLayerPartition verifies partition directory creation and
// isolation. Partitions get their own directory layer namespace — they
// can't see directories from the parent layer, and vice versa.
// This exercises directoryPartition.go which has 0% test coverage.
func TestDirectoryLayerPartition(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)
	dir := directory.Root()

	// Create a partition.
	partDS, err := dir.CreateOrOpen(db, []string{"test_partition", "part1"}, []byte("partition"))
	if err != nil {
		t.Fatalf("create partition: %v", err)
	}
	if string(partDS.GetLayer()) != "partition" {
		t.Errorf("layer: got %q, want %q", partDS.GetLayer(), "partition")
	}
	t.Logf("partition created at path test_partition/part1")

	// Create a subdirectory inside the partition.
	childDS, err := dir.CreateOrOpen(db, []string{"test_partition", "part1", "child"}, nil)
	if err != nil {
		t.Fatalf("create child in partition: %v", err)
	}
	t.Logf("child subspace prefix: %x", childDS.Bytes())

	// Write data in the child subspace.
	_, err = db.Transact(func(tr gofdb.WritableTransaction) (any, error) {
		tr.Set(childDS.Pack(tuple.Tuple{"hello"}), []byte("world"))
		return nil, nil
	})
	if err != nil {
		t.Fatalf("write: %v", err)
	}

	// Read it back.
	result, err := db.Transact(func(tr gofdb.WritableTransaction) (any, error) {
		return tr.Get(childDS.Pack(tuple.Tuple{"hello"})).MustGet(), nil
	})
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(result.([]byte)) != "world" {
		t.Fatalf("got %q, want %q", result, "world")
	}

	// List the partition's contents — should see "child".
	children, err := dir.List(db, []string{"test_partition", "part1"})
	if err != nil {
		t.Fatalf("list partition: %v", err)
	}
	if len(children) != 1 || children[0] != "child" {
		t.Errorf("list: got %v, want [child]", children)
	}

	// Verify the partition exists.
	exists, err := dir.Exists(db, []string{"test_partition", "part1"})
	if err != nil {
		t.Fatalf("exists: %v", err)
	}
	if !exists {
		t.Error("partition should exist")
	}

	// Verify the child inside the partition exists.
	exists, err = dir.Exists(db, []string{"test_partition", "part1", "child"})
	if err != nil {
		t.Fatalf("child exists: %v", err)
	}
	if !exists {
		t.Error("child in partition should exist")
	}

	// Clean up: remove the partition (should remove child too).
	removed, err := dir.Remove(db, []string{"test_partition", "part1"})
	if err != nil {
		t.Fatalf("remove partition: %v", err)
	}
	if !removed {
		t.Error("expected partition to be removed")
	}

	// Verify the child is gone after partition removal.
	exists, err = dir.Exists(db, []string{"test_partition", "part1", "child"})
	if err != nil {
		t.Fatalf("child exists after remove: %v", err)
	}
	if exists {
		t.Error("child should not exist after partition removal")
	}

	// Clean up parent.
	dir.Remove(db, []string{"test_partition"})
}

// TestDirectoryLayerPartitionIsolation verifies that a partition's
// subdirectory namespace is isolated from the root directory layer.
func TestDirectoryLayerPartitionIsolation(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)
	dir := directory.Root()

	// Create a regular directory and a partition at the same level.
	_, err := dir.CreateOrOpen(db, []string{"iso_test", "regular"}, nil)
	if err != nil {
		t.Fatalf("create regular: %v", err)
	}

	_, err = dir.CreateOrOpen(db, []string{"iso_test", "partitioned"}, []byte("partition"))
	if err != nil {
		t.Fatalf("create partition: %v", err)
	}

	// Create "foo" inside the partition.
	_, err = dir.CreateOrOpen(db, []string{"iso_test", "partitioned", "foo"}, nil)
	if err != nil {
		t.Fatalf("create foo in partition: %v", err)
	}

	// List the partition — should only show "foo", not "regular".
	partChildren, err := dir.List(db, []string{"iso_test", "partitioned"})
	if err != nil {
		t.Fatalf("list partition: %v", err)
	}
	for _, c := range partChildren {
		if c == "regular" {
			t.Error("partition should NOT see 'regular' from parent layer")
		}
	}
	if len(partChildren) != 1 || partChildren[0] != "foo" {
		t.Errorf("partition children: got %v, want [foo]", partChildren)
	}

	// List the parent — should show "regular" and "partitioned", not "foo".
	parentChildren, err := dir.List(db, []string{"iso_test"})
	if err != nil {
		t.Fatalf("list parent: %v", err)
	}
	found := map[string]bool{}
	for _, c := range parentChildren {
		found[c] = true
	}
	if !found["regular"] || !found["partitioned"] {
		t.Errorf("parent should contain regular and partitioned, got %v", parentChildren)
	}
	if found["foo"] {
		t.Error("parent should NOT see 'foo' from inside partition")
	}

	// Clean up.
	dir.Remove(db, []string{"iso_test"})
}

// TestDirectoryLayerPartitionData verifies that data written in a partition
// subspace is accessible via the partition's allocated prefix.
func TestDirectoryLayerPartitionData(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)
	dir := directory.Root()

	// Create partition, then a child dir.
	_, err := dir.CreateOrOpen(db, []string{"data_test", "part"}, []byte("partition"))
	if err != nil {
		t.Fatalf("create partition: %v", err)
	}

	childDS, err := dir.CreateOrOpen(db, []string{"data_test", "part", "store"}, nil)
	if err != nil {
		t.Fatalf("create child: %v", err)
	}

	// Write data at child.
	key := childDS.Pack(tuple.Tuple{"record", int64(42)})
	_, err = db.Transact(func(tr gofdb.WritableTransaction) (any, error) {
		tr.Set(key, []byte("data42"))
		return nil, nil
	})
	if err != nil {
		t.Fatalf("write: %v", err)
	}

	// Read back via the same path — should get the same subspace.
	childDS2, err := dir.Open(db, []string{"data_test", "part", "store"}, nil)
	if err != nil {
		t.Fatalf("open child: %v", err)
	}

	if !bytes.Equal(childDS.Bytes(), childDS2.Bytes()) {
		t.Fatalf("prefix changed: first=%x second=%x", childDS.Bytes(), childDS2.Bytes())
	}

	// Verify data via reopened subspace.
	result, err := db.Transact(func(tr gofdb.WritableTransaction) (any, error) {
		return tr.Get(childDS2.Pack(tuple.Tuple{"record", int64(42)})).MustGet(), nil
	})
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(result.([]byte)) != "data42" {
		t.Fatalf("data mismatch: got %q, want %q", result, "data42")
	}

	// Clean up.
	dir.Remove(db, []string{"data_test"})
}

// TestDirectoryLayerPartitionPanics verifies that trying to use a partition
// root as a subspace panics, matching the Python/Java behavior.
func TestDirectoryLayerPartitionPanics(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)
	dir := directory.Root()

	partDS, err := dir.CreateOrOpen(db, []string{"panic_test", "part"}, []byte("partition"))
	if err != nil {
		t.Fatalf("create partition: %v", err)
	}

	// Using the partition root as a subspace should panic.
	tests := []struct {
		name string
		fn   func()
	}{
		{"Bytes", func() { partDS.Bytes() }},
		{"Sub", func() { partDS.Sub("x") }},
		{"Pack", func() { partDS.Pack(tuple.Tuple{"x"}) }},
		{"FDBKey", func() { partDS.FDBKey() }},
		{"FDBRangeKeys", func() { partDS.FDBRangeKeys() }},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			defer func() {
				if r := recover(); r == nil {
					t.Errorf("%s on partition root should panic", tt.name)
				}
			}()
			tt.fn()
		})
	}

	// Clean up.
	dir.Remove(db, []string{"panic_test"})
}
