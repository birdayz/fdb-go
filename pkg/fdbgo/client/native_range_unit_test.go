package client

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"
)

// Expected values below are worked from NativeAPI.actor.cpp (FoundationDB
// 7.3.77) at the lines cited per case, not from the Go code under test.

func sel(key string, orEqual bool, offset int32) KeySelector {
	return KeySelector{Key: []byte(key), OrEqual: orEqual, Offset: offset}
}

func fmtSel(s KeySelector) string { return fmt.Sprintf("(%q,%t,%d)", s.Key, s.OrEqual, s.Offset) }

// The shard is [c, m) throughout.
func TestNativeRequestSelectors(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name               string
		begin, end         KeySelector
		reverse            bool
		wantBegin, wantEnd KeySelector
		wantModified       bool
	}{
		// Forward: end.isDefinitelyGreater(shard.end) → FGE(shard.end) (:4672-4677).
		{"fwd/end-past-shard", sel("d", false, 1), sel("z", false, 1), false, sel("d", false, 1), sel("m", false, 1), true},
		{"fwd/end-FGE-at-shard-end", sel("d", false, 1), sel("m", false, 1), false, sel("d", false, 1), sel("m", false, 1), false},
		{"fwd/end-offset2-at-shard-end", sel("d", false, 1), sel("m", false, 2), false, sel("d", false, 1), sel("m", false, 1), true},
		{"fwd/end-inside-shard", sel("d", false, 1), sel("k", false, 1), false, sel("d", false, 1), sel("k", false, 1), false},
		{"fwd/end-offset5-before-shard-end", sel("d", false, 1), sel("a", false, 5), false, sel("d", false, 1), sel("a", false, 5), false},
		{"fwd/end-lastLessThan-past-shard", sel("d", false, 1), sel("z", false, 0), false, sel("d", false, 1), sel("z", false, 0), false},
		{"fwd/begin-before-shard-kept", sel("a", false, 1), sel("k", false, 1), false, sel("a", false, 1), sel("k", false, 1), false},
		// Reverse: (begin-1).isDefinitelyLess(shard.begin), unless begin is
		// FGE(shard.begin) → FGE(shard.begin) (:4656-4669).
		{"rev/begin-before-shard", sel("a", false, 1), sel("f", false, 1), true, sel("c", false, 1), sel("f", false, 1), true},
		{"rev/begin-FGE-at-shard-begin", sel("c", false, 1), sel("f", false, 1), true, sel("c", false, 1), sel("f", false, 1), false},
		{"rev/begin-lastLessThan-shard-begin", sel("c", false, 0), sel("f", false, 1), true, sel("c", false, 1), sel("f", false, 1), true},
		{"rev/begin-inside-shard", sel("d", false, 1), sel("f", false, 1), true, sel("d", false, 1), sel("f", false, 1), false},
		{"rev/begin-offset2-before-shard", sel("a", false, 2), sel("f", false, 1), true, sel("a", false, 2), sel("f", false, 1), false},
		{"rev/begin-FGT-before-shard", sel("b", true, 1), sel("f", false, 1), true, sel("c", false, 1), sel("f", false, 1), true},
		{"rev/end-past-shard-kept", sel("d", false, 1), sel("z", false, 1), true, sel("d", false, 1), sel("z", false, 1), false},
	} {
		b, e, m := nativeRequestSelectors(tc.begin, tc.end, []byte("c"), []byte("m"), tc.reverse)
		if !equalSel(b, tc.wantBegin) || !equalSel(e, tc.wantEnd) || m != tc.wantModified {
			t.Errorf("%s: got %s %s modified=%t, want %s %s modified=%t", tc.name, fmtSel(b), fmtSel(e), m,
				fmtSel(tc.wantBegin), fmtSel(tc.wantEnd), tc.wantModified)
		}
	}
}

func equalSel(a, b KeySelector) bool {
	return bytes.Equal(a.Key, b.Key) && a.OrEqual == b.OrEqual && a.Offset == b.Offset
}

func TestNativeRangeLoopEdges(t *testing.T) {
	t.Parallel()
	// readToBegin prologue (:4623-4626): begin "" with offset < 1.
	for _, tc := range []struct {
		begin       KeySelector
		readToBegin bool
		want        KeySelector
	}{
		{sel("", false, 0), true, sel("", false, 1)},
		{sel("", false, -3), true, sel("", false, 1)},
		{sel("", false, 1), false, sel("", false, 1)},
		{sel("a", false, 0), false, sel("a", false, 0)},
	} {
		l := nativeRangeLoop{begin: tc.begin, end: sel("z", false, 1)}
		l.start()
		if l.out.readToBegin != tc.readToBegin || !equalSel(l.begin, tc.want) {
			t.Errorf("start(%s): readToBegin=%t begin=%s, want %t %s", fmtSel(tc.begin), l.out.readToBegin, fmtSel(l.begin), tc.readToBegin, fmtSel(tc.want))
		}
	}
	// Exit (:4632): end.key == "" && (offset < 1 || FGE).
	for _, tc := range []struct {
		end  KeySelector
		want bool
	}{
		{sel("", false, 1), true},
		{sel("", false, 0), true},
		{sel("", false, 2), false},
		{sel("", true, 1), false},
		{sel("a", false, 0), false},
	} {
		l := nativeRangeLoop{end: tc.end}
		if got := l.atEnd(); got != tc.want {
			t.Errorf("atEnd(%s) = %t, want %t", fmtSel(tc.end), got, tc.want)
		}
	}
	// Location (:4638-4639): forward begin, isBackward(begin); reverse end,
	// isBackward(end - 1).
	for _, tc := range []struct {
		begin, end KeySelector
		reverse    bool
		key        string
		backward   bool
	}{
		{sel("b", false, 1), sel("y", false, 1), false, "b", false},
		{sel("b", false, 0), sel("y", false, 1), false, "b", true},
		{sel("b", false, 1), sel("y", false, 1), true, "y", true},
		{sel("b", false, 1), sel("y", false, 2), true, "y", false},
		{sel("b", false, 1), sel("y", true, 1), true, "y", false},
	} {
		l := nativeRangeLoop{begin: tc.begin, end: tc.end, reverse: tc.reverse}
		k, back := l.location()
		if string(k) != tc.key || back != tc.backward {
			t.Errorf("location(%s, %s, reverse=%t) = %q %t, want %q %t", fmtSel(tc.begin), fmtSel(tc.end), tc.reverse, k, back, tc.key, tc.backward)
		}
	}
}

func kvs(keys ...string) []KeyValue {
	out := make([]KeyValue, len(keys))
	for i, k := range keys {
		out[i] = KeyValue{Key: []byte(k), Value: []byte("v")}
	}
	return out
}

func keysOf(kvs []KeyValue) string {
	var s []string
	for _, kv := range kvs {
		s = append(s, string(kv.Key))
	}
	return strings.Join(s, ",")
}

type reply struct {
	kvs                  []KeyValue
	more, modified       bool
	shardBegin, shardEnd string
}

func TestNativeRangeLoopOnReply(t *testing.T) {
	t.Parallel()
	unlimited := rangeLimits{-1, 1, -1}
	bytesLimited := rangeLimits{-1, 1, 1000}
	for _, tc := range []struct {
		name       string
		begin, end KeySelector
		limits     rangeLimits
		reverse    bool
		replies    []reply
		wantSteps  []nativeStep
		wantBegin  KeySelector // after the last reply
		wantEnd    KeySelector
		wantKeys   string
		wantMore   bool
		wantRT     string // readThrough; "" = nil
		wantLimits rangeLimits
	}{
		// !rep.more with rows on a modified shard continues at the shard
		// boundary (:4839-4843).
		{
			name: "fwd/shard-exhausted", begin: sel("a", false, 1), end: sel("z", false, 1), limits: unlimited,
			replies:   []reply{{kvs("a", "b"), false, true, "", "m"}},
			wantSteps: []nativeStep{nativeStepNext}, wantBegin: sel("m", false, 1), wantEnd: sel("z", false, 1),
			wantKeys: "a,b", wantLimits: rangeLimits{-1, 0, -1},
		},
		{
			name: "rev/shard-exhausted", begin: sel("a", false, 1), end: sel("z", false, 1), limits: unlimited, reverse: true,
			replies:   []reply{{kvs("y", "x"), false, true, "m", "\xff\xff"}},
			wantSteps: []nativeStep{nativeStepNext}, wantBegin: sel("a", false, 1), wantEnd: sel("m", false, 1),
			wantKeys: "y,x", wantLimits: rangeLimits{-1, 0, -1},
		},
		// rep.more continues at firstGreaterThan(last) forward, firstGreaterOrEqual(last)
		// reverse (:4846-4851).
		{
			name: "fwd/more-FGT", begin: sel("a", false, 1), end: sel("z", false, 1), limits: unlimited,
			replies:   []reply{{kvs("a", "b"), true, true, "", "m"}},
			wantSteps: []nativeStep{nativeStepNext}, wantBegin: sel("b", true, 1), wantEnd: sel("z", false, 1),
			wantKeys: "a,b", wantLimits: rangeLimits{-1, 0, -1},
		},
		{
			name: "rev/more-FGE", begin: sel("a", false, 1), end: sel("z", false, 1), limits: unlimited, reverse: true,
			replies:   []reply{{kvs("y", "x"), true, true, "m", "\xff\xff"}},
			wantSteps: []nativeStep{nativeStepNext}, wantBegin: sel("a", false, 1), wantEnd: sel("x", false, 1),
			wantKeys: "y,x", wantLimits: rangeLimits{-1, 0, -1},
		},
		// No rows and no more on a modified shard: getRangeFallback (:4831-4836).
		{
			name: "fwd/empty-modified-falls-back", begin: sel("a", false, 1), end: sel("z", false, 1), limits: unlimited,
			replies:   []reply{{nil, false, true, "", "m"}},
			wantSteps: []nativeStep{nativeStepFallback}, wantBegin: sel("a", false, 1), wantEnd: sel("z", false, 1),
			wantLimits: unlimited,
		},
		// Unmodified and no more: finished, nothing past it (:4761).
		{
			name: "fwd/unmodified-done", begin: sel("a", false, 1), end: sel("k", false, 1), limits: unlimited,
			replies:   []reply{{kvs("a"), false, false, "", "m"}},
			wantSteps: []nativeStep{nativeStepDone}, wantBegin: sel("a", false, 1), wantEnd: sel("k", false, 1),
			wantKeys: "a", wantLimits: rangeLimits{-1, 0, -1},
		},
		// First reply finished with a modified, exhausted shard: more and
		// readThrough at the shard edge (:4796-4800).
		{
			name: "fwd/first-readThrough", begin: sel("a", false, 1), end: sel("z", false, 1), limits: bytesLimited,
			replies:   []reply{{kvs("a"), false, true, "", "m"}},
			wantSteps: []nativeStep{nativeStepDone}, wantBegin: sel("a", false, 1), wantEnd: sel("z", false, 1),
			wantKeys: "a", wantMore: true, wantRT: "m", wantLimits: rangeLimits{-1, 0, 990},
		},
		{
			name: "rev/first-readThrough", begin: sel("a", false, 1), end: sel("z", false, 1), limits: bytesLimited, reverse: true,
			replies:   []reply{{kvs("y"), false, true, "m", "\xff\xff"}},
			wantSteps: []nativeStep{nativeStepDone}, wantBegin: sel("a", false, 1), wantEnd: sel("z", false, 1),
			wantKeys: "y", wantMore: true, wantRT: "m", wantLimits: rangeLimits{-1, 0, 990},
		},
		// A later reply finishing on a modified, exhausted shard (:4813-4818).
		{
			name: "fwd/later-readThrough", begin: sel("a", false, 1), end: sel("z", false, 1), limits: rangeLimits{3, 1, -1},
			replies:   []reply{{kvs("a", "b"), false, true, "", "m"}, {kvs("n"), false, true, "m", "t"}},
			wantSteps: []nativeStep{nativeStepNext, nativeStepDone}, wantBegin: sel("m", false, 1), wantEnd: sel("z", false, 1),
			wantKeys: "a,b,n", wantMore: true, wantRT: "t", wantLimits: rangeLimits{0, 0, -1},
		},
		// First-reply shortcut more = modified || isReached || rep.more (:4774).
		{
			name: "fwd/first-more-from-reply", begin: sel("a", false, 1), end: sel("k", false, 1), limits: bytesLimited,
			replies:   []reply{{kvs("a"), true, false, "", "m"}},
			wantSteps: []nativeStep{nativeStepDone}, wantBegin: sel("a", false, 1), wantEnd: sel("k", false, 1),
			wantKeys: "a", wantMore: true, wantLimits: rangeLimits{-1, 0, 990},
		},
		{
			name: "fwd/first-more-from-isReached", begin: sel("a", false, 1), end: sel("k", false, 1), limits: rangeLimits{1, 1, -1},
			replies:   []reply{{kvs("a"), false, false, "", "m"}},
			wantSteps: []nativeStep{nativeStepDone}, wantBegin: sel("a", false, 1), wantEnd: sel("k", false, 1),
			wantKeys: "a", wantMore: true, wantLimits: rangeLimits{0, 0, -1},
		},
		{
			name: "fwd/first-more-modified-no-readThrough", begin: sel("a", false, 1), end: sel("z", false, 1), limits: bytesLimited,
			replies:   []reply{{kvs("a"), true, true, "", "m"}},
			wantSteps: []nativeStep{nativeStepDone}, wantBegin: sel("a", false, 1), wantEnd: sel("z", false, 1),
			wantKeys: "a", wantMore: true, wantLimits: rangeLimits{-1, 0, 990},
		},
		// bytes spent before minRows: neither reached nor satisfied, so the read
		// continues (A10), and the next request takes transformRangeLimits'
		// bytes == 0 branch (checked below).
		{
			name: "fwd/bytes0-minRows", begin: sel("a", false, 1), end: sel("k", false, 1), limits: rangeLimits{-1, 3, 5},
			replies:   []reply{{kvs("a"), true, false, "", "m"}, {kvs("b", "c"), true, false, "", "m"}},
			wantSteps: []nativeStep{nativeStepNext, nativeStepDone}, wantBegin: sel("a", true, 1), wantEnd: sel("k", false, 1),
			wantKeys: "a,b,c", wantMore: true, wantLimits: rangeLimits{-1, 0, 0},
		},
		// Reverse with begin lastLessOrEqual(k) and a reply ending at k: the
		// selectors count as unmodified (:4756-4759), so the read finishes
		// without readThrough instead of continuing at the shard boundary.
		{
			name: "rev/lastLessOrEqual-fix", begin: sel("c", true, 0), end: sel("z", false, 1), limits: unlimited, reverse: true,
			replies:   []reply{{kvs("e", "c"), false, true, "d", "\xff\xff"}},
			wantSteps: []nativeStep{nativeStepDone}, wantBegin: sel("c", true, 0), wantEnd: sel("z", false, 1),
			wantKeys: "e,c", wantLimits: rangeLimits{-1, 0, -1},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l := nativeRangeLoop{begin: tc.begin, end: tc.end, limits: tc.limits, reverse: tc.reverse}
			for i, r := range tc.replies {
				step, err := l.onReply(r.kvs, r.more, r.modified, []byte(r.shardBegin), []byte(r.shardEnd))
				if err != nil {
					t.Fatalf("reply %d: %v", i, err)
				}
				if step != tc.wantSteps[i] {
					t.Fatalf("reply %d: step %d, want %d", i, step, tc.wantSteps[i])
				}
				if tc.name == "fwd/bytes0-minRows" && i == 0 {
					if lim, lb := transformRangeLimits(l.limits, false); lim != 2 || lb != replyByteLimit {
						t.Errorf("next request limits (%d, %d), want (2, %d) (:4237-4238)", lim, lb, replyByteLimit)
					}
				}
			}
			if !equalSel(l.begin, tc.wantBegin) || !equalSel(l.end, tc.wantEnd) {
				t.Errorf("selectors %s %s, want %s %s", fmtSel(l.begin), fmtSel(l.end), fmtSel(tc.wantBegin), fmtSel(tc.wantEnd))
			}
			if got := keysOf(l.out.kvs); got != tc.wantKeys {
				t.Errorf("keys %q, want %q", got, tc.wantKeys)
			}
			if l.out.more != tc.wantMore {
				t.Errorf("more %t, want %t", l.out.more, tc.wantMore)
			}
			if string(l.out.readThrough) != tc.wantRT || (tc.wantRT == "") != (l.out.readThrough == nil) {
				t.Errorf("readThrough %q, want %q", l.out.readThrough, tc.wantRT)
			}
			if l.limits != tc.wantLimits {
				t.Errorf("limits %+v, want %+v", l.limits, tc.wantLimits)
			}
		})
	}
}

// Replies C++ asserts impossible (:4751-4752) are internal errors.
func TestNativeRangeLoopImpossibleReplies(t *testing.T) {
	t.Parallel()
	l := nativeRangeLoop{begin: sel("a", false, 1), end: sel("z", false, 1), limits: rangeLimits{-1, 1, -1}}
	if _, err := l.onReply(nil, true, true, []byte(""), []byte("m")); err == nil || !strings.Contains(err.Error(), "4100") {
		t.Errorf("more with no rows: %v, want internal_error", err)
	}
	l = nativeRangeLoop{begin: sel("a", false, 1), end: sel("z", false, 1), limits: rangeLimits{1, 1, -1}}
	if _, err := l.onReply(kvs("a", "b"), false, true, []byte(""), []byte("m")); err == nil || !strings.Contains(err.Error(), "4100") {
		t.Errorf("rows past the limit: %v, want internal_error", err)
	}
}

func TestGetRangeFinishedConflict(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name       string
		begin, end KeySelector
		r          nativeRangeResult
		reverse    bool
		want       [2]string
	}{
		// :4562-4567 and :4573-4578 with offsets 1 and no more: the selector keys.
		{"fwd/done", sel("b", false, 1), sel("f", false, 1), nativeRangeResult{kvs: kvs("b", "c")}, false, [2]string{"b", "f"}},
		// Forward more: end at keyAfter(last row) (:4578).
		{"fwd/more", sel("b", false, 1), sel("f", false, 1), nativeRangeResult{kvs: kvs("b", "c"), more: true}, false, [2]string{"b", "c\x00"}},
		// Reverse more: begin at the last (smallest) row (:4567).
		{"rev/more", sel("b", false, 1), sel("f", false, 1), nativeRangeResult{kvs: kvs("e", "d"), more: true}, true, [2]string{"d", "f"}},
		{"rev/more-begin-offset2", sel("b", false, 3), sel("f", false, 4), nativeRangeResult{kvs: kvs("e", "d"), more: true}, true, [2]string{"b", "e\x00"}},
		{"flags", sel("b", false, 1), sel("f", false, 1), nativeRangeResult{kvs: kvs("b"), readToBegin: true, readThroughEnd: true}, false, [2]string{"", "\xff\xff"}},
		// begin offset 0 with rows: the first row (:4567).
		{"fwd/begin-offset0", sel("c", false, 0), sel("f", false, 1), nativeRangeResult{kvs: kvs("b", "c")}, false, [2]string{"b", "f"}},
		// end offset 2: keyAfter(last row) even without more (:4575-4578).
		{"fwd/end-offset2", sel("b", false, 1), sel("e", false, 2), nativeRangeResult{kvs: kvs("b", "c")}, false, [2]string{"b", "c\x00"}},
		// Empty result, crossed selectors: the fixups at :4570-4572 and :4581-4583.
		{"fwd/crossed-fixups", sel("m", false, 0), sel("c", false, 1), nativeRangeResult{}, false, [2]string{"c", "m"}},
		{"rev/empty", sel("b", false, 1), sel("f", false, 1), nativeRangeResult{more: true}, true, [2]string{"b", "f"}},
	} {
		got := getRangeFinishedConflict(tc.begin, tc.end, &tc.r, tc.reverse)
		if string(got[0]) != tc.want[0] || string(got[1]) != tc.want[1] {
			t.Errorf("%s: [%q, %q), want [%q, %q)", tc.name, got[0], got[1], tc.want[0], tc.want[1])
		}
	}
}

// getRangeFallback's flags (:4482-4491).
func TestFallbackFlags(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		b, e          string
		more, reverse bool
		toBegin, thEn bool
	}{
		{"", "\xff\xff", false, false, true, true},
		{"", "\xff\xff", true, false, true, false},
		{"", "\xff\xff", false, true, true, true},
		{"", "\xff\xff", true, true, false, true},
		{"a", "z", false, false, false, false},
		{"a", "z", false, true, false, false},
	} {
		tb, te := fallbackFlags([]byte(tc.b), []byte(tc.e), tc.more, tc.reverse)
		if tb != tc.toBegin || te != tc.thEn {
			t.Errorf("fallbackFlags(%q, %q, more=%t, reverse=%t) = %t %t, want %t %t", tc.b, tc.e, tc.more, tc.reverse, tb, te, tc.toBegin, tc.thEn)
		}
	}
}

// resolveKey (:4447-4455): FGE and FGT are ready without a request. A nil
// transaction proves no request can be made.
func TestStartResolveKeyTrivial(t *testing.T) {
	t.Parallel()
	var tx *Transaction
	for _, tc := range []struct {
		sel  KeySelector
		want string
	}{
		{sel("k", false, 1), "k"},
		{sel("k", true, 1), "k\x00"},
		{sel("\xff\xff", true, 1), "\xff\xff"},
	} {
		fut, parked := tx.startResolveKey(context.Background(), tc.sel, 1)
		select {
		case <-parked:
		default:
			t.Errorf("%s: parked not closed", fmtSel(tc.sel))
		}
		k, err := fut.wait()
		if err != nil || string(k) != tc.want {
			t.Errorf("%s: %q %v, want %q", fmtSel(tc.sel), k, err, tc.want)
		}
	}
}

// The park seam fires the signal its context carries, once, and only that one.
func TestParkSignal(t *testing.T) {
	t.Parallel()
	notifyParked(context.Background()) // no signal: a no-op

	ctx, p := withParkSignal(context.Background())
	other, q := withParkSignal(context.Background())
	derived, cancel := context.WithCancel(ctx)
	defer cancel()
	notifyParked(derived)
	select {
	case <-p.Done():
	default:
		t.Fatal("signal in a derived context did not fire")
	}
	select {
	case <-q.Done():
		t.Fatal("another context's signal fired")
	default:
	}
	notifyParked(derived)                      // a second fire must not close twice
	if err := sleepCtx(other, 0); err != nil { // a delay parks too
		t.Fatal(err)
	}
	select {
	case <-q.Done():
	default:
		t.Fatal("sleepCtx did not fire the signal")
	}
	q.Fire()
}
