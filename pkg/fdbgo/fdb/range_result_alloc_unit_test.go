package fdb

import (
	"context"
	"reflect"
	"testing"
	"unsafe"

	"fdb.dev/pkg/fdbgo/client"
)

func TestKeyValueLayoutMatchesClient(t *testing.T) {
	pub, cli := reflect.TypeOf(KeyValue{}), reflect.TypeOf(client.KeyValue{})
	if pub.Size() != cli.Size() || pub.Align() != cli.Align() || pub.NumField() != cli.NumField() {
		t.Fatalf("KeyValue layout %d/%d/%d differs from client %d/%d/%d",
			pub.Size(), pub.Align(), pub.NumField(), cli.Size(), cli.Align(), cli.NumField())
	}
	bytesType := reflect.TypeOf([]byte(nil))
	for i := range pub.NumField() {
		p, c := pub.Field(i), cli.Field(i)
		if p.Name != c.Name || p.Offset != c.Offset || !p.Type.ConvertibleTo(bytesType) || p.Type.Kind() != c.Type.Kind() || p.Type.Elem() != c.Type.Elem() {
			t.Fatalf("field %d: %s %v @%d differs from client %s %v @%d", i, p.Name, p.Type, p.Offset, c.Name, c.Type, c.Offset)
		}
	}
}

// Not parallel: AllocsPerRun counts process-wide allocations.
func TestOwnedKeyValuesDoesNotCopy(t *testing.T) {
	kvs := []client.KeyValue{{Key: []byte("a"), Value: []byte("1")}, {Key: []byte("b"), Value: nil}}
	var got []KeyValue
	allocs := testing.AllocsPerRun(100, func() { got = ownedKeyValues(kvs) })
	if allocs != 0 {
		t.Fatalf("ALLOC_RANGE_SLICE: %v allocations, want 0", allocs)
	}
	if len(got) != 2 || cap(got) != 2 || unsafe.Pointer(&got[0]) != unsafe.Pointer(&kvs[0]) {
		t.Fatal("ALLOC_RANGE_SLICE: owned rows were copied or resized")
	}
	if string(got[0].Key) != "a" || string(got[0].Value) != "1" || string(got[1].Key) != "b" || got[1].Value != nil {
		t.Fatalf("reinterpreted rows = %q", got)
	}
	if empty := ownedKeyValues(nil); empty == nil || len(empty) != 0 {
		t.Fatal("an empty range must stay a non-nil empty slice")
	}
}

// Not parallel: AllocsPerRun counts process-wide allocations. A locally served
// range (the writes cover it) drives the real GetSliceWithError: its result is
// the client's per-call copy, not a second slice, and each call's result is
// independent of another's.
func TestGetSliceWithErrorReusesOwnedRows(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	inner := &client.Transaction{}
	t.Cleanup(cancel)
	t.Cleanup(inner.Cancel)
	tr := Transaction{t: &transaction{inner: inner, ctx: ctx}}
	r := KeyRange{Begin: Key("a"), End: Key("z")}
	tr.ClearRange(r)
	tr.Set(Key("b"), []byte("1"))
	tr.Set(Key("c"), []byte("2"))
	result := tr.GetRange(r, RangeOptions{})
	first, err := result.GetSliceWithError()
	if err != nil || len(first) != 2 || string(first[0].Key) != "b" || string(first[1].Value) != "2" {
		t.Fatalf("GetSliceWithError = %q, %v", first, err)
	}
	second, _ := result.GetSliceWithError()
	first[0] = KeyValue{Key: Key("x")}
	if &first[0] == &second[0] || string(second[0].Key) != "b" {
		t.Fatal("two results of one range share rows")
	}
	slice := testing.AllocsPerRun(100, func() {
		if rows, err := result.GetSliceWithError(); err != nil || len(rows) != 2 {
			t.Fatalf("GetSliceWithError = %q, %v", rows, err)
		}
	})
	get := testing.AllocsPerRun(100, func() {
		if rows, _, err := result.(goRangeResult).first.pending.Get(); err != nil || len(rows) != 2 {
			t.Fatalf("PendingRange.Get = %q, %v", rows, err)
		}
	})
	if slice != get {
		t.Fatalf("ALLOC_RANGE_SLICE: GetSliceWithError = %v allocations, PendingRange.Get = %v; want equal", slice, get)
	}
}
