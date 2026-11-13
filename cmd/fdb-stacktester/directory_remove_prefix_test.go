//go:build bazelrunfiles

package main

import (
	"bytes"
	"context"
	"fmt"
	"testing"

	"fdb.dev/pkg/fdbgo/client"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"github.com/apple/foundationdb/bindings/go/src/fdb/tuple"
)

func executeDirectoryArguments(t *testing.T, ctx context.Context, sm *StackMachine, op string, args ...any) {
	t.Helper()
	for i := len(args) - 1; i >= 0; i-- {
		sm.push(i, args[i])
	}
	if err := sm.execute(ctx, 99, op, nil); err != nil {
		t.Fatal(err)
	}
}

func requireDirectoryStack(t *testing.T, sm *StackMachine, wantError bool) {
	t.Helper()
	if wantError {
		if len(sm.stack) != 1 || !bytes.Equal(sm.popBytes(), []byte("DIRECTORY_ERROR")) {
			t.Fatalf("stack=%v want DIRECTORY_ERROR", sm.stack)
		}
	} else if len(sm.stack) != 0 {
		t.Fatalf("successful directory operation changed stack: %v", sm.stack)
	}
}

func TestDirectoryRemoveMatchesPythonAbsentAndErrorSemantics(t *testing.T) {
	t.Parallel()
	db, ctx := stacktesterDatabase(t)
	for _, op := range []string{"DIRECTORY_REMOVE", "DIRECTORY_REMOVE_IF_EXISTS"} {
		for _, suffix := range []string{"", "_DATABASE"} {
			for _, state := range []string{"missing", "existing", "stale", "root", "non-directory"} {
				name := op + suffix + "/" + state
				t.Run(name, func(t *testing.T) {
					sm := NewStackMachine(db, []byte(name))
					root := sm.currentDir().dir
					path := []string{"remove-fixtures", name}
					if state == "existing" || state == "stale" {
						d, err := root.CreateOrOpen(sm.fdbDB(ctx), path, nil)
						if err != nil {
							t.Fatal(err)
						}
						if state == "stale" {
							if removed, err := root.Remove(sm.fdbDB(ctx), path); err != nil || !removed {
								t.Fatalf("remove setup=%v,%v", removed, err)
							}
							sm.dirList[0] = dirEntry{dir: d, dsb: d, ss: d}
						}
					}
					if state == "non-directory" {
						sm.dirList[0] = dirEntry{ss: subspace.FromBytes([]byte("raw-subspace"))}
					}
					if state == "root" || state == "stale" || state == "non-directory" {
						executeDirectoryArguments(t, ctx, sm, op+suffix, int64(0))
					} else {
						executeDirectoryArguments(t, ctx, sm, op+suffix, int64(1), int64(2), path[0], path[1])
					}
					wantError := state == "root" || state == "non-directory" || op == "DIRECTORY_REMOVE" && (state == "missing" || state == "stale")
					requireDirectoryStack(t, sm, wantError)
					if err := sm.currentTr().Commit(ctx); err != nil {
						t.Fatal(err)
					}
					exists, err := root.Exists(sm.fdbDB(ctx), path)
					if err != nil {
						t.Fatal(err)
					}
					if exists {
						t.Fatal("remove left its target directory present")
					}
				})
			}
		}
	}
	for _, op := range []string{"DIRECTORY_REMOVE", "DIRECTORY_REMOVE_IF_EXISTS"} {
		t.Run(op+"/cancelled", func(t *testing.T) {
			sm := NewStackMachine(db, []byte(op))
			sm.currentTr().Cancel()
			executeDirectoryArguments(t, ctx, sm, op, int64(1), int64(1), "missing")
			requireDirectoryStack(t, sm, true)
		})
	}
}

func TestDirectoryCreateDistinguishesEmptyPrefixFromNone(t *testing.T) {
	t.Parallel()
	db, ctx := stacktesterDatabase(t)
	for _, suffix := range []string{"", "_DATABASE"} {
		for _, explicitEmpty := range []bool{false, true} {
			name := fmt.Sprintf("create%s/empty=%v", suffix, explicitEmpty)
			t.Run(name, func(t *testing.T) {
				sm := NewStackMachine(db, []byte(name))
				var prefix any
				if explicitEmpty {
					prefix = []byte{}
				}
				// Decode the real instruction bytes: Apple tuple decoding turns
				// b'' into a typed nil []byte, which must remain distinct from None.
				inst, err := tuple.Unpack(tuple.Tuple{"PUSH", prefix}.Pack())
				if err != nil {
					t.Fatal(err)
				}
				sm.push(0, inst[1])
				sm.push(1, []byte{})
				sm.push(2, name)
				sm.push(3, int64(1))
				if err := sm.execute(ctx, 4, "DIRECTORY_CREATE"+suffix, nil); err != nil {
					t.Fatal(err)
				}
				requireDirectoryStack(t, sm, explicitEmpty)
				if len(sm.dirList) != 2 || sm.dirList[1].isNull() != explicitEmpty {
					t.Fatalf("created directory slot=%v want null=%v", sm.dirList[1], explicitEmpty)
				}
				if err := sm.currentTr().Commit(ctx); err != nil {
					t.Fatal(err)
				}
				exists, err := sm.dirList[0].dir.Exists(sm.fdbDB(ctx), []string{name})
				if err != nil {
					t.Fatal(err)
				}
				if exists == explicitEmpty {
					t.Fatalf("exists=%v explicitEmpty=%v", exists, explicitEmpty)
				}
				if explicitEmpty {
					// Rejected creation must not leak metadata into later logs.
					prefix := tuple.Tuple{"rejected-create", name}.Pack()
					sm.dirIndex = 1
					executeDirectoryArguments(t, ctx, sm, "DIRECTORY_LOG_DIRECTORY", prefix)
					requireDirectoryStack(t, sm, true)
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
					if len(result.([]client.KeyValue)) != 0 {
						t.Fatal("rejected directory emitted metadata logs")
					}
				}
			})
		}
	}
}

func TestDirectoryEmptyLayerRemainsOpenWildcard(t *testing.T) {
	t.Parallel()
	db, ctx := stacktesterDatabase(t)
	for _, op := range []string{"DIRECTORY_OPEN", "DIRECTORY_CREATE_OR_OPEN"} {
		for _, suffix := range []string{"", "_DATABASE"} {
			for _, layer := range []any{nil, []byte{}, []byte("different")} {
				name := fmt.Sprintf("%s%s/%v", op, suffix, layer)
				t.Run(name, func(t *testing.T) {
					sm := NewStackMachine(db, []byte(name))
					if _, err := sm.currentDir().dir.CreateOrOpen(sm.fdbDB(ctx), []string{name}, []byte("custom-layer")); err != nil {
						t.Fatal(err)
					}
					executeDirectoryArguments(t, ctx, sm, op+suffix, int64(1), name, layer)
					wantError := layer != nil && len(layer.([]byte)) != 0
					requireDirectoryStack(t, sm, wantError)
					if len(sm.dirList) != 2 || sm.dirList[1].isNull() != wantError {
						t.Fatal("incorrect appended directory slot")
					}
				})
			}
		}
	}
	// Explicit empty prefixes are distinct even when passed as strings.
	sm := &StackMachine{}
	for _, empty := range []any{[]byte(nil), []byte{}, ""} {
		sm.push(0, empty)
		if got := sm.popBytesOrNil(); got == nil || len(got) != 0 {
			t.Fatalf("empty bytes=%v must be non-nil", got)
		}
	}
	sm.push(0, nil)
	if got := sm.popBytesOrNil(); got != nil {
		t.Fatalf("None=%v wantnil", got)
	}
}
