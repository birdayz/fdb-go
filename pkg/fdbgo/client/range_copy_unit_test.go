package client

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"reflect"
	"testing"

	"fdb.dev/pkg/fdbgo/wire/types"
)

func rangeCopyTransaction() *Transaction {
	db := &database{}
	db.locCache.insertSorted([]locationEntry{{begin: []byte("a"), end: []byte("z")}})
	return &Transaction{db: db}
}

func rangeCopyReply(rows []KeyValue, more bool) []byte {
	data := binary.LittleEndian.AppendUint32(nil, uint32(len(rows)))
	for _, row := range rows {
		data = binary.LittleEndian.AppendUint32(data, uint32(len(row.Key)))
		data = append(data, row.Key...)
		data = binary.LittleEndian.AppendUint32(data, uint32(len(row.Value)))
		data = append(data, row.Value...)
	}
	return types.MarshalErrorOrValueGetKeyValuesReply(&types.GetKeyValuesReply{Data: data, More: more})
}

// Exclusive: AllocsPerRun counts all goroutines in the test process.
func TestRangeReplyAccumulationAllocations(t *testing.T) {
	tx := rangeCopyTransaction()
	defer tx.Cancel()
	ctx := context.Background()
	begin, end := []byte("a"), []byte("z")
	body := rangeCopyReply([]KeyValue{{Key: []byte("b"), Value: []byte("value")}}, false)
	ops := plainRangeScanOps
	var sends int
	ops.send = func(context.Context, []byte, []byte, int, int, bool, []ServerInfo) ([]KeyValue, bool, error) {
		sends++
		rows, more, _, err := parseGetKeyValuesReply(body)
		return rows, more, err
	}
	check := func(rows []KeyValue, more bool, err error) {
		if err != nil || more || len(rows) != 1 || string(rows[0].Key) != "b" || string(rows[0].Value) != "value" {
			t.Fatalf("nonempty parsed reply = %v, more=%t, err=%v", rows, more, err)
		}
	}
	const samples = 1000
	control := testing.AllocsPerRun(samples, func() {
		locations, err := tx.db.locCache.locateRange(tx.db, ctx, begin, end, 100, false, tx.tenantId, tx.currentSpan())
		if err != nil || len(locations) != 1 {
			t.Fatalf("location control = %v, %v", locations, err)
		}
		check(ops.send(ctx, begin, end, 2, ByteLimitUnlimited, false, locations[0].Servers))
	})
	scan := testing.AllocsPerRun(samples, func() {
		check(rangeScanImpl(tx, ctx, begin, end, 2, ByteLimitUnlimited, false, ops))
	})
	t.Logf("range reply allocations: scan=%.0f locate+parse=%.0f extra=%.0f sends=%d", scan, control, scan-control, sends)
	if sends != 2*(samples+1) || control == 0 || !tx.hadRead.Load() {
		t.Fatal("range reply allocation controls did not execute")
	}
	if scan != control {
		t.Errorf("first parsed reply adds %.0f descriptor allocations, want 0", scan-control)
	}
}

func TestForwardCacheDescriptorAllocations(t *testing.T) {
	begin, end := []byte("a"), []byte("z")
	rows := []KeyValue{{Key: []byte("b"), Value: []byte("first")}, {Key: []byte("y"), Value: []byte("last")}}
	reverseRows := []KeyValue{rows[1], rows[0]}
	var c rywCache
	entries := make([]cacheEntry, 0, 1)
	var calls int
	check := func() {
		calls++
		if len(c.serverCache.entries) != 1 || len(c.serverCache.entries[0].kvs) != 2 || string(c.serverCache.entries[0].kvs[0].Key) != "b" || string(c.serverCache.entries[0].kvs[1].Key) != "y" {
			t.Fatalf("nonempty cache control = %v", c.serverCache.entries)
		}
	}
	const samples = 1000
	direct := testing.AllocsPerRun(samples, func() {
		c.serverCache.entries = entries
		c.serverCache.insert(begin, end, rows)
		check()
	})
	forward := testing.AllocsPerRun(samples, func() {
		c.serverCache.entries = entries
		c.cacheServerResult(begin, end, rows, false, false)
		check()
	})
	reverse := testing.AllocsPerRun(samples, func() {
		c.serverCache.entries = entries
		c.cacheServerResult(begin, end, reverseRows, false, true)
		check()
	})
	t.Logf("cache descriptor allocations: forward=%.0f reverse=%.0f insert=%.0f calls=%d", forward, reverse, direct, calls)
	if calls != 3*(samples+1) || direct != 3 {
		t.Fatalf("cache control: calls=%d insert=%.0f, want %d/3", calls, direct, 3*(samples+1))
	}
	if forward != direct {
		t.Errorf("forward pre-cache copy adds %.0f allocations, want 0", forward-direct)
	}
	if reverse-direct != 1 {
		t.Errorf("reverse descriptor copy adds %.0f allocations, want unchanged 1", reverse-direct)
	}
}

func testRangeReplyOwnership[T any](t *testing.T, ops rangeScanOps[T], row func(string) T) {
	t.Helper()
	for _, reverse := range []bool{false, true} {
		for _, outcome := range []string{"complete", "retry", "empty-later", "row-limit", "byte-target", "error", "cancel", "ceiling"} {
			t.Run(fmt.Sprintf("reverse=%t/%s", reverse, outcome), func(t *testing.T) {
				t.Parallel()
				tx := rangeCopyTransaction()
				defer tx.Cancel()
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				firstKey, nextKey := "b", "c"
				if reverse {
					firstKey, nextKey = "y", "x"
				}
				backing := []T{row(firstKey), row("sentinel"), row("tail")}
				original := append([]T(nil), backing...)
				limit, target := 10, ByteLimitUnlimited
				if outcome == "row-limit" {
					limit = 1
				}
				if outcome == "byte-target" {
					target = 1
				}
				if outcome == "ceiling" {
					tx.db.rangeByteCeiling = 1
				}
				calls := 0
				wantErr := errors.New("later reply failed")
				localOps := ops
				localOps.send = func(ctx context.Context, begin, end []byte, gotLimit int, _ int, gotReverse bool, _ []ServerInfo) ([]T, bool, error) {
					calls++
					wantLimit := limit
					if calls > 1 {
						wantLimit--
					}
					if gotLimit != wantLimit {
						t.Fatalf("remaining limit = %d, want %d", gotLimit, wantLimit)
					}
					if gotReverse != reverse {
						t.Fatal("scan direction changed")
					}
					if calls == 1 {
						return backing[:1], true, nil
					}
					if calls != 2 && !(outcome == "retry" && calls == 3) {
						t.Fatalf("unexpected send %d", calls)
					}
					if reverse && string(end) != firstKey || !reverse && string(begin) != firstKey+"\x00" {
						t.Fatalf("next request = %q..%q", begin, end)
					}
					if outcome == "retry" && calls == 2 {
						return nil, false, errReplyTimeout
					}
					if outcome == "empty-later" {
						return make([]T, 0, 3), false, nil
					}
					if outcome == "error" {
						return nil, false, wantErr
					}
					if outcome == "cancel" {
						cancel()
						return nil, false, ctx.Err()
					}
					return []T{row(nextKey)}, false, nil
				}
				got, more, err := rangeScanImpl(tx, ctx, []byte("a"), []byte("z"), limit, target, reverse, localOps)
				switch outcome {
				case "error":
					if !errors.Is(err, wantErr) || got != nil || more || calls != 2 {
						t.Fatalf("partial error = %v/%t/%v, calls=%d", got, more, err, calls)
					}
				case "cancel":
					if !errors.Is(err, context.Canceled) || got != nil || more || calls != 2 {
						t.Fatalf("partial cancel = %v/%t/%v, calls=%d", got, more, err, calls)
					}
				case "ceiling":
					var ceiling *RangeMaterializationLimitError
					if !errors.As(err, &ceiling) || got != nil || more || calls != 1 {
						t.Fatalf("ceiling = %v/%t/%v, calls=%d", got, more, err, calls)
					}
				case "row-limit", "byte-target":
					if err != nil || !more || len(got) != 1 || calls != 1 || string(ops.key(&got[0])) != firstKey {
						t.Fatalf("limited = %v/%t/%v, calls=%d", got, more, err, calls)
					}
				case "empty-later":
					if err != nil || more || len(got) != 1 || calls != 2 || string(ops.key(&got[0])) != firstKey {
						t.Fatalf("empty later reply = %v/%t/%v, calls=%d", got, more, err, calls)
					}
				default:
					wantCalls := 2
					if outcome == "retry" {
						wantCalls++
					}
					if err != nil || more || len(got) != 2 || calls != wantCalls || string(ops.key(&got[0])) != firstKey || string(ops.key(&got[1])) != nextKey {
						t.Fatalf("two replies = %v/%t/%v, calls=%d", got, more, err, calls)
					}
				}
				if !reflect.DeepEqual(backing, original) {
					t.Fatalf("later append overwrote reply spare capacity: %v, want %v", backing, original)
				}
			})
		}
	}
	for _, reverse := range []bool{false, true} {
		t.Run(fmt.Sprintf("empty-first-shard/reverse=%t", reverse), func(t *testing.T) {
			t.Parallel()
			tx := rangeCopyTransaction()
			defer tx.Cancel()
			tx.db.locCache.entries = []locationEntry{{begin: []byte("a"), end: []byte("m")}, {begin: []byte("m"), end: []byte("z")}}
			key := "n"
			if reverse {
				key = "b"
			}
			calls := 0
			localOps := ops
			localOps.send = func(context.Context, []byte, []byte, int, int, bool, []ServerInfo) ([]T, bool, error) {
				calls++
				if calls == 1 {
					return make([]T, 0, 2), false, nil
				}
				return []T{row(key)}, false, nil
			}
			got, more, err := rangeScanImpl(tx, context.Background(), []byte("a"), []byte("z"), 0, ByteLimitUnlimited, reverse, localOps)
			if err != nil || more || len(got) != 1 || calls != 2 || string(ops.key(&got[0])) != key {
				t.Fatalf("empty first shard = %v/%t/%v, calls=%d", got, more, err, calls)
			}
		})
	}
	for _, nonnil := range []bool{false, true} {
		t.Run(fmt.Sprintf("empty/nonnil=%t", nonnil), func(t *testing.T) {
			t.Parallel()
			tx := rangeCopyTransaction()
			defer tx.Cancel()
			var empty []T
			if nonnil {
				empty = make([]T, 0, 4)
			}
			calls := 0
			localOps := ops
			localOps.send = func(context.Context, []byte, []byte, int, int, bool, []ServerInfo) ([]T, bool, error) {
				calls++
				return empty, false, nil
			}
			got, more, err := rangeScanImpl(tx, context.Background(), []byte("a"), []byte("z"), 0, ByteLimitUnlimited, false, localOps)
			if err != nil || more || got != nil || calls != 1 {
				t.Fatalf("empty reply = %#v/%t/%v, calls=%d", got, more, err, calls)
			}
		})
	}
}

func TestRangeReplyOwnershipPlainAndMapped(t *testing.T) {
	t.Parallel()
	t.Run("plain", func(t *testing.T) {
		t.Parallel()
		testRangeReplyOwnership(t, plainRangeScanOps, func(key string) KeyValue { return KeyValue{Key: []byte(key), Value: []byte("value")} })
	})
	t.Run("mapped", func(t *testing.T) {
		t.Parallel()
		testRangeReplyOwnership(t, mappedRangeScanOps, func(key string) MappedKeyValue {
			return MappedKeyValue{Key: []byte(key), Value: []byte("primary"), Kind: MappedResultGetRange, GetRange: MappedGetRange{Rows: []KeyValue{{Key: []byte("secondary/" + key), Value: []byte("value")}}}}
		})
	})
}

func TestForwardCacheRetainsIndependentDescriptors(t *testing.T) {
	t.Parallel()
	for _, reverse := range []bool{false, true} {
		for _, more := range []bool{false, true} {
			for _, overlap := range []bool{false, true} {
				t.Run(fmt.Sprintf("reverse=%t/more=%t/overlap=%t", reverse, more, overlap), func(t *testing.T) {
					t.Parallel()
					var c rywCache
					row := func(k string) KeyValue { return KeyValue{Key: []byte(k), Value: []byte("v/" + k)} }
					if overlap {
						c.serverCache.insert([]byte("a"), []byte("c"), []KeyValue{row("b")})
						c.serverCache.insert([]byte("w"), []byte("z"), []KeyValue{row("y")})
					}
					rows := []KeyValue{row("b"), row("m"), row("y")}
					if reverse {
						rows[0], rows[2] = rows[2], rows[0]
					}
					original := append([]KeyValue(nil), rows...)
					begin, end := []byte("a"), []byte("z")
					c.cacheServerResult(begin, end, rows, more, reverse)
					if !reflect.DeepEqual(rows, original) {
						t.Fatal("cache insertion reordered input descriptors")
					}
					for i := range rows {
						rows[i] = row("corrupted")
					}
					begin[0], end[0] = '!', '!'
					lo, hi := []byte("a"), []byte("z")
					if more && !overlap {
						if reverse {
							lo = []byte("b")
						} else {
							hi = []byte("y\x00")
						}
					}
					cached, known := c.serverCache.getRangeKVs(lo, hi)
					want := []KeyValue{row("b"), row("m"), row("y")}
					if !known || !reflect.DeepEqual(cached, want) {
						t.Fatalf("retained cache = %v, known=%t; want %v", cached, known, want)
					}
					// Public copies must not turn these immutable internal bytes into a mutable alias.
					public := cloneReadKVs(cached)
					public[0].Key[0], public[0].Value[0] = '!', '!'
					if !bytes.Equal(cached[0].Key, []byte("b")) || !bytes.Equal(cached[0].Value, []byte("v/b")) {
						t.Fatal("public byte copy aliases retained cache")
					}
				})
			}
		}
	}
}
