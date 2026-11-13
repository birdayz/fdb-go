//go:build bazelrunfiles

package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"testing"
	"time"

	"fdb.dev/pkg/fdbgo/client"
	gofdb "fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/directory"
	"github.com/apple/foundationdb/bindings/go/src/fdb/tuple"
)

// API selection is process-global and immutable; exercise each layout in an
// isolated copy of this Bazel test binary without selecting the native C API.
func TestTupleVersionstampUsesPureGoAPIVersion(t *testing.T) {
	if version := os.Getenv("STACKTESTER_VERSIONSTAMP_TEST_API"); version != "" {
		api, err := strconv.Atoi(version)
		if err != nil {
			t.Fatal(err)
		}
		if err := gofdb.APIVersion(api); err != nil {
			t.Fatal(err)
		}
		testVersionstampPacking(t, api)
		return
	}
	t.Parallel()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, api := range []int{510, 610, 730} {
		t.Run(strconv.Itoa(api), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, exe, "-test.run=^TestTupleVersionstampUsesPureGoAPIVersion$", "-test.v")
			cmd.Env = append(os.Environ(), fmt.Sprintf("STACKTESTER_VERSIONSTAMP_TEST_API=%d", api))
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("API%d: %v\n%s", api, err, out)
			}
		})
	}
}

func testVersionstampPacking(t *testing.T, api int) {
	t.Helper()
	ctx := context.Background()
	prefix := []byte{0x61, 0, 0xff}
	stamp := tuple.IncompleteVersionstamp(0x1234)
	complete := tuple.Versionstamp{TransactionVersion: [10]byte{1}, UserVersion: 2}
	for _, tc := range []struct {
		name   string
		items  tuple.Tuple
		status string
		body   []byte
		offset uint32
	}{
		{"none", tuple.Tuple{tuple.UUID{1}, tuple.Tuple{complete}}, "ERROR: NONE", nil, 0},
		{"multiple", tuple.Tuple{stamp, tuple.Tuple{stamp}}, "ERROR: MULTIPLE", nil, 0},
		{"single", tuple.Tuple{stamp}, "OK", append([]byte{0x33}, stamp.Bytes()...), uint32(len(prefix) + 1)},
		{"nested", tuple.Tuple{tuple.Tuple{tuple.UUID{1}, stamp}}, "OK", append(append(append([]byte{0x05, 0x30}, makeUUIDBytes()...), append([]byte{0x33}, stamp.Bytes()...)...), 0), uint32(len(prefix) + 19)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sm := &StackMachine{}
			for i := len(tc.items) - 1; i >= 0; i-- {
				sm.push(i, tc.items[i])
			}
			sm.push(10, int64(len(tc.items)))
			sm.push(11, prefix)
			if err := sm.execute(ctx, 12, "TUPLE_PACK_WITH_VERSIONSTAMP", nil); err != nil {
				t.Fatal(err)
			}
			if tc.status != "OK" {
				if len(sm.stack) != 1 || !bytes.Equal(sm.popBytes(), []byte(tc.status)) {
					t.Fatalf("status stack=%v want%s", sm.stack, tc.status)
				}
				return
			}
			if len(sm.stack) != 2 {
				t.Fatalf("success stack depth=%d want2", len(sm.stack))
			}
			got := sm.popBytes()
			want := append(append([]byte{}, prefix...), tc.body...)
			var trailer [4]byte
			binary.LittleEndian.PutUint32(trailer[:], tc.offset)
			width := 4
			if api < 520 {
				width = 2
			}
			want = append(want, trailer[:width]...)
			if !bytes.Equal(got, want) {
				t.Fatalf("packed=%x want%x", got, want)
			}
			if !bytes.Equal(sm.popBytes(), []byte("OK")) {
				t.Fatal("missing OK")
			}
		})
	}
}

func makeUUIDBytes() []byte {
	b := make([]byte, 16)
	b[0] = 1
	return b
}

type logDirectoryProbe struct {
	directory.Directory
	exists               bool
	existsErr, errorList error
	calls                *[]string
}

func (d logDirectoryProbe) Exists(gofdb.ReadTransactor, []string) (bool, error) {
	*d.calls = append(*d.calls, "exists")
	return d.exists, d.existsErr
}

func (d logDirectoryProbe) List(gofdb.ReadTransactor, []string) ([]string, error) {
	*d.calls = append(*d.calls, "list")
	return []string{"must not log when absent"}, d.errorList
}

func TestLogDirectoryReadsBeforeWriting(t *testing.T) {
	t.Parallel()
	db, ctx := stacktesterDatabase(t)
	for _, tc := range []struct {
		name               string
		exists             bool
		existsErr, listErr error
		wantError          bool
		calls              int
	}{
		{"absent", false, nil, nil, false, 1},
		{"exists error", false, errors.New("read failed"), nil, true, 1},
		{"list error", true, nil, errors.New("list failed"), true, 2},
		{"existing", true, nil, nil, false, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sm := NewStackMachine(db, []byte(tc.name))
			var calls []string
			sm.dirList[0].dir = logDirectoryProbe{Directory: sm.dirList[0].dir, exists: tc.exists, existsErr: tc.existsErr, errorList: tc.listErr, calls: &calls}
			prefix := tuple.Tuple{"directory-probe", tc.name}.Pack()
			sm.push(1, prefix)
			if err := sm.execute(ctx, 2, "DIRECTORY_LOG_DIRECTORY", nil); err != nil {
				t.Fatal(err)
			}
			if len(calls) != tc.calls || calls[0] != "exists" || tc.calls == 2 && calls[1] != "list" {
				t.Fatalf("calls=%v", calls)
			}
			if tc.wantError {
				if len(sm.stack) != 1 || !bytes.Equal(sm.popBytes(), []byte("DIRECTORY_ERROR")) {
					t.Fatalf("error stack=%v", sm.stack)
				}
			} else if len(sm.stack) != 0 {
				t.Fatalf("success stack=%v", sm.stack)
			}
			// Commit even the failing operation: partial writes would become visible.
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
			if tc.wantError {
				if len(kvs) != 0 {
					t.Fatalf("partial directory log=%v", kvs)
				}
				return
			}
			if len(kvs) != 4 {
				t.Fatalf("log fields=%d want4", len(kvs))
			}
			for _, kv := range kvs {
				key, err := tuple.Unpack(kv.Key[len(prefix):])
				if err != nil {
					t.Fatal(err)
				}
				if key[1] == "children" {
					want := []byte{}
					if tc.exists {
						want = tuple.Tuple{"must not log when absent"}.Pack()
					}
					if !bytes.Equal(kv.Value, want) {
						t.Fatalf("children=%x want%x", kv.Value, want)
					}
				}
			}
		})
	}
}

func TestLogDirectoryStaleAndInvalidEntries(t *testing.T) {
	t.Parallel()
	db, ctx := stacktesterDatabase(t)
	sm := NewStackMachine(db, []byte("stale-directory"))
	root := sm.currentDir().dir
	stale, err := root.CreateOrOpen(sm.fdbDB(), []string{"removed"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := stale.CreateOrOpen(sm.fdbDB(), []string{"child"}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := root.Remove(sm.fdbDB(), []string{"removed"}); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name      string
		entry     dirEntry
		wantError bool
	}{
		{"stale", dirEntry{dir: stale, dsb: stale}, false},
		{"invalid", dirEntry{}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sm := NewStackMachine(db, []byte(tc.name))
			sm.dirList[0] = tc.entry
			prefix := tuple.Tuple{"directory-stale", tc.name}.Pack()
			sm.push(1, prefix)
			if err := sm.execute(ctx, 2, "DIRECTORY_LOG_DIRECTORY", nil); err != nil {
				t.Fatal(err)
			}
			if tc.wantError {
				if len(sm.stack) != 1 || !bytes.Equal(sm.popBytes(), []byte("DIRECTORY_ERROR")) {
					t.Fatalf("stack=%v", sm.stack)
				}
			} else if len(sm.stack) != 0 {
				t.Fatalf("stack=%v", sm.stack)
			}
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
			if tc.wantError {
				if len(kvs) != 0 {
					t.Fatal("invalid directory logged fields")
				}
				return
			}
			if len(kvs) != 4 {
				t.Fatalf("stale log fields=%d", len(kvs))
			}
			for _, kv := range kvs {
				key, err := tuple.Unpack(kv.Key[len(prefix):])
				if err != nil {
					t.Fatal(err)
				}
				switch key[1] {
				case "exists":
					if !bytes.Equal(kv.Value, tuple.Tuple{int64(0)}.Pack()) {
						t.Fatalf("stale exists=%x", kv.Value)
					}
				case "children":
					if len(kv.Value) != 0 {
						t.Fatalf("stale children=%x", kv.Value)
					}
				}
			}
		})
	}
}
