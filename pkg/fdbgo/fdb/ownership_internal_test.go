package fdb

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"fdb.dev/pkg/fdbgo/client"
)

type ownershipCountingKey struct {
	calls atomic.Int32
	key   Key
}

func (k *ownershipCountingKey) FDBKey() Key {
	k.calls.Add(1)
	return k.key
}

func ownershipLocalTransaction(t *testing.T) Transaction {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	inner := &client.Transaction{}
	t.Cleanup(cancel)
	t.Cleanup(inner.Cancel)
	return Transaction{t: &transaction{inner: inner, ctx: ctx}}
}

// A terminal pipelined error takes the same fallback closure as dependent RYW.
func TestOwnership_GetFallbackEvaluatesKeyOnce(t *testing.T) {
	t.Parallel()
	tr := ownershipLocalTransaction(t)
	key := &ownershipCountingKey{key: Key{0xff, 0xff, 'x'}}
	_, err := tr.Get(key).Get()
	var fdbErr Error
	if !errors.As(err, &fdbErr) || fdbErr.Code != 2004 {
		t.Fatalf("illegal key error = %v, want 2004", err)
	}
	if got := key.calls.Load(); got != 1 {
		t.Fatalf("OWNERSHIP_FALLBACK_KEY: FDBKey called %d times, want once at call time", got)
	}
}

func TestOwnership_RangeCapturesBounds(t *testing.T) {
	t.Parallel()
	for _, snapshot := range []bool{false, true} {
		for _, selectors := range []bool{false, true} {
			t.Run(fmt.Sprintf("snapshot=%t/selectors=%t", snapshot, selectors), func(t *testing.T) {
				t.Parallel()
				tr := ownershipLocalTransaction(t)
				tr.ClearRange(KeyRange{Begin: Key("a"), End: Key("z")})
				tr.Set(Key("b"), []byte("BBBB"))
				tr.Set(Key("y"), []byte("YYYY"))
				begin, end := Key("a"), Key("c")
				var r Range = KeyRange{Begin: begin, End: end}
				if selectors {
					r = SelectorRange{Begin: FirstGreaterOrEqual(begin), End: FirstGreaterOrEqual(end)}
				}
				var result RangeResult
				if snapshot {
					result = tr.Snapshot().GetRange(r, RangeOptions{Limit: 1})
				} else {
					result = tr.GetRange(r, RangeOptions{Limit: 1})
				}
				begin[0], end[0] = 'x', 'z'
				rows, err := result.GetSliceWithError()
				if err != nil || len(rows) != 1 || string(rows[0].Key) != "b" || string(rows[0].Value) != "BBBB" {
					t.Fatalf("OWNERSHIP_RANGE_INPUT: got %v, %v; want b=BBBB", rows, err)
				}
			})
		}
	}
}

func TestOwnership_FutureMemoIsNotTransactionStorage(t *testing.T) {
	t.Parallel()
	tr := ownershipLocalTransaction(t)
	tr.Set(Key("key"), []byte("AAAA"))
	future := tr.Get(Key("key"))
	first, err := future.Get()
	if err != nil || string(first) != "AAAA" {
		t.Fatalf("first future = %q, %v", first, err)
	}
	first[0] = 'Z'
	memo, err := future.Get()
	if err != nil || string(memo) != "ZAAA" {
		t.Fatalf("same future lost its memo: %q, %v", memo, err)
	}
	independent, err := tr.Get(Key("key")).Get()
	if err != nil || string(independent) != "AAAA" {
		t.Fatalf("OWNERSHIP_FUTURE: independent future = %q, %v; want AAAA", independent, err)
	}
}
