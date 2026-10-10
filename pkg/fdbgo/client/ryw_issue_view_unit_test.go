package client

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"testing"

	"fdb.dev/pkg/fdbgo/wire"
)

// A read sees the write map as of its issue (C++ RYWIterator over a WriteMap
// version later writes do not change, WriteMap.h:158-160). These tests change
// the write map while the storage read is in flight.

func issueViewNum(n uint64) []byte {
	b := make([]byte, 8)
	binary.LittleEndian.PutUint64(b, n)
	return b
}

func sameReadResult(gotV []byte, gotErr error, wantV []byte, wantErr error) bool {
	if (gotErr == nil) != (wantErr == nil) {
		return false
	}
	if gotErr != nil {
		var g, w *wire.FDBError
		return errors.As(gotErr, &g) && errors.As(wantErr, &w) && g.Code == w.Code
	}
	return (gotV == nil) == (wantV == nil) && bytes.Equal(gotV, wantV)
}

func TestRYWDependentPointReadUsesIssueTimeStack(t *testing.T) {
	t.Parallel()
	key := []byte("dep/k")
	operand := append([]byte("stamp-"), make([]byte, 14)...)
	issue := map[string]func(c *rywCache){
		"add": func(c *rywCache) { c.atomic(MutAddValue, key, issueViewNum(3)) },
		"add-then-versionstamp-bypassed": func(c *rywCache) {
			c.atomic(MutAddValue, key, issueViewNum(3))
			c.atomic(MutSetVersionstampedValue, key, operand)
			c.setBypassUnreadable(true)
		},
	}
	later := map[string]func(c *rywCache){
		"none":         func(*rywCache) {},
		"set":          func(c *rywCache) { c.set(key, issueViewNum(99)) },
		"clear":        func(c *rywCache) { c.clear(key) },
		"clear-range":  func(c *rywCache) { c.clearRange([]byte("dep/"), []byte("dep0")) },
		"add":          func(c *rywCache) { c.atomic(MutAddValue, key, issueViewNum(100)) },
		"versionstamp": func(c *rywCache) { c.atomic(MutSetVersionstampedValue, key, operand) },
		"bypass-off":   func(c *rywCache) { c.setBypassUnreadable(false); c.set(key, issueViewNum(99)) },
	}
	for issueName, issueOps := range issue {
		for laterName, laterOps := range later {
			t.Run(issueName+"/"+laterName, func(t *testing.T) {
				t.Parallel()
				storage := func(context.Context, []byte) ([]byte, error) { return issueViewNum(10), nil }
				var control rywCache
				issueOps(&control)
				wantV, wantErr := control.get(context.Background(), key, storage)
				expected := operand
				if issueName == "add" {
					expected = issueViewNum(13)
				}
				if wantErr != nil || !bytes.Equal(wantV, expected) {
					t.Fatalf("control %s = %x, %v; want %x", issueName, wantV, wantErr, expected)
				}

				var c rywCache
				issueOps(&c)
				gotV, gotErr := c.get(context.Background(), key, func(ctx context.Context, k []byte) ([]byte, error) {
					laterOps(&c) // a write issued while the storage read is in flight
					return storage(ctx, k)
				})
				if !sameReadResult(gotV, gotErr, wantV, wantErr) {
					t.Fatalf("ISSUE_VIEW_POINT: dependent read = %x, %v; want %x, %v as issued", gotV, gotErr, wantV, wantErr)
				}
				if laterName == "none" {
					c.mu.Lock()
					folded := !c.writes[string(key)].hasAtomics
					c.mu.Unlock()
					if folded {
						t.Fatal("the read folded its result into the write map")
					}
				}
			})
		}
	}
}

func TestRYWGetRangeNoWritesFastPathIgnoresLaterWrites(t *testing.T) {
	t.Parallel()
	rows := []KeyValue{
		{Key: []byte("rng/a"), Value: []byte("1")},
		{Key: []byte("rng/b"), Value: []byte("2")},
		{Key: []byte("rng/c"), Value: []byte("3")},
	}
	operand := append([]byte("stamp-"), make([]byte, 14)...)
	later := map[string]func(c *rywCache){
		"set-new":      func(c *rywCache) { c.set([]byte("rng/a5"), []byte("local")) },
		"set-existing": func(c *rywCache) { c.set([]byte("rng/b"), []byte("local")) },
		"clear":        func(c *rywCache) { c.clear([]byte("rng/b")) },
		"clear-range":  func(c *rywCache) { c.clearRange([]byte("rng/"), []byte("rng0")) },
		"versionstamp": func(c *rywCache) { c.atomic(MutSetVersionstampedValue, []byte("rng/b"), operand) },
	}
	for name, laterOps := range later {
		for _, reverse := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/reverse=%t", name, reverse), func(t *testing.T) {
				t.Parallel()
				want := append([]KeyValue(nil), rows...)
				if reverse {
					for i, j := 0, len(want)-1; i < j; i, j = i+1, j-1 {
						want[i], want[j] = want[j], want[i]
					}
				}
				var c rywCache
				got, more, err := c.getRange(context.Background(), []byte("rng/"), []byte("rng0"), 0, 1<<20, reverse,
					func(context.Context, []byte, []byte, int, int, bool) ([]KeyValue, bool, error) {
						laterOps(&c)
						return append([]KeyValue(nil), want...), false, nil
					})
				if err != nil || more || !sameKVs(got, want) {
					t.Fatalf("ISSUE_VIEW_RANGE: range = %s, more=%t, %v; want the storage rows only", formatKVs(got), more, err)
				}
			})
		}
	}
}

// The unreadable cap is decided at issue: a later clear that removes it, or a
// later versionstamp inside the window, does not change the answer.
func TestRYWGetRangeFastPathKeepsIssueTimeUnreadableCap(t *testing.T) {
	t.Parallel()
	storage := []KeyValue{{Key: []byte("cap/a"), Value: []byte("1")}, {Key: []byte("cap/b"), Value: []byte("2")}}
	operand := append([]byte("stamp-"), make([]byte, 14)...)
	for _, c := range []struct {
		name    string
		limit   int
		later   func(*rywCache)
		want    []KeyValue
		more    bool
		wantErr bool
	}{
		// The window ends before the cap with the limit unfilled: the read reaches it.
		{"reach/cap-cleared-later", 0, func(r *rywCache) { r.clearRange([]byte("cap/m"), []byte("cap/n")) }, nil, false, true},
		// The limit fills inside the window: more, and no reach.
		{"limit-filled/versionstamp-later", 2, func(r *rywCache) {
			r.atomic(MutSetVersionstampedValue, []byte("cap/a5"), operand)
		}, storage, true, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			var r rywCache
			r.addUnreadableRange([]byte("cap/m"), []byte("cap/n"))
			got, more, err := r.getRange(context.Background(), []byte("cap/"), []byte("cap0"), c.limit, 1<<20, false,
				func(_ context.Context, _, end []byte, _ int, _ int, _ bool) ([]KeyValue, bool, error) {
					if !bytes.Equal(end, []byte("cap/m")) {
						t.Errorf("storage read ends at %q, want the issue-time cap cap/m", end)
					}
					c.later(&r)
					return append([]KeyValue(nil), storage...), false, nil
				})
			if c.wantErr {
				var fe *wire.FDBError
				if !errors.As(err, &fe) || fe.Code != ErrAccessedUnreadable {
					t.Fatalf("ISSUE_VIEW_CAP: range = %s, more=%t, %v; want accessed_unreadable", formatKVs(got), more, err)
				}
				return
			}
			if err != nil || more != c.more || !sameKVs(got, c.want) {
				t.Fatalf("ISSUE_VIEW_CAP: range = %s, more=%t, %v; want %s, more=%t", formatKVs(got), more, err, formatKVs(c.want), c.more)
			}
		})
	}
}

func sameKVs(got, want []KeyValue) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range want {
		if !bytes.Equal(got[i].Key, want[i].Key) || !bytes.Equal(got[i].Value, want[i].Value) {
			return false
		}
	}
	return true
}

func formatKVs(kvs []KeyValue) string {
	var b bytes.Buffer
	b.WriteByte('[')
	for i, kv := range kvs {
		if i > 0 {
			b.WriteByte(' ')
		}
		fmt.Fprintf(&b, "%s=%s", kv.Key, kv.Value)
	}
	b.WriteByte(']')
	return b.String()
}
