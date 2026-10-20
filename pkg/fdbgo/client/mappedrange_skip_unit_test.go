package client

import (
	"context"
	"errors"
	"math"
	"reflect"
	"testing"

	"fdb.dev/pkg/fdbgo/transport"
	"fdb.dev/pkg/fdbgo/wire"
	"fdb.dev/pkg/fdbgo/wire/types"
)

func TestKeyResolutionRangeFallback(t *testing.T) {
	t.Parallel()
	loc := LocationResult{ShardBegin: []byte("m"), ShardEnd: []byte("t")}
	for _, tc := range []struct {
		name    string
		key     string
		orEqual bool
		offset  int32
		reverse bool
		want    bool
	}{
		{"forward_complete", "n", true, 0, false, false},
		{"forward_at_begin", "m", true, 0, false, false},
		{"forward_at_end", "t", true, 0, false, true},
		{"forward_past_end", "u", true, 0, false, true},
		{"forward_partial_one_interior", "n", false, 1, false, false},
		{"forward_partial_one_at_end", "t", false, 1, false, true},
		{"forward_partial_two", "n", false, 2, false, true},
		{"reverse_complete", "n", true, 0, true, false},
		{"reverse_at_begin", "m", true, 0, true, true},
		{"reverse_before_begin", "l", true, 0, true, true},
		{"reverse_partial", "n", false, 0, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := keyResolutionNeedsRangeFallback([]byte(tc.key), tc.orEqual, tc.offset, loc, tc.reverse); got != tc.want {
				t.Fatalf("fallback=%v want=%v", got, tc.want)
			}
		})
	}
	if keyResolutionNeedsRangeFallback([]byte("z"), true, 0, LocationResult{ShardBegin: []byte("m")}, false) {
		t.Fatal("unbounded shard end forced fallback for an interior key")
	}
}

func TestMappedSkipResolutionStepsDoNotWrap(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		distance int64
		offsets  []int32
	}{
		{3, []int32{4}},
		{-3, []int32{-2}},
		{1 << 30, []int32{1073741825}},
		{-1 << 30, []int32{-1073741823}},
		{math.MaxInt32, []int32{1073741825, 1073741824}},
		{-math.MaxInt32, []int32{-1073741823, -1073741822}},
	} {
		remaining := tc.distance
		for i, want := range tc.offsets {
			offset, rest := mappedSkipResolutionStep(remaining)
			if offset != want || rest != remaining-(int64(offset)-1) {
				t.Fatalf("distance=%d step=%d offset=%d rest=%d want offset=%d", tc.distance, i, offset, rest, want)
			}
			remaining = rest
		}
		if remaining != 0 {
			t.Fatalf("distance=%d has unresolved remainder=%d", tc.distance, remaining)
		}
	}
}

func TestMappedSelectorNativeShardClipping(t *testing.T) {
	t.Parallel()
	shard := LocationResult{ShardBegin: []byte("m"), ShardEnd: []byte("t")}
	for _, tc := range []struct {
		name      string
		begin     KeySelector
		end       KeySelector
		reverse   bool
		modified  bool
		wantBegin string
		wantEnd   string
	}{
		{"forward_end_past", mappedSelectorFGE([]byte("n")), mappedSelectorFGE([]byte("z")), false, true, "n", "t"},
		{"forward_end_equal", mappedSelectorFGE([]byte("n")), mappedSelectorFGE([]byte("t")), false, false, "n", "t"},
		{"forward_end_before", mappedSelectorFGE([]byte("n")), mappedSelectorFGE([]byte("s")), false, false, "n", "s"},
		{"reverse_begin_before", mappedSelectorFGE([]byte("a")), mappedSelectorFGE([]byte("s")), true, true, "m", "s"},
		{"reverse_begin_equal", mappedSelectorFGE([]byte("m")), mappedSelectorFGE([]byte("s")), true, false, "m", "s"},
		{"reverse_begin_after", mappedSelectorFGE([]byte("n")), mappedSelectorFGE([]byte("s")), true, false, "n", "s"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, e, modified := mappedRangeShardSelectors(tc.begin, tc.end, shard, tc.reverse)
			if string(b.Key) != tc.wantBegin || string(e.Key) != tc.wantEnd || modified != tc.modified {
				t.Fatalf("selectors=%+v,%+v modified=%v want keys=%q,%q modified=%v", b, e, modified, tc.wantBegin, tc.wantEnd, tc.modified)
			}
			if modified && ((!tc.reverse && !mappedSelectorIsFGE(e)) || (tc.reverse && !mappedSelectorIsFGE(b))) {
				t.Fatal("clipped selector is not FGE")
			}
		})
	}
}

func TestMappedSkipRejectsUnsupportedMappedDispatch(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name       string
		begin, end KeySelector
	}{
		{"forward", KeySelector{Key: []byte("a"), Offset: 2}, mappedSelectorFGE([]byte("z"))},
		{"greater_than", KeySelector{Key: []byte("a"), OrEqual: true, Offset: 1}, mappedSelectorFGE([]byte("z"))},
		{"negative_begin", KeySelector{Key: []byte("a"), Offset: -1}, mappedSelectorFGE([]byte("z"))},
		{"reverse", mappedSelectorFGE([]byte("a")), KeySelector{Key: []byte("z"), Offset: 0}},
		{"inverted", mappedSelectorFGE([]byte("z")), mappedSelectorFGE([]byte("a"))},
		{"equal", mappedSelectorFGE([]byte("a")), mappedSelectorFGE([]byte("a"))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// A nil transaction proves no RPC/location lookup can precede rejection.
			var tx *Transaction
			rows, more, err := tx.sendGetMappedSelectors(context.Background(), tc.begin, tc.end, nil, newMappedRangeLimits(1, -1), 0, false, nil, readTagSample{})
			if !mappedSkipHasError(err, ErrInternalError) || len(rows) != 0 || more {
				t.Fatalf("unsupported mapped request was not rejected: rows=%v more=%v err=%v", rows, more, err)
			}
		})
	}
}

func TestMappedRequestUsesOnlyPlainSelectors(t *testing.T) {
	t.Parallel()
	data, buf := buildGetMappedKeyValuesRequest([]byte("mapper"))(firstGreaterOrEqual([]byte("b")), firstGreaterOrEqual([]byte("e")), 123, -5, 512, false, -1, types.SpanContext{}, readTagSample{}, transport.UID{}, transport.UID{})
	defer getMappedKeyValuesBufPool.Put(buf)
	var req types.GetMappedKeyValuesRequest
	if err := req.UnmarshalFDB(data); err != nil {
		t.Fatal(err)
	}
	if req.Begin.OrEqual || req.Begin.Offset != 1 || req.End.OrEqual || req.End.Offset != 1 || string(req.Begin.Key) != "b" || string(req.End.Key) != "e" || req.Limit != -5 || req.LimitBytes != 512 || req.Version != 123 || string(req.Mapper) != "mapper" {
		t.Fatalf("request lost selector/limit fields: %+v", req)
	}
}

func TestMappedLimitsUseOnlyPrimaryBytes(t *testing.T) {
	t.Parallel()
	limits := newMappedRangeLimits(4, 30)
	limits.decrement([]MappedKeyValue{{Key: []byte("k"), Value: []byte("v"), Kind: MappedResultGetValue, GetValue: MappedGetValue{Key: []byte("secondary"), Value: make([]byte, 1024)}}})
	if limits.rows != 3 || limits.bytes != 20 || limits.minRows != 0 || limits.reached() || !limits.satisfiedMinRows() {
		t.Fatalf("mapped limits charged secondary payload or wrong row overhead: %+v", limits)
	}
	limits.decrement([]MappedKeyValue{{Key: []byte("long-primary"), Value: []byte("value")}})
	if !limits.reached() || limits.bytes != 0 {
		t.Fatalf("byte budget not exhausted: %+v", limits)
	}
	for _, tc := range []struct {
		limits mappedRangeLimits
		rows   int
		bytes  int
	}{
		{newMappedRangeLimits(0, -1), replyByteLimit, -1},
		{newMappedRangeLimits(7, 4096), 7, 4096},
		{mappedRangeLimits{rows: 8, bytes: 0, minRows: 1}, 1, -1},
	} {
		rows, byteTarget := tc.limits.requestLimits()
		if rows != tc.rows || byteTarget != tc.bytes {
			t.Fatalf("request limits=%d,%d want=%d,%d", rows, byteTarget, tc.rows, tc.bytes)
		}
	}
}

func TestMappedSkipEmptyOrInvertedRangeDoesNotResolve(t *testing.T) {
	t.Parallel()
	for _, begin := range [][]byte{nil, []byte("original-begin")} {
		for _, reverse := range []bool{false, true} {
			// A nil database makes any accidental location/getKey dispatch fail.
			tx := &Transaction{}
			result, err := tx.getMappedRangeWithResolvedSkip(context.Background(), begin, nil, nil, newMappedRangeLimits(1, -1), math.MaxInt32, 0, reverse)
			if err != nil || len(result.rows) != 0 || result.more || tx.hadRead.Load() || len(tx.readConflicts) != 0 {
				t.Fatalf("empty range issued a read: begin=%q reverse=%v result=%+v error=%v", begin, reverse, result, err)
			}
		}
	}
}

func TestMappedSkipPrimaryConflictOriginalAnchors(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name       string
		reverse    bool
		more       bool
		keys       []string
		begin, end string
	}{
		{"forward_limited", false, true, []string{"d", "f"}, "a", "f\x00"},
		{"reverse_limited", true, true, []string{"v", "t"}, "t", "z"},
		{"forward_empty", false, false, nil, "a", "z"},
		{"reverse_empty", true, false, nil, "a", "z"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result := mappedRangeResult{more: tc.more}
			for _, key := range tc.keys {
				result.rows = append(result.rows, MappedKeyValue{Key: []byte(key)})
			}
			r := primaryConflictRange([]byte("a"), []byte("z"), tc.reverse, result.more, result.rows)
			if string(r[0]) != tc.begin || string(r[1]) != tc.end {
				t.Fatalf("conflict=[%q,%q) want=[%q,%q)", r[0], r[1], tc.begin, tc.end)
			}
		})
	}
}

func TestMappedSkipConflictPreservesWithinRangePrefix(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"set", "clear", "clearRange", "atomic", "unreadable"} {
		tx := &Transaction{}
		switch kind {
		case "set":
			tx.ryw.set([]byte("m"), []byte("written"))
		case "clear":
			tx.ryw.clear([]byte("m"))
		case "clearRange":
			tx.ryw.addClearedRange([]byte("m"), []byte("q"))
		case "atomic":
			tx.ryw.atomic(MutAddValue, []byte("m"), []byte{1})
		case "unreadable":
			tx.ryw.atomic(MutSetVersionstampedValue, []byte("m"), make([]byte, 14))
		}
		err := tx.addMappedRangeConflictMustUnmodified(context.Background(), []byte("a"), []byte("z"))
		if !mappedSkipHasError(err, 2039) || len(tx.readConflicts) != 1 || string(tx.readConflicts[0].Begin) != "a" || string(tx.readConflicts[0].End) != "m" {
			t.Fatalf("kind=%s error=%v conflicts=%+v; want 2039 with [a,m) retained", kind, err, tx.readConflicts)
		}
	}
}

func TestMappedSkipSecondaryFailureKeepsEarlierConflicts(t *testing.T) {
	t.Parallel()
	tx := &Transaction{}
	tx.ryw.set([]byte("secondary/m"), []byte("written"))
	result := mappedRangeResult{rows: []MappedKeyValue{
		{Key: []byte("primary/c"), Kind: MappedResultGetValue, GetValue: MappedGetValue{Key: []byte("secondary/earlier")}},
		{Key: []byte("primary/d"), Kind: MappedResultGetRange, GetRange: MappedGetRange{Begin: mappedSelectorFGE([]byte("secondary/h")), End: mappedSelectorFGE([]byte("secondary/z"))}},
	}}
	err := tx.addMappedRangeConflicts(context.Background(), []byte("primary/a"), []byte("primary/z"), false, result.more, result.rows)
	if !mappedSkipHasError(err, 2039) || len(tx.readConflicts) != 3 {
		t.Fatalf("secondary failure lost prior conflicts: error=%v conflicts=%+v", err, tx.readConflicts)
	}
	for i, want := range [][2]string{{"primary/a", "primary/z"}, {"secondary/earlier", "secondary/earlier\x00"}, {"secondary/h", "secondary/m"}} {
		if string(tx.readConflicts[i].Begin) != want[0] || string(tx.readConflicts[i].End) != want[1] {
			t.Fatalf("conflict %d = [%q,%q) want [%q,%q)", i, tx.readConflicts[i].Begin, tx.readConflicts[i].End, want[0], want[1])
		}
	}
}

func TestMappedSkipConflictAtBeginInsertsNothing(t *testing.T) {
	t.Parallel()
	tx := &Transaction{}
	tx.ryw.addClearedRange([]byte("a"), []byte("n"))
	if err := tx.addMappedRangeConflictMustUnmodified(context.Background(), []byte("m"), []byte("z")); !mappedSkipHasError(err, 2039) || len(tx.readConflicts) != 0 {
		t.Fatalf("modified begin inserted conflicts: error=%v conflicts=%+v", err, tx.readConflicts)
	}
	if err := tx.addMappedRangeConflictMustUnmodified(context.Background(), []byte("z"), []byte("z")); err != nil || len(tx.readConflicts) != 0 {
		t.Fatalf("empty interval inserted conflicts: error=%v conflicts=%+v", err, tx.readConflicts)
	}
}

func TestMappedSkipDeferredEntrySurvivesPendingReset(t *testing.T) {
	t.Parallel()
	tx := &Transaction{}
	tx.deferredErr.Store(&wire.FDBError{Code: 2004})
	pending := tx.StartGetMappedRangeWithSkip(context.Background(), []byte("a"), []byte("z"), nil, 1, -1, 3, false)
	tx.Reset()
	for i := 0; i < 2; i++ {
		rows, more, err := pending.Get()
		if len(rows) != 0 || more || !mappedSkipHasError(err, 2004) {
			t.Fatalf("pending result after reset=%+v,%v,%v; want captured entry2004", rows, more, err)
		}
	}
	tx.readErrMu.Lock()
	issued := tx.readIncarnationLocked().issuedReads
	tx.readErrMu.Unlock()
	if issued != 0 {
		t.Fatalf("pending Skip introduced Commit's issued-read barrier: %d", issued)
	}
}

func TestMappedSkipCancelPrecedesEmptyShortcut(t *testing.T) {
	t.Parallel()
	tx := &Transaction{}
	tx.Cancel()
	_, _, err := tx.GetMappedRangeWithByteTargetAndSkip(context.Background(), []byte("b"), nil, nil, 1, -1, math.MaxInt32, false)
	if !mappedSkipHasError(err, 1025) {
		t.Fatalf("safe boundary shortcut bypassed cancellation preflight: %v", err)
	}
}

func TestMappedSkipSnapshotCannotRequestIt(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"GetMappedRangeWithByteTargetAndSkip", "StartGetMappedRangeWithSkip"} {
		if _, ok := reflect.TypeOf((*Snapshot)(nil)).MethodByName(name); ok {
			t.Fatalf("Snapshot exposes unsupported mapped Skip method %s", name)
		}
	}
}

func mappedSkipHasError(err error, code int) bool {
	var ferr *wire.FDBError
	return errors.As(err, &ferr) && ferr.Code == code
}
