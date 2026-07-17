package executor

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

// nljTestRows builds n {K:i} rows with tuple PKs — the PK feeds the
// continuation's check value.
func nljTestRows(field string, n int) []QueryResult {
	rows := make([]QueryResult, n)
	for i := range rows {
		r := dmap(map[string]any{field: int64(i)})
		r.PrimaryKey = tuple.Tuple{field, int64(i)}
		rows[i] = r
	}
	return rows
}

// nljEquiPreds builds the baked O.K = I.J equijoin predicate.
func nljEquiPreds() []predicates.QueryPredicate {
	return []predicates.QueryPredicate{
		&predicates.ComparisonPredicate{
			Operand: &values.FieldValue{
				Child:    &values.QuantifiedObjectValue{Correlation: values.NamedCorrelationIdentifier("O")},
				Field:    "K",
				Resolved: values.NewFieldPathOfSingle("K", 0, false),
			},
			Comparison: predicates.Comparison{
				Type: predicates.ComparisonEquals,
				Operand: &values.FieldValue{
					Child:    &values.QuantifiedObjectValue{Correlation: values.NamedCorrelationIdentifier("I")},
					Field:    "J",
					Resolved: values.NewFieldPathOfSingle("J", 0, false),
				},
			},
		},
	}
}

func nljTestCursor(t *testing.T, outer recordlayer.RecordCursor[QueryResult], inner []QueryResult, jt plans.JoinType, preds []predicates.QueryPredicate) *nljCursor {
	t.Helper()
	c, err := newNLJCursor(outer, inner, jt, "O", "I", preds, nil, EmptyEvaluationContext(), recordlayer.NewExecuteState(0))
	if err != nil {
		t.Fatalf("newNLJCursor: %v", err)
	}
	return c
}

// nljRowKey renders an emitted row for order-sensitive comparison.
func nljRowKey(qr QueryResult) string {
	if qr.Positional != nil {
		return fmt.Sprintf("%v|%v", qr.PrimaryKey, qr.Positional.Slots)
	}
	return fmt.Sprintf("%v|nil", qr.PrimaryKey)
}

// drainNLJ pulls the cursor to exhaustion, returning each emitted row's key
// and its continuation bytes.
func drainNLJ(t *testing.T, c *nljCursor) (keys []string, conts [][]byte) {
	t.Helper()
	ctx := context.Background()
	for {
		res, err := c.OnNext(ctx)
		if err != nil {
			t.Fatalf("OnNext: %v", err)
		}
		if !res.HasNext() {
			return keys, conts
		}
		keys = append(keys, nljRowKey(res.GetValue()))
		b, berr := res.GetContinuation().ToBytes()
		if berr != nil {
			t.Fatalf("continuation ToBytes: %v", berr)
		}
		conts = append(conts, b)
	}
}

// TestNLJContinuation_ResumeEverySplitPoint pins the real NLJ page
// continuation (RFC-180: the retired one-byte fake marker reached the outer
// child raw on resume, where a key-value cursor's raw-suffix fallback could
// silently accept it as a scan position — wrong rows). For EVERY page split
// point, resuming from the emitted continuation must reproduce exactly the
// uninterrupted run's remaining rows: the prior-outer position re-reads the
// current outer row, the check value verifies it, and the tuple-packed inner
// position (with the LEFT-OUTER matched flag) continues mid-inner.
func TestNLJContinuation_ResumeEverySplitPoint(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		outers int
		inners int
		jt     plans.JoinType
		preds  func() []predicates.QueryPredicate
	}{
		// Cross join (nil preds): every pair emits — pure mid-inner sweep.
		{"cross_linear", 3, 3, plans.JoinInner, func() []predicates.QueryPredicate { return nil }},
		// Equijoin, LEFT OUTER: outers beyond the inner key range are
		// UNMATCHED and emit null-padded — the matched flag must survive
		// the page boundary or resumed pages emit duplicate/missing pads.
		{"left_outer_linear", 4, 2, plans.JoinLeftOuter, nljEquiPreds},
		// ≥100 inner rows: the hash index builds; innerIdx indexes the
		// recomputed per-outer match list.
		{"equijoin_hash", 3, 120, plans.JoinInner, nljEquiPreds},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			outers := nljTestRows("K", tc.outers)
			inners := nljTestRows("J", tc.inners)

			full := nljTestCursor(t, recordlayer.FromList(outers), inners, tc.jt, tc.preds())
			fullKeys, conts := drainNLJ(t, full)
			if len(fullKeys) == 0 {
				t.Fatal("fixture emitted no rows — the sweep pins nothing")
			}
			if tc.name == "equijoin_hash" && full.hashIndex == nil {
				t.Fatal("hash fixture did not build the hash index")
			}

			for split := 1; split <= len(fullKeys); split++ {
				outerCont, rs, derr := decodeNLJContinuation(conts[split-1])
				if derr != nil {
					t.Fatalf("split %d: decode: %v", split, derr)
				}
				resumed := nljTestCursor(t,
					recordlayer.FromListWithContinuation(outers, outerCont),
					inners, tc.jt, tc.preds())
				resumed.applyResume(rs)
				gotKeys, _ := drainNLJ(t, resumed)
				want := fullKeys[split:]
				if len(gotKeys) != len(want) {
					t.Fatalf("split %d: resumed %d rows, want %d\nresumed: %v\nwant: %v",
						split, len(gotKeys), len(want), gotKeys, want)
				}
				for i := range want {
					if gotKeys[i] != want[i] {
						t.Fatalf("split %d row %d: resumed %q, want %q", split, i, gotKeys[i], want[i])
					}
				}
			}
		})
	}
}

// TestNLJContinuation_LegacyFakeMarkerRejected pins the loud decline for the
// retired one-byte fake marker (and any other unrecognized bytes): the decode
// must error, never forward the bytes to the outer child.
func TestNLJContinuation_LegacyFakeMarkerRejected(t *testing.T) {
	t.Parallel()
	for _, b := range [][]byte{{0}, {0xff, 0xfe}, []byte("garbage")} {
		if _, _, err := decodeNLJContinuation(b); err == nil {
			t.Fatalf("bytes %v must be rejected loudly", b)
		} else {
			var uc *UnsupportedContinuationError
			if !errors.As(err, &uc) {
				t.Fatalf("bytes %v: want UnsupportedContinuationError, got %T: %v", b, err, err)
			}
		}
	}
}

// TestNLJContinuation_FullDrainRejected pins that a FULL OUTER drain-phase
// continuation declines on resume (the drain bitmap has no serialized form).
func TestNLJContinuation_FullDrainRejected(t *testing.T) {
	t.Parallel()
	b, err := (&nljContinuation{fullOuter: true}).ToBytes()
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	_, _, derr := decodeNLJContinuation(b)
	if derr == nil || !strings.Contains(derr.Error(), "FULL OUTER") {
		t.Fatalf("drain marker must decline loudly, got %v", derr)
	}
}

// TestNLJContinuation_CheckValueMismatchRestarts pins the Java check-value
// contract (FlatMapPipelinedCursor, and this package's own flatMapCursor):
// resuming a mid-inner continuation over CHANGED outer data discards the
// stale inner position and RESTARTS that outer row's inner from scratch —
// never an error, never a resume of the stale index against the wrong row.
func TestNLJContinuation_CheckValueMismatchRestarts(t *testing.T) {
	t.Parallel()
	outers := nljTestRows("K", 2)
	inners := nljTestRows("J", 2)
	full := nljTestCursor(t, recordlayer.FromList(outers), inners, plans.JoinInner, nil)
	fullKeys, conts := drainNLJ(t, full)

	// Split after pair 2 of outer row 0 (innerIdx=2): a faithful resume
	// would skip that outer's pairs. With a TAMPERED outer row, the resume
	// must instead restart its inner at index 0.
	outerCont, rs, derr := decodeNLJContinuation(conts[1])
	if derr != nil {
		t.Fatalf("decode: %v", derr)
	}
	if rs == nil || rs.innerIdx == 0 {
		t.Fatal("fixture must capture a mid-inner state past index 0")
	}
	tampered := nljTestRows("K", 3)[1:]
	resumed := nljTestCursor(t, recordlayer.FromListWithContinuation(tampered, outerCont), inners, plans.JoinInner, nil)
	resumed.applyResume(rs)
	gotKeys, _ := drainNLJ(t, resumed)
	// The re-read (changed) outer row emits ALL its pairs (restart), then the
	// remaining outer rows follow — count = full inner width per re-read
	// outer, never the stale-suffix count.
	wantLen := len(inners) * len(tampered)
	if len(gotKeys) != wantLen {
		t.Fatalf("changed outer data must RESTART the row's inner (got %d rows, want %d)\ngot: %v\nfull run was: %v",
			len(gotKeys), wantLen, gotKeys, fullKeys)
	}
}

// TestNLJContinuation_FullOuterMidStreamRejected pins that EVERY FULL OUTER
// emission carries the declining marker — the cross-outer matchedInner bitmap
// has no serialized form, so a resumed page would rebuild it zeroed and the
// drain phase would re-pad already-matched inner rows (wrong rows). A
// mid-stream FULL continuation (e.g. captured under LIMIT) must not decode as
// resumable.
func TestNLJContinuation_FullOuterMidStreamRejected(t *testing.T) {
	t.Parallel()
	outers := nljTestRows("K", 2)
	inners := nljTestRows("J", 2)
	c := nljTestCursor(t, recordlayer.FromList(outers), inners, plans.JoinFullOuter, nil)
	res, err := c.OnNext(context.Background())
	if err != nil || !res.HasNext() {
		t.Fatalf("first FULL pair: %v", err)
	}
	b, berr := res.GetContinuation().ToBytes()
	if berr != nil {
		t.Fatalf("ToBytes: %v", berr)
	}
	if _, _, derr := decodeNLJContinuation(b); derr == nil || !strings.Contains(derr.Error(), "FULL OUTER") {
		t.Fatalf("a mid-stream FULL OUTER continuation must decline on decode, got %v", derr)
	}
}

// TestNLJContinuation_ArmedResumeSurvivesOuterStop pins the resumed-page
// corner: the outer child stops out-of-band BEFORE re-yielding the resumed
// row. The wrapped continuation must carry the still-armed mid-inner state —
// dropping it would replay that outer row from inner index 0 (duplicates).
func TestNLJContinuation_ArmedResumeSurvivesOuterStop(t *testing.T) {
	t.Parallel()
	outer := &pausingCursor{rows: nil, cont: []byte("outer-pos")}
	c := nljTestCursor(t, outer, nljTestRows("J", 3), plans.JoinInner, nil)
	c.applyResume(&nljResumeState{innerIdx: 2, outerMatched: true, check: []byte("pk")})
	res, err := c.OnNext(context.Background())
	if err != nil || res.HasNext() {
		t.Fatalf("outer must pause without a row: %v", err)
	}
	b, berr := res.GetContinuation().ToBytes()
	if berr != nil {
		t.Fatalf("ToBytes: %v", berr)
	}
	outerCont, rs, derr := decodeNLJContinuation(b)
	if derr != nil {
		t.Fatalf("decode: %v", derr)
	}
	if string(outerCont) != "outer-pos" {
		t.Fatalf("outer bytes = %q, want the wrapped outer position", outerCont)
	}
	if rs == nil || rs.innerIdx != 2 || !rs.outerMatched || string(rs.check) != "pk" {
		t.Fatalf("armed mid-inner state must ride the wrap, got %+v", rs)
	}
}

// pausingCursor emits its rows, then returns an out-of-band pause with a
// bytes continuation — models an outer child hitting a scan/time limit.
type pausingCursor struct {
	rows   []QueryResult
	pos    int
	closed bool
	cont   []byte
}

func (p *pausingCursor) OnNext(context.Context) (recordlayer.RecordCursorResult[QueryResult], error) {
	if p.pos < len(p.rows) {
		r := p.rows[p.pos]
		p.pos++
		return recordlayer.NewResultWithValue(r, recordlayer.NewBytesContinuation([]byte{byte(p.pos)})), nil
	}
	return recordlayer.NewResultNoNext[QueryResult](
		recordlayer.TimeLimitReached, recordlayer.NewBytesContinuation(p.cont),
	), nil
}
func (p *pausingCursor) Close() error   { p.closed = true; return nil }
func (p *pausingCursor) IsClosed() bool { return p.closed }

// TestNLJContinuation_BetweenOuterWrap pins the out-of-band outer stop: the
// outer child's continuation is WRAPPED in the NLJ envelope (never passed
// through raw), and decoding yields exactly the outer bytes with no
// mid-inner state.
func TestNLJContinuation_BetweenOuterWrap(t *testing.T) {
	t.Parallel()
	outer := &pausingCursor{rows: nljTestRows("K", 1), cont: []byte("outer-pos")}
	c := nljTestCursor(t, outer, nljTestRows("J", 1), plans.JoinInner, nil)
	ctx := context.Background()
	// Drain the one pair, then hit the outer pause.
	for {
		res, err := c.OnNext(ctx)
		if err != nil {
			t.Fatalf("OnNext: %v", err)
		}
		if res.HasNext() {
			continue
		}
		if res.GetNoNextReason() != recordlayer.TimeLimitReached {
			t.Fatalf("reason = %v, want TimeLimitReached", res.GetNoNextReason())
		}
		b, berr := res.GetContinuation().ToBytes()
		if berr != nil {
			t.Fatalf("ToBytes: %v", berr)
		}
		outerCont, rs, derr := decodeNLJContinuation(b)
		if derr != nil {
			t.Fatalf("decode: %v", derr)
		}
		if string(outerCont) != "outer-pos" {
			t.Fatalf("outer bytes = %q, want the wrapped outer position", outerCont)
		}
		if rs != nil {
			t.Fatal("a between-outer stop must carry no mid-inner state")
		}
		return
	}
}
