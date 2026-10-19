package fdb

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"

	"fdb.dev/pkg/fdbgo/client"
)

// AllocsPerRun uses process-wide counters, so this test must remain exclusive.
// The real native client answers from its write map; no RPC goroutine adds noise.
func TestNativeExactRangeAllocations(t *testing.T) {
	for _, snapshot := range []bool{false, true} {
		t.Run(fmt.Sprintf("snapshot=%t", snapshot), func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			inner := &client.Transaction{}
			t.Cleanup(cancel)
			t.Cleanup(inner.Cancel)
			tr := Transaction{t: &transaction{inner: inner, ctx: ctx}}
			begin, key, end := Key(bytes.Repeat([]byte{'a'}, 32)), Key(bytes.Repeat([]byte{'b'}, 32)), Key(bytes.Repeat([]byte{'c'}, 32))
			tr.ClearRange(KeyRange{Begin: begin, End: end})
			tr.Set(key, []byte("native-row"))
			var exact Range = KeyRange{Begin: begin, End: end}
			var selectors Range = SelectorRange{Begin: FirstGreaterOrEqual(begin), End: FirstGreaterOrEqual(end)}
			var reader ReadTransaction = tr
			if snapshot {
				reader = tr.Snapshot()
			}
			const samples = 1000
			var nativeReads, exactReads, selectorReads int
			native := testing.AllocsPerRun(samples, func() {
				var pending *client.PendingRange
				if snapshot {
					pending = inner.Snapshot().StartGetRange(ctx, begin, end, 2, 80000, false)
				} else {
					pending = inner.StartGetRange(ctx, begin, end, 2, 80000, false)
				}
				rows, more, err := pending.Get()
				if err != nil || more || len(rows) != 1 || !bytes.Equal(rows[0].Key, key) || string(rows[0].Value) != "native-row" {
					t.Fatalf("native control: rows=%v more=%t err=%v", rows, more, err)
				}
				nativeReads++
			})
			read := func(r Range) {
				result := reader.GetRange(r, RangeOptions{Mode: StreamingModeWantAll, Limit: 2})
				if result.(goRangeResult).first.pending == nil {
					t.Fatal("facade did not issue a native first batch")
				}
				rows, err := result.GetSliceWithError()
				if err != nil || len(rows) != 1 || !bytes.Equal(rows[0].Key, key) || string(rows[0].Value) != "native-row" {
					t.Fatalf("facade control: rows=%v err=%v", rows, err)
				}
			}
			exactAllocs := testing.AllocsPerRun(samples, func() { read(exact); exactReads++ })
			selectorAllocs := testing.AllocsPerRun(samples, func() { read(selectors); selectorReads++ })
			t.Logf("native range allocations: exact=%.0f selector=%.0f native=%.0f exact-overhead=%.0f; reads=%d/%d/%d", exactAllocs, selectorAllocs, native, exactAllocs-native, nativeReads, exactReads, selectorReads)
			if nativeReads != samples+1 || exactReads != samples+1 || selectorReads != samples+1 || native <= 0 || selectorAllocs <= native {
				t.Fatal("native range allocation controls did not execute")
			}
			if got := selectorAllocs - native; got != 18 {
				t.Errorf("selector range overhead = %.0f allocations, want unchanged 18", got)
			}
			// Two owned byte clones, the result and the first batch; the rows are the client's.
			if got := exactAllocs - native; got != 4 {
				t.Errorf("native exact range overhead = %.0f allocations, want 4 (no private key/range reboxing)", got)
			}
		})
	}
}

type ownedRangeKey struct {
	key  Key
	call func()
}

func (k *ownedRangeKey) FDBKey() Key {
	k.call()
	return k.key
}

type ownedExactRange struct {
	begin, end KeyConvertible
	call       func()
}

func (r *ownedExactRange) FDBRangeKeys() (KeyConvertible, KeyConvertible) {
	r.call()
	return r.begin, r.end
}

func (*ownedExactRange) FDBRangeKeySelectors() (Selectable, Selectable) {
	panic("an ExactRange must not evaluate its selector callback")
}

type ownedSelector struct {
	selector KeySelector
	call     func()
}

func (s *ownedSelector) FDBKeySelector() KeySelector {
	s.call()
	return s.selector
}

type ownedSelectorRange struct {
	begin, end Selectable
	call       func()
}

func (r *ownedSelectorRange) FDBRangeKeySelectors() (Selectable, Selectable) {
	r.call()
	return r.begin, r.end
}

func TestNativeRangeCallbackAndCloneOrder(t *testing.T) {
	t.Parallel()
	for _, snapshot := range []bool{false, true} {
		for _, selectors := range []bool{false, true} {
			t.Run(fmt.Sprintf("snapshot=%t/selectors=%t", snapshot, selectors), func(t *testing.T) {
				t.Parallel()
				tr := ownershipLocalTransaction(t)
				tr.ClearRange(KeyRange{Begin: Key("a"), End: Key("z")})
				tr.Set(Key("b"), []byte("BBBB"))
				tr.Set(Key("d"), []byte("DDDD"))
				var events []string
				begin := &ownedRangeKey{key: Key("a"), call: func() { events = append(events, "begin-key") }}
				end := &ownedRangeKey{key: Key("c"), call: func() {
					events = append(events, "end-key")
					begin.key[0] = 'x' // The begin clone must precede this callback.
				}}
				var r Range = &ownedExactRange{begin: begin, end: end, call: func() { events = append(events, "range-keys") }}
				wantEvents := []string{"range-keys", "begin-key", "end-key"}
				if selectors {
					r = &ownedSelectorRange{
						begin: &ownedSelector{selector: FirstGreaterOrEqual(begin), call: func() { events = append(events, "begin-selector") }},
						end:   &ownedSelector{selector: FirstGreaterOrEqual(end), call: func() { events = append(events, "end-selector") }},
						call:  func() { events = append(events, "range-selectors") },
					}
					wantEvents = []string{"range-selectors", "begin-selector", "end-selector", "begin-key", "end-key"}
				}
				var reader ReadTransaction = tr
				if snapshot {
					reader = tr.Snapshot()
				}
				result := reader.GetRange(r, RangeOptions{})
				if !reflect.DeepEqual(events, wantEvents) {
					t.Fatalf("issue callbacks = %v, want %v", events, wantEvents)
				}
				first := result.(goRangeResult).first
				end.key[0] = 'z'
				if string(first.begin) != "a" || string(first.end) != "c" {
					t.Fatalf("owned first batch bounds = %q/%q, want a/c", first.begin, first.end)
				}
				// Another operation and write must not replace this result's issue-time view.
				tr.Set(Key("b"), []byte("late"))
				other := reader.GetRange(KeyRange{Begin: Key("d"), End: Key("e")}, RangeOptions{})
				for i := 0; i < 2; i++ {
					it := result.Iterator()
					if !it.Advance() {
						t.Fatal("first batch is empty")
					}
					kv, err := it.Get()
					if err != nil || string(kv.Key) != "b" || string(kv.Value) != "BBBB" {
						t.Fatalf("iterator = %v, %v", kv, err)
					}
					kv.Key[0], kv.Value[0] = '!', '!'
					if it.Advance() {
						t.Fatal("unexpected second row")
					}
					rows, err := result.GetSliceWithError()
					if err != nil || len(rows) != 1 || string(rows[0].Key) != "b" || string(rows[0].Value) != "BBBB" {
						t.Fatalf("slice = %v, %v", rows, err)
					}
					rows[0].Key[0], rows[0].Value[0] = '?', '?'
				}
				rows, err := other.GetSliceWithError()
				if err != nil || len(rows) != 1 || string(rows[0].Key) != "d" {
					t.Fatalf("independent operation = %v, %v", rows, err)
				}
				if !reflect.DeepEqual(events, wantEvents) {
					t.Fatalf("consumption repeated callbacks: %v", events)
				}
			})
		}
	}
}

func TestNativeExactRangeCallbacksBeforeErrors(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name               string
		options            RangeOptions
		cancelled, illegal bool
		want               int
	}{
		{"exact-zero", RangeOptions{Mode: StreamingModeExact}, true, true, 2210},
		{"exact-minus-one", RangeOptions{Mode: StreamingModeExact, Limit: -1}, true, true, 2210},
		{"mode", RangeOptions{Mode: StreamingMode(99), Limit: -2}, true, true, 2000},
		{"cancelled", RangeOptions{Limit: -2}, true, true, 1025},
		{"key-before-limit", RangeOptions{Limit: -2}, false, true, 2004},
		{"limit", RangeOptions{Limit: -2}, false, false, 2012},
	}
	for _, snapshot := range []bool{false, true} {
		for _, tc := range cases {
			t.Run(fmt.Sprintf("snapshot=%t/%s", snapshot, tc.name), func(t *testing.T) {
				t.Parallel()
				tr := ownershipLocalTransaction(t)
				if tc.cancelled {
					tr.Cancel()
				}
				var events []string
				begin := &ownedRangeKey{key: Key("a"), call: func() { events = append(events, "begin") }}
				end := &ownedRangeKey{key: Key("z"), call: func() { events = append(events, "end") }}
				if tc.illegal {
					end.key = Key{0xff, 1}
				}
				r := &ownedExactRange{begin: begin, end: end, call: func() { events = append(events, "range") }}
				var reader ReadTransaction = tr
				if snapshot {
					reader = tr.Snapshot()
				}
				result := reader.GetRange(r, tc.options)
				wantEvents := []string{"range", "begin", "end"}
				if !reflect.DeepEqual(events, wantEvents) {
					t.Fatalf("callbacks before error = %v", events)
				}
				check := func(err error) {
					t.Helper()
					var fe Error
					if !errors.As(err, &fe) || fe.Code != tc.want {
						t.Fatalf("error = %v, want %d", err, tc.want)
					}
				}
				_, err := result.GetSliceWithError()
				check(err)
				for i := 0; i < 2; i++ {
					it := result.Iterator()
					it.Advance()
					_, err := it.Get()
					check(err)
				}
				if !reflect.DeepEqual(events, wantEvents) {
					t.Fatalf("error consumption repeated callbacks: %v", events)
				}
			})
		}
	}
}
