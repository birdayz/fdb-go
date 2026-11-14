//go:build bazelrunfiles

package main

import (
	"bytes"
	"strings"
	"testing"

	"fdb.dev/pkg/fdbgo/client"
	"github.com/apple/foundationdb/bindings/go/src/fdb/tuple"
)

func TestDirectoryCreatorPanicRecoveryAllOperations(t *testing.T) {
	t.Parallel()
	db, ctx := stacktesterDatabase(t)
	for _, op := range []string{
		"DIRECTORY_CREATE_SUBSPACE",
		"DIRECTORY_CREATE_LAYER",
		"DIRECTORY_CREATE_OR_OPEN",
		"DIRECTORY_CREATE",
		"DIRECTORY_OPEN",
		"DIRECTORY_MOVE",
		"DIRECTORY_MOVE_TO",
		"DIRECTORY_OPEN_SUBSPACE",
	} {
		t.Run(op, func(t *testing.T) {
			sm := NewStackMachine(db, []byte(op))
			// Every creator first pops an integer; a string forces panic recovery.
			executeDirectoryArguments(t, ctx, sm, op, "not-an-integer")
			requireDirectoryStack(t, sm, true)
			if len(sm.dirList) != 2 || !sm.dirList[1].isNull() {
				t.Fatalf("panicking creator left %d slots, want2 with null slot1", len(sm.dirList))
			}
			executeDirectoryArguments(t, ctx, sm, "DIRECTORY_CHANGE", int64(1))
			if sm.dirIndex != 0 {
				t.Fatalf("null slot selected index%d instead of error index0", sm.dirIndex)
			}
			// Python assigns the next creator index2, independent of observed length.
			prefix := []byte("after-panic/")
			executeDirectoryArguments(t, ctx, sm, "DIRECTORY_CREATE_SUBSPACE", int64(0), prefix)
			requireDirectoryStack(t, sm, false)
			if len(sm.dirList) != 3 || sm.dirList[2].ss == nil {
				t.Fatal("following creator did not occupy slot2")
			}
			executeDirectoryArguments(t, ctx, sm, "DIRECTORY_CHANGE", int64(2))
			requireDirectoryStack(t, sm, false)
			if sm.dirIndex != 2 || !bytes.Equal(sm.currentDir().ss.Bytes(), prefix) {
				t.Fatal("following subspace could not be selected at index2")
			}
		})
	}
}

// Seed 1989826954 instruction 1419 opens a subspace on a partition root: DIRECTORY_ERROR.
// Omitting Python's reserved null slot shifts every subsequent directory index.
func TestDirectoryCreatorPanicReservesSlotAndPreservesLaterIndexes(t *testing.T) {
	t.Parallel()
	db, ctx := stacktesterDatabase(t)
	sm := NewStackMachine(db, []byte("partition-panic"))
	partition, err := sm.currentDir().dir.CreateOrOpen(sm.fdbDB(ctx), []string{"partition"}, []byte("partition"))
	if err != nil {
		t.Fatal(err)
	}
	sm.appendDir(dirEntry{dir: partition, dsb: partition, ss: partition})
	sm.dirIndex = 1
	executeDirectoryArguments(t, ctx, sm, "DIRECTORY_OPEN_SUBSPACE", int64(1), "child")
	requireDirectoryStack(t, sm, true)
	if len(sm.dirList) != 3 || !sm.dirList[2].isNull() {
		t.Fatalf("panicking creator left %d slots, want3 with null slot2", len(sm.dirList))
	}
	executeDirectoryArguments(t, ctx, sm, "DIRECTORY_CHANGE", int64(2))
	if sm.dirIndex != 0 {
		t.Fatalf("null slot selected index%d instead of error index0", sm.dirIndex)
	}
	sm.dirIndex = 1
	executeDirectoryArguments(t, ctx, sm, "DIRECTORY_PACK_KEY", int64(0))
	requireDirectoryStack(t, sm, true)
	if len(sm.dirList) != 3 {
		t.Fatal("non-creator panic appended a directory slot")
	}

	// Allocate exactly the slots later instructions address, without deriving
	// their indexes from the observed list length.
	executeDirectoryArguments(t, ctx, sm, "DIRECTORY_CREATE_SUBSPACE", int64(0), []byte("panic-layer/nodes/"))
	executeDirectoryArguments(t, ctx, sm, "DIRECTORY_CREATE_SUBSPACE", int64(0), []byte("panic-layer/content/"))
	requireDirectoryStack(t, sm, false)
	if len(sm.dirList) != 5 || sm.dirList[3].ss == nil || sm.dirList[4].ss == nil {
		t.Fatal("raw subspace indexes drifted")
	}
	// A partition also rejects use as a layer's node subspace through Bytes().
	executeDirectoryArguments(t, ctx, sm, "DIRECTORY_CREATE_LAYER", int64(1), int64(4), int64(0))
	requireDirectoryStack(t, sm, true)
	if len(sm.dirList) != 6 || !sm.dirList[5].isNull() {
		t.Fatal("failed layer constructor did not reserve slot5")
	}
	executeDirectoryArguments(t, ctx, sm, "DIRECTORY_CREATE_LAYER", int64(3), int64(4), int64(0))
	requireDirectoryStack(t, sm, false)
	if len(sm.dirList) != 7 || sm.dirList[6].dir == nil {
		t.Fatal("later layer constructor used shifted indexes")
	}
	executeDirectoryArguments(t, ctx, sm, "DIRECTORY_CHANGE", int64(6))
	executeDirectoryArguments(t, ctx, sm, "DIRECTORY_CREATE_OR_OPEN", int64(1), "child", nil)
	requireDirectoryStack(t, sm, false)
	if len(sm.dirList) != 8 || sm.dirList[7].dir == nil {
		t.Fatal("later directory creation failed")
	}
	if err := sm.currentTr().Commit(ctx); err != nil {
		t.Fatal(err)
	}

	prefix := tuple.Tuple{"panic-layer", "directory-log"}.Pack()
	executeDirectoryArguments(t, ctx, sm, "DIRECTORY_LOG_DIRECTORY", prefix)
	requireDirectoryStack(t, sm, false)
	if err := sm.currentTr().Commit(ctx); err != nil {
		t.Fatal(err)
	}
	result, err := db.Transact(ctx, func(tx *client.Transaction) (any, error) {
		kvs, _, err := tx.GetRange(ctx, prefix, strinc(prefix), 100)
		return kvs, err
	})
	if err != nil {
		t.Fatal(err)
	}
	kvs := result.([]client.KeyValue)
	if len(kvs) != 4 {
		t.Fatalf("later layer emitted%d log fields,want4", len(kvs))
	}
	for _, kv := range kvs {
		key, err := tuple.Unpack(kv.Key[len(prefix):])
		if err != nil {
			t.Fatal(err)
		}
		if key[0] != int64(6) {
			t.Fatalf("log directory index=%v want6", key[0])
		}
		switch key[1] {
		case "children":
			if !bytes.Equal(kv.Value, tuple.Tuple{"child"}.Pack()) {
				t.Fatalf("children=%x", kv.Value)
			}
		case "exists":
			if !bytes.Equal(kv.Value, []byte{0x15, 1}) {
				t.Fatalf("exists=%x", kv.Value)
			}
		}
	}
}

func TestDirectoryMoveToShortAbsolutePathReturnsErrorWithoutChanges(t *testing.T) {
	t.Parallel()
	db, ctx := stacktesterDatabase(t)
	sm := NewStackMachine(db, []byte("move-short-path"))
	partition, err := sm.currentDir().dir.CreateOrOpen(sm.fdbDB(ctx), []string{"outer", "partition"}, []byte("partition"))
	if err != nil {
		t.Fatal(err)
	}
	child, err := partition.CreateOrOpen(sm.fdbDB(ctx), []string{"child"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	dataKey := append(append([]byte{}, child.Bytes()...), []byte("data")...)
	_, err = db.Transact(ctx, func(tx *client.Transaction) (any, error) { tx.Set(dataKey, []byte("unchanged")); return nil, nil })
	if err != nil {
		t.Fatal(err)
	}
	for _, destination := range [][]string{nil, {"outer"}, {"other", "partition", "moved"}} {
		// Call the facade directly: stacktester recovery must not hide a panic.
		if _, err := child.MoveTo(sm.fdbDB(ctx), destination); err == nil || !strings.Contains(err.Error(), "cannot move between partitions") {
			t.Fatalf("MoveTo(%v)=%v want cross-partition error", destination, err)
		}
		exists, err := partition.Exists(sm.fdbDB(ctx), []string{"child"})
		if err != nil || !exists {
			t.Fatalf("failed move changed original directory: exists=%v err=%v", exists, err)
		}
		exists, err = partition.Exists(sm.fdbDB(ctx), []string{"moved"})
		if err != nil || exists {
			t.Fatalf("failed move created destination: exists=%v err=%v", exists, err)
		}
		result, err := db.Transact(ctx, func(tx *client.Transaction) (any, error) { return tx.Get(ctx, dataKey) })
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(result.([]byte), []byte("unchanged")) {
			t.Fatalf("failed move changed contents: %q", result)
		}
	}
	// A valid absolute destination in the same partition still succeeds.
	moved, err := child.MoveTo(sm.fdbDB(ctx), []string{"outer", "partition", "moved"})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(moved.Bytes(), child.Bytes()) {
		t.Fatal("move changed physical content prefix")
	}
	if exists, err := partition.Exists(sm.fdbDB(ctx), []string{"child"}); err != nil || exists {
		t.Fatalf("old name exists=%v err=%v", exists, err)
	}
	if exists, err := partition.Exists(sm.fdbDB(ctx), []string{"moved"}); err != nil || !exists {
		t.Fatalf("new name exists=%v err=%v", exists, err)
	}
}
