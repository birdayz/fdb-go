//go:build bazelrunfiles

package main

import (
	"bytes"
	"math/big"
	"reflect"
	"testing"

	"fdb.dev/pkg/fdbgo/client"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"github.com/apple/foundationdb/bindings/go/src/fdb/tuple"
)

// DIRECTORY_UNPACK_KEY differs from TUPLE_UNPACK: Python pushes each raw
// value, not its packed one-element tuple. LOG_STACK does the one required pack.
func TestDirectoryUnpackKeyPreservesRawValuesAndLogBytes(t *testing.T) {
	t.Parallel()
	db, ctx := stacktesterDatabase(t)
	sm := NewStackMachine(db, []byte("directory-unpack"))
	ss := subspace.FromBytes([]byte("directory-unpack/subspace/"))
	sm.dirList[0] = dirEntry{ss: ss}
	large := new(big.Int).Lsh(big.NewInt(1), 100)
	stamp := tuple.Versionstamp{TransactionVersion: [10]byte{1, 2, 3}, UserVersion: 0x1234}
	values := tuple.Tuple{
		nil,
		[]byte{0, 1, 0xff},
		"unicode\u9348\x00", int64(0), int64(-123), int64(456), large,
		float32(1.25), float64(-3.5), false, true,
		tuple.UUID{1, 2, 3},
		stamp,
		tuple.Tuple{nil, []byte("nested"), tuple.UUID{4}, tuple.Tuple{stamp, int64(-7)}},
	}
	key := append(append([]byte{}, ss.Bytes()...), values.Pack()...)
	sm.push(1, key)
	if err := sm.execute(ctx, 42, "DIRECTORY_UNPACK_KEY", nil); err != nil {
		t.Fatal(err)
	}
	if len(sm.stack) != len(values) {
		t.Fatalf("unpacked depth=%d want%d", len(sm.stack), len(values))
	}
	for i, want := range values {
		got := sm.stack[i]
		if got.idx != 42 || !reflect.DeepEqual(got.value, want) {
			t.Fatalf("unpacked[%d]=(%T,%v) @%d want (%T,%v) @42", i, got.value, got.value, got.idx, want, want)
		}
	}
	prefix := tuple.Tuple{"directory-unpack", "log"}.Pack()
	sm.push(43, prefix)
	if err := sm.execute(ctx, 44, "LOG_STACK", nil); err != nil {
		t.Fatal(err)
	}
	result, err := db.Transact(ctx, func(tx *client.Transaction) (any, error) {
		kvs, more, err := tx.GetRange(ctx, prefix, strinc(prefix), 100)
		if more {
			t.Error("truncated log")
		}
		return kvs, err
	})
	if err != nil {
		t.Fatal(err)
	}
	kvs := result.([]client.KeyValue)
	if len(kvs) != len(values) {
		t.Fatalf("log entries=%d want%d", len(kvs), len(values))
	}
	for i, kv := range kvs {
		wantKey := append(append([]byte{}, prefix...), tuple.Tuple{int64(i), int64(42)}.Pack()...)
		wantValue := tuple.Tuple{values[i]}.Pack()
		if !bytes.Equal(kv.Key, wantKey) || !bytes.Equal(kv.Value, wantValue) {
			t.Fatalf("log[%d]=%x:%x want%x:%x", i, kv.Key, kv.Value, wantKey, wantValue)
		}
	}
	// Literal encodings independently pin representative type markers, not a
	// comparison where both sides could accidentally derive a double pack.
	for index, want := range map[int][]byte{0: {0}, 1: {1, 0, 0xff, 1, 0xff, 0}, 3: {0x14}, 9: {0x26}, 10: {0x27}} {
		if !bytes.Equal(kvs[index].Value, want) {
			t.Fatalf("literal log[%d]=%x want%x", index, kvs[index].Value, want)
		}
	}
	for _, bad := range []struct {
		name  string
		entry dirEntry
		key   []byte
	}{
		{"outside subspace", dirEntry{ss: ss}, []byte("outside")},
		{"invalid entry", dirEntry{}, key},
	} {
		t.Run(bad.name, func(t *testing.T) {
			sm.dirList[0] = bad.entry
			sm.push(1, bad.key)
			if err := sm.execute(ctx, 2, "DIRECTORY_UNPACK_KEY", nil); err != nil {
				t.Fatal(err)
			}
			if len(sm.stack) != 1 || !bytes.Equal(sm.popBytes(), []byte("DIRECTORY_ERROR")) {
				t.Fatalf("error stack=%v", sm.stack)
			}
		})
	}
}
