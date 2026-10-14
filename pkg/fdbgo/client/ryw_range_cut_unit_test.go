package client

import (
	"context"
	"fmt"
	"math"
	"testing"
)

// rewalkNoWritesPage is the reference for a no-writes byte-limited page: cache the
// reply, then walk the snapshot cache as the merged RYW iterator would.
func rewalkNoWritesPage(c *rywCache, begin, end []byte, limit, byteTarget int, reverse bool, kvs []KeyValue, more bool) ([]KeyValue, bool, error) {
	c.cacheServerResult(begin, end, kvs, more, reverse)
	if rows, rowsMore, known, err := c.localRange(begin, end, limit, byteTarget, reverse, false); known {
		return rows, rowsMore, err
	}
	kvs, remaining := applyRangeByteLimit(kvs, byteTarget)
	return kvs, more || remaining <= 0, nil
}

func cutRows(n int) []KeyValue {
	rows := make([]KeyValue, n)
	for i := range rows {
		rows[i] = KeyValue{Key: []byte(fmt.Sprintf("cut/%04d", i)), Value: []byte(fmt.Sprintf("value-%04d", i))}
	}
	return rows
}

// The no-writes page cuts the storage reply directly unless cached state follows
// it; both must equal the snapshot-cache re-walk.
func TestRYWNoWritesByteCutMatchesRewalk(t *testing.T) {
	t.Parallel()
	all := cutRows(8)
	const rowBytes = 8 + 8 + 10 // NativeAPI charge per row: 8 + key + value
	begin, end := []byte("cut/"), []byte("cut0")
	type tc struct {
		name       string
		begin, end []byte
		limit      int
		byteTarget int
		reply      int  // rows in the storage reply (scan direction)
		more       bool // storage more flag
		precache   bool // cache the rows after the reply before the read
	}
	cases := []tc{
		{name: "complete/target-unreached", byteTarget: 100 * rowBytes, reply: 8},
		{name: "complete/target-reached", byteTarget: 3 * rowBytes, reply: 8},
		{name: "complete/target-reached-exactly-last", byteTarget: 8 * rowBytes, reply: 8},
		{name: "more/target-reached", byteTarget: 2*rowBytes + 1, reply: 4, more: true},
		{name: "more/target-unreached/next-unknown", byteTarget: 100 * rowBytes, reply: 3, more: true},
		{name: "more/target-unreached/next-cached", byteTarget: 100 * rowBytes, reply: 3, more: true, precache: true},
		{name: "more/target-reached-in-cache", byteTarget: 5 * rowBytes, reply: 3, more: true, precache: true},
		{name: "more/reply-reaches-begin", begin: all[0].Key, byteTarget: 100 * rowBytes, reply: 8, more: true},
		{name: "more/reply-reaches-end", end: keyAfterBytes(all[7].Key), byteTarget: 100 * rowBytes, reply: 8, more: true},
		{name: "limit-filled/complete", limit: 3, byteTarget: 100 * rowBytes, reply: 3},
		{name: "limit-filled/more", limit: 3, byteTarget: 100 * rowBytes, reply: 3, more: true},
		{name: "limit-filled/next-cached", limit: 5, byteTarget: 100 * rowBytes, reply: 3, more: true, precache: true},
		{name: "empty/complete", byteTarget: 100 * rowBytes, reply: 0},
	}
	for _, tc := range cases {
		for _, reverse := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/reverse=%t", tc.name, reverse), func(t *testing.T) {
				t.Parallel()
				begin, end := begin, end
				if tc.begin != nil {
					begin = tc.begin
				}
				if tc.end != nil {
					end = tc.end
				}
				ordered := all
				if reverse {
					ordered = reversedKVs(all)
				}
				reply := append([]KeyValue(nil), ordered[:tc.reply]...)
				prepare := func(c *rywCache) {
					if !tc.precache {
						return
					}
					c.mu.Lock()
					defer c.mu.Unlock()
					last := reply[len(reply)-1].Key
					if reverse {
						c.serverCache.insert(begin, last, all[:len(all)-tc.reply])
					} else {
						c.serverCache.insert(keyAfterBytes(last), end, all[tc.reply:])
					}
				}
				var ref rywCache
				prepare(&ref)
				want, wantMore, wantErr := rewalkNoWritesPage(&ref, begin, end, tc.limit, tc.byteTarget, reverse, reply, tc.more)

				var c rywCache
				prepare(&c)
				got, more, err := c.getRange(context.Background(), begin, end, tc.limit, tc.byteTarget, reverse,
					func(context.Context, []byte, []byte, int, int, bool) ([]KeyValue, bool, error) {
						return append([]KeyValue(nil), reply...), tc.more, nil
					})
				if (err == nil) != (wantErr == nil) || more != wantMore || !sameKVs(got, want) {
					t.Fatalf("BYTE_CUT: range = %s, more=%t, %v; re-walk = %s, more=%t, %v", formatKVs(got), more, err, formatKVs(want), wantMore, wantErr)
				}

				var snap rywCache
				prepare(&snap)
				got, more, err = snap.getSnapshotRange(context.Background(), begin, end, tc.limit, tc.byteTarget, reverse,
					withoutReadThrough(func(context.Context, []byte, []byte, int, int, bool) ([]KeyValue, bool, error) {
						return append([]KeyValue(nil), reply...), tc.more, nil
					}))
				if (err == nil) != (wantErr == nil) || more != wantMore || !sameKVs(got, want) {
					t.Fatalf("BYTE_CUT_SNAPSHOT: range = %s, more=%t, %v; re-walk = %s, more=%t, %v", formatKVs(got), more, err, formatKVs(want), wantMore, wantErr)
				}
			})
		}
	}
}

// A byte-limited page with no local writes must not pay per-row work beyond the
// storage reply copy: storage stopping short of the target is the common case.
func TestRYWNoWritesBytePageAllocsIndependentOfRows(t *testing.T) {
	type read func(*rywCache, context.Context, []byte, []byte, int, int, bool, func(context.Context, []byte, []byte, int, int, bool) ([]KeyValue, bool, error)) ([]KeyValue, bool, error)
	snapshot := func(c *rywCache, ctx context.Context, begin, end []byte, limit, byteTarget int, reverse bool, serve func(context.Context, []byte, []byte, int, int, bool) ([]KeyValue, bool, error)) ([]KeyValue, bool, error) {
		return c.getSnapshotRange(ctx, begin, end, limit, byteTarget, reverse, withoutReadThrough(serve))
	}
	for name, read := range map[string]read{"getRange": (*rywCache).getRange, "getSnapshotRange": snapshot} {
		measure := func(n int) float64 {
			reply := cutRows(n)
			var c rywCache
			serve := func(context.Context, []byte, []byte, int, int, bool) ([]KeyValue, bool, error) {
				return reply, true, nil
			}
			ctx := context.Background()
			return testing.AllocsPerRun(20, func() {
				c.reset()
				if _, _, err := read(&c, ctx, []byte("cut/"), []byte("cut0"), 0, 100*n*26, false, serve); err != nil {
					t.Fatal(err)
				}
			})
		}
		small, large := measure(10), measure(1000)
		if large > small+2 {
			t.Fatalf("NOWRITES_BYTE_ALLOCS %s: %v allocs for a 1000-row page vs %v for 10 rows; the cut must not re-walk row by row", name, large, small)
		}
	}
}

func TestSnapshotCacheSkipKnownEmpty(t *testing.T) {
	t.Parallel()
	var sc snapshotCache
	kv := func(k string) KeyValue { return KeyValue{Key: []byte(k), Value: []byte("v")} }
	sc.insert([]byte("a"), []byte("c"), []KeyValue{kv("b")})
	sc.insert([]byte("c"), []byte("e"), nil)
	sc.insert([]byte("e"), []byte("g"), nil)
	sc.insert([]byte("h"), []byte("k"), []KeyValue{kv("i")})
	sc.insert([]byte("k"), []byte("m"), nil)
	for _, tc := range []struct {
		begin, end string
		reverse    bool
		want       [2]string
	}{
		{"b\x00", "z", false, [2]string{"g", "z"}}, // past b, across adjacent empty ranges, up to the gap
		{"a", "z", false, [2]string{"a", "z"}},     // a known row at the origin
		{"g", "z", false, [2]string{"g", "z"}},     // origin not known
		{"d", "f", false, [2]string{"g", "f"}},     // whole range known empty
		{"c", "h\x00", false, [2]string{"g", "h\x00"}},
		{"a", "m", true, [2]string{"a", "k"}}, // down from m to the row at i
		{"a", "i", true, [2]string{"a", "h"}}, // [h,k) holds nothing below i; nothing ends at h
		{"a", "z", true, [2]string{"a", "z"}}, // keys just below z are not known
		{"d", "f", true, [2]string{"d", "c"}}, // whole range known empty
	} {
		b, e := sc.skipKnownEmpty([]byte(tc.begin), []byte(tc.end), tc.reverse)
		if got := [2]string{string(b), string(e)}; got != tc.want {
			t.Errorf("[%q,%q) reverse=%v: got %q, want %q", tc.begin, tc.end, tc.reverse, got, tc.want)
		}
	}
}

// A RYW range read caches through a reply's readThrough and starts its next storage
// read there (ReadYourWrites.actor.cpp:496-499, 760-764), also with local writes.
func TestRYWRangeReadStartsPastReadThrough(t *testing.T) {
	t.Parallel()
	for _, path := range []string{"fast", "snapshot", "writes"} {
		for _, reverse := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/reverse=%v", path, reverse), func(t *testing.T) {
				t.Parallel()
				var c rywCache
				first, readThrough, nextBegin, nextEnd, wantRequest := "b", "m", "b\x00", "z", "m..z"
				if reverse {
					first, nextBegin, nextEnd, wantRequest = "y", "a", "y", "a..m"
				}
				var requests []string
				server := func(_ context.Context, b, e []byte, _ int, _ int, _ bool) ([]KeyValue, bool, []byte, error) {
					requests = append(requests, string(b)+".."+string(e))
					if len(requests) == 1 {
						return []KeyValue{{Key: []byte(first), Value: []byte("v")}}, true, []byte(readThrough), nil
					}
					return nil, false, nil, nil
				}
				read := func(begin, end string) {
					var err error
					if path == "snapshot" {
						_, _, err = c.getSnapshotRange(context.Background(), []byte(begin), []byte(end), 0, 1000, reverse, server)
					} else {
						r := &rywRangeRead{}
						c.issueRangeInto(r, []byte(begin), []byte(end), 0, 1000, reverse, false)
						_, _, err = r.run(context.Background(), server)
					}
					if err != nil {
						t.Fatal(err)
					}
				}
				read("a", "z")
				if path == "writes" {
					// A write past readThrough; one inside the known range would end the page.
					write := "q"
					if reverse {
						write = "c"
					}
					c.set([]byte(write), []byte("local"))
				}
				read(nextBegin, nextEnd)
				if len(requests) != 2 || requests[1] != wantRequest {
					t.Fatalf("storage requests %q, want the second to be %s", requests, wantRequest)
				}
			})
		}
	}
}

func TestRYWRequestBytesDoesNotOverflow(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		bytes, requests, want int
	}{
		{-1, 20, -1},
		{0, 20, 0},
		{1, 0, 1},
		{1, 2, 4},
		{30000, 1, 60000},
		{30000, 2, 80000},
		{1, 20, 80000},
		{80000, 1000, 80000},
		{math.MaxInt, 1, 80000},
		{math.MaxInt, 20, 80000},
	} {
		if got := rywRequestBytes(tc.bytes, tc.requests); got != tc.want {
			t.Errorf("bytes=%d requests=%d: got %d, want %d", tc.bytes, tc.requests, got, tc.want)
		}
	}
}

// The write-overlay RYW path also applies C++ setRequestLimits (ReadYourWrites.actor.cpp:587-588,
// 748-753): each storage request's byte limit is min(byteTarget << min(requestCount, 20), REPLY_BYTE_LIMIT).
func TestRYWWriteOverlayRequestBytesClampedAndDoubled(t *testing.T) {
	t.Parallel()
	for _, reverse := range []bool{false, true} {
		t.Run(fmt.Sprintf("reverse=%v", reverse), func(t *testing.T) {
			t.Parallel()
			var c rywCache
			c.set([]byte("m"), []byte("local"))
			c.clearRange([]byte("b"), []byte("d"))
			var gotBytes []int
			server := func(_ context.Context, _, _ []byte, _ int, byteTarget int, _ bool) ([]KeyValue, bool, []byte, error) {
				gotBytes = append(gotBytes, byteTarget)
				return nil, false, nil, nil
			}
			r := &rywRangeRead{}
			c.issueRangeInto(r, []byte("a"), []byte("z"), 0, 120000, reverse, false)
			if _, _, err := r.run(context.Background(), server); err != nil {
				t.Fatal(err)
			}
			if len(gotBytes) != 1 || gotBytes[0] != replyByteLimit {
				t.Fatalf("120000-byte target sent %v, want [%d]", gotBytes, replyByteLimit)
			}

			var c2 rywCache
			c2.clearRange([]byte("b"), []byte("e"))
			gotBytes = nil
			server2 := func(_ context.Context, _, _ []byte, _ int, byteTarget int, _ bool) ([]KeyValue, bool, []byte, error) {
				gotBytes = append(gotBytes, byteTarget)
				switch len(gotBytes) {
				case 1:
					return []KeyValue{{Key: []byte("c"), Value: []byte("cleared")}}, true, nil, nil
				case 2:
					return []KeyValue{{Key: []byte("d"), Value: []byte("cleared")}}, true, nil, nil
				default:
					return nil, false, nil, nil
				}
			}
			r2 := &rywRangeRead{}
			c2.issueRangeInto(r2, []byte("a"), []byte("z"), 0, 30000, reverse, false)
			if _, _, err := r2.run(context.Background(), server2); err != nil {
				t.Fatal(err)
			}
			want := []int{30000, 60000, replyByteLimit}
			if fmt.Sprint(gotBytes) != fmt.Sprint(want) {
				t.Fatalf("30000-byte target across cleared spans sent %v, want %v", gotBytes, want)
			}
		})
	}
}

func TestRYWCacheHitsDoNotScaleStorageRequestBytes(t *testing.T) {
	t.Parallel()
	for _, cached := range []int{0, 257, 514} {
		for _, reverse := range []bool{false, true} {
			t.Run(fmt.Sprintf("cached=%d/reverse=%v", cached, reverse), func(t *testing.T) {
				t.Parallel()
				begin, end := []byte("a"), []byte("z")
				cacheBegin, cacheEnd := []byte("a"), []byte("b")
				if reverse {
					cacheBegin, cacheEnd = []byte("y"), []byte("z")
				}
				rows := make([]KeyValue, cached)
				for i := range rows {
					rows[i] = KeyValue{Key: []byte(fmt.Sprintf("%s%04d", cacheBegin, i)), Value: []byte("cleared")}
				}
				var c rywCache
				c.serverCache.insert(cacheBegin, cacheEnd, rows)
				c.clearRange(cacheBegin, cacheEnd)
				var requestedBytes []int
				server := func(_ context.Context, _, _ []byte, _ int, byteTarget int, _ bool) ([]KeyValue, bool, []byte, error) {
					requestedBytes = append(requestedBytes, byteTarget)
					return nil, false, nil, nil
				}
				r := &rywRangeRead{}
				c.issueRangeInto(r, begin, end, 100, 30000, reverse, false)
				got, more, err := r.run(context.Background(), server)
				if err != nil || len(got) != 0 || more {
					t.Fatalf("range returned %v, more=%v, err=%v", got, more, err)
				}
				// C++ counts storage requests, not cached batches of cleared rows.
				if len(requestedBytes) != 1 || requestedBytes[0] != 30000 {
					t.Fatalf("first storage request after cached clears sent %v, want [30000]", requestedBytes)
				}
			})
		}
	}
}
