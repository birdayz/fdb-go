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
	db.locCache.insertRanges([]locationEntry{{begin: []byte("a"), end: []byte("z")}})
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
	ops.send = func(context.Context, []byte, []byte, rangeLimits, int64, bool, []ServerInfo, readTagSample) ([]KeyValue, bool, error) {
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
		check(ops.send(ctx, begin, end, newRangeLimits(2, ByteLimitUnlimited), 0, false, locations[0].Servers, readTagSample{}))
	})
	scan := testing.AllocsPerRun(samples, func() {
		rows, more, _, err := rangeScanImpl(tx, ctx, begin, end, newRangeLimits(2, ByteLimitUnlimited), 0, false, ops)
		check(rows, more, err)
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
				localOps.send = func(ctx context.Context, begin, end []byte, gotLimits rangeLimits, _ int64, gotReverse bool, _ []ServerInfo, _ readTagSample) ([]T, bool, error) {
					gotLimit := int(gotLimits.rows)
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
				got, more, _, err := rangeScanImpl(tx, ctx, []byte("a"), []byte("z"), newRangeLimits(limit, target), 0, reverse, localOps)
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
			localOps.send = func(context.Context, []byte, []byte, rangeLimits, int64, bool, []ServerInfo, readTagSample) ([]T, bool, error) {
				calls++
				if calls == 1 {
					return make([]T, 0, 2), false, nil
				}
				return []T{row(key)}, false, nil
			}
			got, more, _, err := rangeScanImpl(tx, context.Background(), []byte("a"), []byte("z"), newRangeLimits(0, ByteLimitUnlimited), 0, reverse, localOps)
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
			localOps.send = func(context.Context, []byte, []byte, rangeLimits, int64, bool, []ServerInfo, readTagSample) ([]T, bool, error) {
				calls++
				return empty, false, nil
			}
			got, more, _, err := rangeScanImpl(tx, context.Background(), []byte("a"), []byte("z"), newRangeLimits(0, ByteLimitUnlimited), 0, false, localOps)
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

type scanReply struct {
	rows []string
	more bool
	err  error
}

// scanWith runs rangeScanImpl over cached shards with scripted storage replies,
// recording each request's bounds and byte budget.
func scanWith(t *testing.T, shards [][2]string, begin, end string, byteTarget int, reverse bool, replies []scanReply) ([]KeyValue, bool, []byte, []string) {
	t.Helper()
	tx := rangeCopyTransaction()
	t.Cleanup(tx.Cancel)
	tx.db.locCache.entries = nil
	for _, s := range shards {
		tx.db.locCache.insertRanges([]locationEntry{{begin: []byte(s[0]), end: []byte(s[1])}})
	}
	var requests []string
	ops := plainRangeScanOps
	ops.send = func(_ context.Context, b, e []byte, gotLimits rangeLimits, _ int64, _ bool, _ []ServerInfo, _ readTagSample) ([]KeyValue, bool, error) {
		requests = append(requests, fmt.Sprintf("%s..%s/%d", b, e, gotLimits.bytes))
		if len(requests) > len(replies) {
			t.Fatalf("unexpected request %s", requests[len(requests)-1])
		}
		r := replies[len(requests)-1]
		var kvs []KeyValue
		for _, k := range r.rows {
			kvs = append(kvs, KeyValue{Key: []byte(k), Value: []byte("0123456789")})
		}
		return kvs, r.more, r.err
	}
	kvs, more, readThrough, err := rangeScanImpl(tx, context.Background(), []byte(begin), []byte(end), newRangeLimits(0, byteTarget), 0, reverse, ops)
	if err != nil {
		t.Fatal(err)
	}
	return kvs, more, readThrough, requests
}

// C++ getRange reports readThrough only when its own first reply's rows end a shard
// the range continues past (NativeAPI.actor.cpp:4762, 4795-4799).
func TestRangeScanReadThrough(t *testing.T) {
	t.Parallel()
	two := [][2]string{{"a", "m"}, {"m", "z"}}
	for _, tc := range []struct {
		name        string
		begin, end  string
		byteTarget  int
		reverse     bool
		replies     []scanReply
		readThrough string
		requests    int
	}{
		{"forward", "a", "z", 1000, false, []scanReply{{rows: []string{"b"}}}, "m", 1},
		{"reverse", "a", "z", 1000, true, []scanReply{{rows: []string{"y"}}}, "m", 1},
		{"after_reply_timeout", "a", "z", 1000, false, []scanReply{{err: errReplyTimeout}, {rows: []string{"b"}}}, "m", 2},
		{"cut_short", "a", "z", 1000, false, []scanReply{{rows: []string{"b"}, more: true}}, "", 1},
		{"empty_first_shard", "a", "z", 1000, false, []scanReply{{}, {rows: []string{"n"}}}, "", 2},
		{"range_ends_in_shard", "a", "k", 1000, false, []scanReply{{rows: []string{"b"}}}, "", 1},
		{"no_byte_target", "a", "z", ByteLimitUnlimited, false, []scanReply{{rows: []string{"b"}}, {rows: []string{"n"}}}, "", 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, _, readThrough, requests := scanWith(t, two, tc.begin, tc.end, tc.byteTarget, tc.reverse, tc.replies)
			if string(readThrough) != tc.readThrough || (tc.readThrough == "") != (readThrough == nil) || len(requests) != tc.requests {
				t.Fatalf("readThrough %q after %q, want %q after %d requests", readThrough, requests, tc.readThrough, tc.requests)
			}
		})
	}
}

// The next pair's first shard is read with the bytes the earlier rows left, and a
// budget used up returns before any request with no bytes (NativeAPI.actor.cpp:4359-4362).
func TestRangeScanPairBudget(t *testing.T) {
	t.Parallel()
	three := [][2]string{{"a", "g"}, {"g", "m"}, {"m", "z"}}
	const row = 8 + 1 + 10 // C++ GetRangeLimits charge: 8 + key + value
	t.Run("remaining_budget", func(t *testing.T) {
		t.Parallel()
		kvs, more, _, requests := scanWith(t, three, "a", "z", 100, false, []scanReply{{}, {rows: []string{"h"}}, {rows: []string{"n"}}})
		want := []string{"a..g/100", "g..m/100", fmt.Sprintf("m..z/%d", 100-row)}
		if !reflect.DeepEqual(requests, want) || len(kvs) != 2 || more {
			t.Fatalf("requests %q rows %d more %v, want %q, 2 rows, more=false", requests, len(kvs), more, want)
		}
	})
	t.Run("reverse_budget_used_up", func(t *testing.T) {
		t.Parallel()
		// The second reply ends on its shard's begin, so the reverse more fix would
		// otherwise move on to the next pair with nothing left to send.
		kvs, more, _, requests := scanWith(t, three, "a", "z", 2*row, true, []scanReply{{}, {rows: []string{"l", "g"}, more: true}})
		if len(requests) != 2 || len(kvs) != 2 || !more {
			t.Fatalf("requests %q rows %d more %v, want 2 requests, 2 rows, more=true", requests, len(kvs), more)
		}
	})
}
