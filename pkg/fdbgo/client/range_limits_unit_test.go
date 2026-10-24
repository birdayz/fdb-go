package client

import (
	"context"
	"fmt"
	"math"
	"testing"

	"fdb.dev/pkg/fdbgo/transport"
	"fdb.dev/pkg/fdbgo/wire/types"
)

// Expected values below are worked from the C++ source cited per case, not
// from the Go code under test.

func TestNewRangeLimits(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		rows, bytes int
		want        rangeLimits
	}{
		{0, 0, rangeLimits{-1, 1, -1}}, // fdb_c.cpp:991-994: 0 is unlimited
		{-1, -1, rangeLimits{-1, 1, -1}},
		{5, 4096, rangeLimits{5, 1, 4096}},
		{math.MaxInt64, math.MaxInt64, rangeLimits{math.MaxInt32, 1, math.MaxInt32}},
	} {
		if got := newRangeLimits(tc.rows, tc.bytes); got != tc.want {
			t.Errorf("newRangeLimits(%d, %d) = %+v, want %+v", tc.rows, tc.bytes, got, tc.want)
		}
	}
}

func TestRangeLimitsPredicates(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		l                                  rangeLimits
		reached, satisfied, valid, rowsLim bool
	}{
		// isReached: rows == 0 || (bytes == 0 && minRows == 0) (NativeAPI.actor.cpp:2856-2858).
		{rangeLimits{0, 1, -1}, true, false, false, true}, // minRows > rows: invalid
		{rangeLimits{-1, 0, 0}, true, true, true, false},
		{rangeLimits{-1, 1, 0}, false, false, true, false},
		{rangeLimits{5, 0, -1}, false, false, true, true},
		// hasSatisfiedMinRows: hasByteLimit && minRows == 0 (:2875-2877).
		{rangeLimits{-1, 0, 100}, false, true, true, false},
		{rangeLimits{-1, 2, 100}, false, false, true, false},
		// isValid (FDBTypes.h:753-756).
		{rangeLimits{2, 3, -1}, false, false, false, true},
		{rangeLimits{-2, 1, -1}, false, false, false, true},
		{rangeLimits{-1, 1, -2}, false, false, false, false},
		{rangeLimits{-1, -1, -1}, false, false, false, false},
	} {
		l := tc.l
		if l.isReached() != tc.reached || l.hasSatisfiedMinRows() != tc.satisfied || l.isValid() != tc.valid || l.hasRowLimit() != tc.rowsLim {
			t.Errorf("%+v: reached=%t satisfied=%t valid=%t rowLimit=%t, want %t %t %t %t", l,
				l.isReached(), l.hasSatisfiedMinRows(), l.isValid(), l.hasRowLimit(), tc.reached, tc.satisfied, tc.valid, tc.rowsLim)
		}
	}
}

// decrement is GetRangeLimits::decrement(VectorRef) (NativeAPI.actor.cpp:2811-2821).
func TestRangeLimitsDecrement(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		l          rangeLimits
		rows, size int
		want       rangeLimits
	}{
		{rangeLimits{5, 1, 100}, 2, 30, rangeLimits{3, 0, 70}},
		{rangeLimits{-1, 3, -1}, 2, 30, rangeLimits{-1, 1, -1}},
		{rangeLimits{-1, 1, 20}, 1, 30, rangeLimits{-1, 0, 0}},
		{rangeLimits{2, 1, -1}, 3, 0, rangeLimits{0, 0, -1}}, // C++ asserts; Go stops at zero
	} {
		l := tc.l
		l.decrement(tc.rows, tc.size)
		if l != tc.want {
			t.Errorf("%+v.decrement(%d, %d) = %+v, want %+v", tc.l, tc.rows, tc.size, l, tc.want)
		}
	}
}

// transformRangeLimits is NativeAPI.actor.cpp:4222-4240.
func TestTransformRangeLimits(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		l                 rangeLimits
		reverse           bool
		limit, limitBytes int32
	}{
		{rangeLimits{-1, 1, -1}, false, 80000, 80000},
		{rangeLimits{-1, 1, -1}, true, -80000, 80000},
		{rangeLimits{3, 1, 4096}, false, 3, 4096},
		{rangeLimits{3, 1, 4096}, true, -3, 4096},
		{rangeLimits{100000, 1, 100000}, false, 80000, 80000},
		{rangeLimits{-1, 1, 256}, false, 80000, 256},
		// A spent byte limit asks for what minRows still needs (:4236-4239).
		{rangeLimits{-1, 2, 0}, false, 2, 80000},
		{rangeLimits{7, 2, 0}, true, -2, 80000},
	} {
		limit, limitBytes := transformRangeLimits(tc.l, tc.reverse)
		if limit != tc.limit || limitBytes != tc.limitBytes {
			t.Errorf("transformRangeLimits(%+v, %t) = %d, %d; want %d, %d", tc.l, tc.reverse, limit, limitBytes, tc.limit, tc.limitBytes)
		}
	}
}

// setRequestLimits is ReadYourWrites.actor.cpp:569-590. The first cases are the
// requests RFC-262 A11 and S1 derive for the wire goldens and traces.
func TestSetRequestLimits(t *testing.T) {
	t.Parallel()
	const maxInt = math.MaxInt32
	for _, tc := range []struct {
		name         string
		l            rangeLimits
		add          int64
		offset       int32
		requestCount int
		want         rangeLimits
	}{
		{"getkey_offset", rangeLimits{1, 1, -1}, 1, 1, 0, rangeLimits{2, 2, -1}},
		{"getkey_negative_offset", rangeLimits{1, 1, -1}, 2, 1, 0, rangeLimits{3, 3, -1}},
		{"set_then_range", rangeLimits{-1, 1, 4096}, 0, 1, 0, rangeLimits{-1, 1, 4096}},
		{"T1 request 2", rangeLimits{1, 0, -1}, 1, 1, 1, rangeLimits{2, 2, -1}},
		{"T2 request 2", rangeLimits{4, 0, -1}, 0, 1, 1, rangeLimits{4, 1, -1}},
		// bytes == 0 becomes unlimited and manufactures rows for an unlimited row limit (:578-584).
		{"T5 request 2", rangeLimits{-1, 0, 0}, 1, 1, 1, rangeLimits{2, 2, -1}},
		{"T5 with a row limit", rangeLimits{maxInt - 1, 0, 0}, 1, 1, 1, rangeLimits{maxInt, 2, -1}},
		{"rows exhausted", rangeLimits{0, 0, 0}, 1, 1, 1, rangeLimits{2, 2, -1}},
		// The offset floors both limits.
		{"offset", rangeLimits{1, 1, -1}, 0, 5, 0, rangeLimits{5, 5, -1}},
		{"negative offset", rangeLimits{1, 1, -1}, 0, -3, 0, rangeLimits{1, 1, -1}},
		// INT_MAX saturation (:570-575).
		{"saturation", rangeLimits{5, 1, -1}, maxInt, 1, 0, rangeLimits{maxInt, maxInt, -1}},
		{"offset saturation", rangeLimits{-1, 1, 100}, 0, maxInt, 0, rangeLimits{-1, maxInt, 100}},
		// Bytes double per request, by at most 20, and cap at REPLY_BYTE_LIMIT (:585-588).
		{"bytes rc 1", rangeLimits{-1, 1, 30000}, 0, 1, 1, rangeLimits{-1, 1, 60000}},
		{"bytes rc 2", rangeLimits{-1, 1, 30000}, 0, 1, 2, rangeLimits{-1, 1, 80000}},
		{"bytes rc 16", rangeLimits{-1, 1, 1}, 0, 1, 16, rangeLimits{-1, 1, 65536}},
		{"bytes rc 17", rangeLimits{-1, 1, 1}, 0, 1, 17, rangeLimits{-1, 1, 80000}},
		{"bytes rc 64", rangeLimits{-1, 1, 1}, 0, 1, 64, rangeLimits{-1, 1, 80000}},
		{"bytes max rc 20", rangeLimits{-1, 1, maxInt}, 0, 1, 20, rangeLimits{-1, 1, 80000}},
	} {
		l := tc.l
		setRequestLimits(&l, tc.add, tc.offset, tc.requestCount)
		if l != tc.want {
			t.Errorf("%s: setRequestLimits(%+v, %d, %d, %d) = %+v, want %+v", tc.name, tc.l, tc.add, tc.offset, tc.requestCount, l, tc.want)
		}
	}
}

// FDBTypes.h:581-700.
func TestKeySelectorHelpers(t *testing.T) {
	t.Parallel()
	k := []byte("k")
	if got := (KeySelector{Key: k, OrEqual: true, Offset: 3}).removeOrEqual(); string(got.Key) != "k\x00" || got.OrEqual || got.Offset != 3 {
		t.Errorf("removeOrEqual(k, true, 3) = %+v", got)
	}
	if got := (KeySelector{Key: allKeysEnd, OrEqual: true, Offset: 1}).removeOrEqual(); string(got.Key) != "\xff\xff" || got.OrEqual {
		t.Errorf("removeOrEqual(\\xff\\xff) = %+v, want keyAfter(\\xff\\xff) = \\xff\\xff", got)
	}
	if got := (KeySelector{Key: k, Offset: -2}).removeOrEqual(); string(got.Key) != "k" || got.Offset != -2 {
		t.Errorf("removeOrEqual without orEqual = %+v", got)
	}
	for _, tc := range []struct {
		s                                KeySelector
		fge, fgt, lle, backward          bool
		greaterJ, greaterK, lessK, lessL bool
	}{
		// isDefinitelyGreater(x): offset >= 1 && (FGE ? key > x : key >= x)  (:667)
		// isDefinitelyLess(x):    offset <= 0 && (LLE ? key < x : key <= x)   (:669)
		{firstGreaterOrEqual(k), true, false, false, false, true, false, false, false},
		{firstGreaterThan(k), false, true, false, false, true, true, false, false},
		{KeySelector{Key: k, OrEqual: true}, false, false, true, false, false, false, false, true},
		{KeySelector{Key: k}, false, false, false, true, false, false, true, true},
		{KeySelector{Key: k, Offset: 2}, false, false, false, false, true, true, false, false},
		{KeySelector{Key: k, Offset: -1}, false, false, false, true, false, false, true, true},
	} {
		s := tc.s
		got := []bool{
			s.isFirstGreaterOrEqual(), s.isFirstGreaterThan(), s.isLastLessOrEqual(), s.isBackward(),
			s.isDefinitelyGreater([]byte("j")), s.isDefinitelyGreater(k), s.isDefinitelyLess(k), s.isDefinitelyLess([]byte("l")),
		}
		want := []bool{tc.fge, tc.fgt, tc.lle, tc.backward, tc.greaterJ, tc.greaterK, tc.lessK, tc.lessL}
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Errorf("%+v: got %v, want %v", s, got, want)
		}
	}
}

// The request builders carry a selector verbatim, so a RYW request with an
// offset or orEqual reaches storage as C++ sends it.
func TestRequestBuildersCarrySelectors(t *testing.T) {
	t.Parallel()
	begin := KeySelector{Key: []byte("b"), OrEqual: true, Offset: 3}
	end := KeySelector{Key: []byte("e"), Offset: -2}
	body, bufp := buildGetKeyValuesRequest(begin, end, 7, 2, 80000, false, -1, types.SpanContext{}, readTagSample{}, transport.UID{}, transport.UID{})
	var req types.GetKeyValuesRequest
	if err := req.UnmarshalFDB(body); err != nil {
		t.Fatal(err)
	}
	getKeyValuesBufPool.Put(bufp)
	if got := fmt.Sprintf("%q %t %d / %q %t %d", req.Begin.Key, req.Begin.OrEqual, req.Begin.Offset, req.End.Key, req.End.OrEqual, req.End.Offset); got != `"b" true 3 / "e" false -2` {
		t.Errorf("GetKeyValuesRequest selectors = %s", got)
	}
	body, bufp = buildGetMappedKeyValuesRequest([]byte("m"))(begin, end, 7, 2, 80000, false, -1, types.SpanContext{}, readTagSample{}, transport.UID{}, transport.UID{})
	var mreq types.GetMappedKeyValuesRequest
	if err := mreq.UnmarshalFDB(body); err != nil {
		t.Fatal(err)
	}
	getMappedKeyValuesBufPool.Put(bufp)
	if got := fmt.Sprintf("%q %t %d / %q %t %d", mreq.Begin.Key, mreq.Begin.OrEqual, mreq.Begin.Offset, mreq.End.Key, mreq.End.OrEqual, mreq.End.Offset); got != `"b" true 3 / "e" false -2` {
		t.Errorf("GetMappedKeyValuesRequest selectors = %s", got)
	}
}

// getExactRange keeps reading while a byte-limited request has not returned
// minRows rows (NativeAPI.actor.cpp:4415 checks hasSatisfiedMinRows, not "any
// rows"), and the next request asks for the rows still missing (:4236-4239).
func TestExactRangeHonorsMinRows(t *testing.T) {
	t.Parallel()
	for _, reverse := range []bool{false, true} {
		t.Run(fmt.Sprintf("reverse=%t", reverse), func(t *testing.T) {
			t.Parallel()
			db := &database{}
			db.locCache.insertRanges([]locationEntry{{begin: []byte("a"), end: []byte("z")}})
			tx := &Transaction{db: db}
			defer tx.Cancel()
			replies := [][]string{{"c"}, {"d", "e"}}
			if reverse {
				replies = [][]string{{"x"}, {"w", "v"}}
			}
			var requests []string
			ops := plainRangeScanOps
			ops.send = func(_ context.Context, b, e []byte, l rangeLimits, readVersion int64, _ bool, _ []ServerInfo, _ readTagSample) ([]KeyValue, bool, error) {
				limit, limitBytes := transformRangeLimits(l, reverse)
				requests = append(requests, fmt.Sprintf("%s..%s %+v v%d wire=%d/%d", b, e, l, readVersion, limit, limitBytes))
				if len(requests) > len(replies) {
					t.Fatalf("unexpected request %s", requests[len(requests)-1])
				}
				var kvs []KeyValue
				for _, k := range replies[len(requests)-1] {
					kvs = append(kvs, KeyValue{Key: []byte(k), Value: []byte("0123456789")})
				}
				return kvs, true, nil
			}
			kvs, more, _, err := rangeScanImpl(tx, context.Background(), []byte("a"), []byte("z"), rangeLimits{-1, 3, 1}, 42, reverse, ops)
			if err != nil {
				t.Fatal(err)
			}
			want := []string{
				"a..z {rows:-1 minRows:3 bytes:1} v42 wire=80000/1",
				"c\x00..z {rows:-1 minRows:2 bytes:0} v42 wire=2/80000",
			}
			if reverse {
				want = []string{
					"a..z {rows:-1 minRows:3 bytes:1} v42 wire=-80000/1",
					"a..x {rows:-1 minRows:2 bytes:0} v42 wire=-2/80000",
				}
			}
			if fmt.Sprintf("%q", requests) != fmt.Sprintf("%q", want) {
				t.Fatalf("requests\n got %q\nwant %q", requests, want)
			}
			if len(kvs) != 3 || !more {
				t.Fatalf("got %d rows, more=%t; want 3 rows and more (bytes and minRows both spent)", len(kvs), more)
			}
		})
	}
}
