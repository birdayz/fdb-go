package client

import (
	"context"
	"fmt"
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
					func(context.Context, []byte, []byte, int, int, bool) ([]KeyValue, bool, error) {
						return append([]KeyValue(nil), reply...), tc.more, nil
					})
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
	for name, read := range map[string]read{"getRange": (*rywCache).getRange, "getSnapshotRange": (*rywCache).getSnapshotRange} {
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
