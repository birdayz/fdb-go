//go:build bazelrunfiles

package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"fdb.dev/pkg/fdbgo/client"
	foundationdb "fdb.dev/pkg/testcontainers/foundationdb"
	"github.com/apple/foundationdb/bindings/go/src/fdb/tuple"
)

func stacktesterDatabase(t *testing.T, opts ...client.Option) (*client.Database, context.Context) {
	t.Helper()
	db, ctx, _ := stacktesterDatabaseWithClusterFile(t, opts...)
	return db, ctx
}

func stacktesterDatabaseWithClusterFile(t *testing.T, opts ...client.Option) (*client.Database, context.Context, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	t.Cleanup(cancel)
	container, err := foundationdb.Run(ctx, "", foundationdb.WithDirectIP())
	if err != nil {
		if foundationdb.SkipWithoutDocker(err) {
			t.Skip("FDB not available (no Docker)")
		}
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := foundationdb.Terminate(cleanup, container); err != nil {
			t.Error(err)
		}
	})
	cluster, err := container.ClusterFile(ctx)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "fdb.cluster")
	if err := os.WriteFile(path, []byte(cluster), 0o600); err != nil {
		t.Fatal(err)
	}
	db, err := client.OpenDatabase(ctx, path, append([]client.Option{client.WithAPIVersion(730)}, opts...)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db, ctx, path
}

// Upstream Python tester.log_stack concatenates the already-packed prefix with
// (stack index, instruction index); packing the prefix again hides every result.
func TestLogStackUsesRawPrefixAndPreservesBatchIndexes(t *testing.T) {
	t.Parallel()
	db, ctx := stacktesterDatabase(t)
	sm := NewStackMachine(db, []byte("instructions"))
	prefix := tuple.Tuple{"tester_output", "stack"}.Pack()
	for i := 0; i < 205; i++ {
		sm.push(i*3+1, []byte{byte(i), 0, 0xff})
	}
	sm.push(1000, prefix)
	if err := sm.execute(ctx, 1001, "LOG_STACK", nil); err != nil {
		t.Fatal(err)
	}
	if len(sm.stack) != 0 {
		t.Fatal("LOG_STACK did not empty operand stack")
	}
	result, err := db.Transact(ctx, func(tx *client.Transaction) (any, error) {
		kvs, more, err := tx.GetRange(ctx, prefix, strinc(prefix), 1000)
		if more {
			t.Error("unexpected truncated result")
		}
		return kvs, err
	})
	if err != nil {
		t.Fatal(err)
	}
	kvs := result.([]client.KeyValue)
	if len(kvs) != 205 {
		t.Fatalf("visible stack entries=%d want205", len(kvs))
	}
	for i, kv := range kvs {
		wantKey := append(append([]byte{}, prefix...), tuple.Tuple{int64(i), int64(i*3 + 1)}.Pack()...)
		wantValue := tuple.Tuple{[]byte{byte(i), 0, 0xff}}.Pack()
		if !bytes.Equal(kv.Key, wantKey) || !bytes.Equal(kv.Value, wantValue) {
			t.Fatalf("entry%d: %x=%x want%x=%x", i, kv.Key, kv.Value, wantKey, wantValue)
		}
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	sm.push(1, []byte("must not silently discard failed logging"))
	sm.push(2, prefix)
	err = sm.execute(cancelled, 3, "LOG_STACK", nil)
	if err == nil || !strings.HasPrefix(err.Error(), "log stack: ") || !errors.Is(err, context.Canceled) {
		t.Fatalf("failed nonempty LOG_STACK lost diagnostic or cause: %v", err)
	}
	if len(sm.stack) != 0 {
		t.Fatalf("LOG_STACK retained operands after failure: %+v", sm.stack)
	}
}

func executeStackOperation(t *testing.T, ctx context.Context, sm *StackMachine, op string, args ...any) []byte {
	t.Helper()
	sm.stack = nil
	for i := len(args) - 1; i >= 0; i-- {
		sm.push(i, args[i])
	}
	if err := sm.execute(ctx, 100, op, nil); err != nil {
		t.Fatal(err)
	}
	if len(sm.stack) != 1 {
		t.Fatalf("%s stack depth=%d want1", op, len(sm.stack))
	}
	return sm.popBytes()
}

func TestRangeStreamingExactRequiresLimit(t *testing.T) {
	t.Parallel()
	db, ctx := stacktesterDatabase(t)
	sm := NewStackMachine(db, []byte("streaming"))
	begin, end := []byte("range/a"), []byte("range/z")
	_, err := db.Transact(ctx, func(tx *client.Transaction) (any, error) {
		tx.Set(begin, []byte("value"))
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, base := range []string{"GET_RANGE", "GET_RANGE_STARTS_WITH", "GET_RANGE_SELECTOR"} {
		for _, suffix := range []string{"", "_SNAPSHOT", "_DATABASE"} {
			for _, tc := range []struct {
				mode, limit int64
				invalid     bool
			}{{0, 0, true}, {0, 1, false}, {-1, 0, false}, {-2, 0, false}} {
				var args []any
				switch base {
				case "GET_RANGE":
					args = []any{begin, end, tc.limit, int64(0), tc.mode}
				case "GET_RANGE_STARTS_WITH":
					args = []any{[]byte("range/"), tc.limit, int64(0), tc.mode}
				case "GET_RANGE_SELECTOR":
					args = []any{begin, int64(0), int64(1), end, int64(0), int64(1), tc.limit, int64(0), tc.mode, []byte{}}
				}
				got := executeStackOperation(t, ctx, sm, base+suffix, args...)
				want := tuple.Tuple{begin, []byte("value")}.Pack()
				if tc.invalid {
					want = tuple.Tuple{[]byte("ERROR"), []byte("2210")}.Pack()
				}
				if !bytes.Equal(got, want) {
					t.Fatalf("%s%s mode%d limit%d got%x want%x", base, suffix, tc.mode, tc.limit, got, want)
				}
			}
		}
	}
	got := executeStackOperation(t, ctx, sm, "ON_ERROR", int64(0))
	want := tuple.Tuple{[]byte("ERROR"), []byte("2000")}.Pack()
	if !bytes.Equal(got, want) {
		t.Fatalf("ON_ERROR(0)=%x want%x", got, want)
	}
}

func TestDatabaseSelectorFiltersAfterLimit(t *testing.T) {
	t.Parallel()
	db, ctx := stacktesterDatabase(t)
	sm := NewStackMachine(db, []byte("selectors"))
	keys := [][]byte{[]byte("selector/a"), []byte("selector/b/1"), []byte("selector/b/2"), []byte("selector/c")}
	_, err := db.Transact(ctx, func(tx *client.Transaction) (any, error) {
		for _, key := range keys {
			tx.Set(key, key)
		}
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		prefix         []byte
		limit, reverse int64
		indexes        []int
	}{
		{[]byte{}, 0, 0, []int{0, 1, 2, 3}},
		{[]byte("selector/b/"), 1, 0, nil},
		{[]byte("selector/b/"), 1, 1, nil},
		{[]byte("selector/b/"), 2, 0, []int{1}},
		{[]byte("selector/b/"), 2, 1, []int{2}},
	} {
		got := executeStackOperation(t, ctx, sm, "GET_RANGE_SELECTOR_DATABASE", keys[0], int64(0), int64(1), keys[3], int64(1), int64(1), tc.limit, tc.reverse, int64(-1), tc.prefix)
		want := tuple.Tuple{}
		for _, i := range tc.indexes {
			want = append(want, keys[i], keys[i])
		}
		if !bytes.Equal(got, want.Pack()) {
			t.Fatalf("prefix%q limit%d reverse%d got%x want%x", tc.prefix, tc.limit, tc.reverse, got, want.Pack())
		}
	}
}

func TestThreadsShareTransactionRegistryOnly(t *testing.T) {
	t.Parallel()
	db, ctx := stacktesterDatabase(t)
	parent := NewStackMachine(db, []byte("parent"))
	parent.dirList = append(parent.dirList, dirEntry{})
	parent.dirIndex, parent.dirErrorIndex = 1, 1
	child := newStackMachine(db, []byte("child"), parent.transactionRegistry)
	if child.currentTr() == nil {
		t.Fatal("child initial transaction missing")
	}
	if len(child.dirList) != 1 || child.dirIndex != 0 || child.dirErrorIndex != 0 {
		t.Fatal("child inherited directory state")
	}
	for _, sm := range []*StackMachine{parent, child} {
		sm.push(0, []byte("shared"))
		if err := sm.execute(ctx, 1, "USE_TRANSACTION", nil); err != nil {
			t.Fatal(err)
		}
	}
	if parent.currentTr() != child.currentTr() {
		t.Fatal("USE_TRANSACTION did not share handle")
	}
	old := parent.currentTr()
	if err := child.execute(ctx, 2, "NEW_TRANSACTION", nil); err != nil {
		t.Fatal(err)
	}
	if parent.currentTr() == old || parent.currentTr() != child.currentTr() {
		t.Fatal("replacement not shared")
	}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		sm := newStackMachine(db, []byte(fmt.Sprintf("worker%d", i)), parent.transactionRegistry)
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 500; j++ {
				sm.push(0, []byte(fmt.Sprintf("%s/%d", sm.prefix, j)))
				if err := sm.execute(ctx, 1, "USE_TRANSACTION", nil); err != nil {
					t.Error(err)
					return
				}
				if err := sm.execute(ctx, 2, "NEW_TRANSACTION", nil); err != nil {
					t.Error(err)
					return
				}
				if err := sm.execute(ctx, 3, "RESET", nil); err != nil {
					t.Error(err)
					return
				}
				if sm.currentTr() == nil {
					t.Error("missing transaction")
					return
				}
			}
		}()
	}
	wg.Wait()
	// Exercise START_THREAD itself without NEW_TRANSACTION in the child.
	prefix := []byte("thread-instructions")
	out := []byte("thread-result")
	parentDirectory, err := parent.dirList[0].dir.CreateOrOpen(parent.fdbDB(ctx), []string{"parent-only"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	parent.dirList[1] = dirEntry{dir: parentDirectory, dsb: parentDirectory, ss: parentDirectory}
	parent.dirIndex, parent.dirErrorIndex = 1, 1
	logPrefixes := [][]byte{[]byte("thread-directory/initial/"), []byte("thread-directory/list/"), []byte("thread-directory/error/")}
	instructions := []tuple.Tuple{
		{"PUSH", []byte("child value")},
		{"PUSH", out},
		{"SET"},
		{"PUSH", logPrefixes[0]},
		{"DIRECTORY_LOG_DIRECTORY"},
		{"PUSH", int64(1)},
		{"DIRECTORY_CHANGE"},
		{"PUSH", logPrefixes[1]},
		{"DIRECTORY_LOG_DIRECTORY"},
		{"PUSH", int64(99)},
		{"DIRECTORY_CHANGE"},
		{"PUSH", logPrefixes[2]},
		{"DIRECTORY_LOG_DIRECTORY"},
		{"COMMIT"},
	}
	_, err = db.Transact(ctx, func(tx *client.Transaction) (any, error) {
		for i, inst := range instructions {
			tx.Set(tuple.Tuple{prefix, int64(i)}.Pack(), inst.Pack())
		}
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	parent.push(0, prefix)
	if err := parent.execute(ctx, 1, "START_THREAD", nil); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { parent.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	value, err := db.Transact(ctx, func(tx *client.Transaction) (any, error) { return tx.Get(ctx, out) })
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(value.([]byte), []byte("child value")) {
		t.Fatalf("child output=%q", value)
	}
	for _, logPrefix := range logPrefixes {
		rows, _, err := db.CreateTransaction().GetRange(ctx, logPrefix, append(bytes.Clone(logPrefix), 0xff), 0)
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) != 4 {
			t.Fatalf("child directory state at %q: got %d log fields, want 4 from fresh root state", logPrefix, len(rows))
		}
		for _, row := range rows {
			fields, err := tuple.Unpack(row.Key[len(logPrefix):])
			if err != nil || len(fields) != 2 || fields[0] != int64(0) {
				t.Fatalf("child directory state at %q: key=%x fields=%v err=%v, want root index0", logPrefix, row.Key, fields, err)
			}
			if fields[1] == "path" && len(row.Value) != 0 {
				t.Fatalf("child directory state at %q: inherited parent path %x", logPrefix, row.Value)
			}
			if fields[1] == "exists" && !bytes.Equal(row.Value, tuple.Tuple{int64(1)}.Pack()) {
				t.Fatalf("child directory root at %q does not exist: %x", logPrefix, row.Value)
			}
		}
	}
}
