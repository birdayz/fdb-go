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
				resumed.armResume(outerCont, rs)
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

// TestNLJContinuation_DoubleResumeEverySplitPair pins the resumed-page outer
// seeding (armResume): a token taken FROM a resumed page must carry the real
// outer position. Without the seed, priorOuterCont on the resumed page is nil,
// the second-generation token encodes an EMPTY outer position, and the second
// resume replays the outer scan from row 0 — silent duplicates. Sweeps every
// (first split, second split) pair.
func TestNLJContinuation_DoubleResumeEverySplitPair(t *testing.T) {
	t.Parallel()
	outers := nljTestRows("K", 3)
	inners := nljTestRows("J", 3)
	full := nljTestCursor(t, recordlayer.FromList(outers), inners, plans.JoinInner, nil)
	fullKeys, conts := drainNLJ(t, full)

	for s1 := 1; s1 < len(fullKeys); s1++ {
		oc1, rs1, err := decodeNLJContinuation(conts[s1-1])
		if err != nil {
			t.Fatalf("split %d decode: %v", s1, err)
		}
		r1 := nljTestCursor(t, recordlayer.FromListWithContinuation(outers, oc1), inners, plans.JoinInner, nil)
		r1.armResume(oc1, rs1)
		keys1, conts1 := drainNLJ(t, r1)
		for s2 := 1; s2 <= len(keys1); s2++ {
			oc2, rs2, err := decodeNLJContinuation(conts1[s2-1])
			if err != nil {
				t.Fatalf("split %d/%d decode: %v", s1, s2, err)
			}
			r2 := nljTestCursor(t, recordlayer.FromListWithContinuation(outers, oc2), inners, plans.JoinInner, nil)
			r2.armResume(oc2, rs2)
			keys2, _ := drainNLJ(t, r2)
			got := append(append(append([]string{}, fullKeys[:s1]...), keys1[:s2]...), keys2...)
			if len(got) != len(fullKeys) {
				t.Fatalf("splits %d/%d: %d total rows, want %d (duplicated/dropped outer prefix)\ngot: %v\nwant: %v",
					s1, s2, len(got), len(fullKeys), got, fullKeys)
			}
			for i := range fullKeys {
				if got[i] != fullKeys[i] {
					t.Fatalf("splits %d/%d row %d: %q, want %q", s1, s2, i, got[i], fullKeys[i])
				}
			}
		}
	}
}

// TestNLJContinuation_FullOuterExecutorRejectsAllTokens pins the executor
// guard: a FULL OUTER plan rejects EVERY incoming continuation — including a
// mid-inner token minted by a previous binary version (kind 0, no FULL
// marker), which the decoder alone would accept and resume with a zeroed
// matchedInner bitmap (re-padded rows). Join-type context is the authority.
func TestNLJContinuation_FullOuterExecutorRejectsAllTokens(t *testing.T) {
	t.Parallel()
	outerAlias := values.NamedCorrelationIdentifier("TO")
	innerAlias := values.NamedCorrelationIdentifier("TI")
	evalCtx := EmptyEvaluationContext()
	state := recordlayer.NewExecuteState(0)
	for _, alias := range []values.CorrelationIdentifier{outerAlias, innerAlias} {
		tt := evalCtx.GetOrCreateTempTable(alias, state)
		for _, r := range nljTestRows("K", 2) {
			if err := tt.Add(r); err != nil {
				t.Fatalf("seed: %v", err)
			}
		}
	}
	plan := plans.NewRecordQueryNestedLoopJoinPlan(
		plans.NewRecordQueryTempTableScanPlan(outerAlias),
		plans.NewRecordQueryTempTableScanPlan(innerAlias),
		nil, plans.JoinFullOuter, "TO", "TI", nil,
	)
	baseVersionToken, err := (&nljContinuation{hasInner: true, innerIdx: 1, outerMatched: true}).ToBytes()
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	_, execErr := executeNestedLoopJoin(context.Background(), plan, nil, evalCtx, baseVersionToken,
		recordlayer.ExecuteProperties{State: state})
	if execErr == nil || !strings.Contains(execErr.Error(), "FULL OUTER") {
		t.Fatalf("a FULL OUTER plan must reject every incoming continuation, got %v", execErr)
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
	resumed.armResume(outerCont, rs)
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
	c.armResume(nil, &nljResumeState{innerIdx: 2, outerMatched: true, check: []byte("pk")})
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

// TestNLJContinuation_ArmedStateAppliesToLaterMatchingRow pins Java's
// kept-armed contract (FlatMapPipelinedCursor nulls initialInnerContinuation
// ONLY in the match branch): when a NEW outer row appears at the resume
// position (inserted between transactions before the saved row), the
// mismatching row runs its inner from scratch while the armed state WAITS,
// and the saved row — arriving later — resumes at its saved inner position.
// Discarding on the first mismatch would re-emit the saved row's
// already-delivered pairs: duplicate rows.
func TestNLJContinuation_ArmedStateAppliesToLaterMatchingRow(t *testing.T) {
	t.Parallel()
	outers := nljTestRows("K", 2)
	inners := nljTestRows("J", 3)
	full := nljTestCursor(t, recordlayer.FromList(outers), inners, plans.JoinInner, nil)
	fullKeys, conts := drainNLJ(t, full)
	if len(fullKeys) != 6 {
		t.Fatalf("cross fixture must emit 6 pairs, got %d", len(fullKeys))
	}

	// conts[3] = after the saved outer row K1's FIRST pair (innerIdx=1).
	outerCont, rs, derr := decodeNLJContinuation(conts[3])
	if derr != nil {
		t.Fatalf("decode: %v", derr)
	}
	if rs == nil || rs.innerIdx != 1 {
		t.Fatalf("fixture must capture innerIdx=1 on the second outer, got %+v", rs)
	}

	// Insert a NEW row X at the resume position: the resumed outer yields
	// X (mismatch) first, then the saved K1 (match).
	x := dmap(map[string]any{"K": int64(99)})
	x.PrimaryKey = tuple.Tuple{"K", int64(99)}
	newOuters := []QueryResult{outers[0], x, outers[1]}
	resumed := nljTestCursor(t, recordlayer.FromListWithContinuation(newOuters, outerCont), inners, plans.JoinInner, nil)
	resumed.armResume(outerCont, rs)
	gotKeys, _ := drainNLJ(t, resumed)

	// X emits ALL 3 pairs; the saved K1 resumes at innerIdx=1 → 2 pairs.
	// 6 rows (X full + K1 full) means the armed state was discarded and
	// K1's first pair duplicated.
	if len(gotKeys) != 5 {
		t.Fatalf("armed state must apply to the LATER matching row (want 3 new + 2 resumed = 5 rows), got %d: %v",
			len(gotKeys), gotKeys)
	}
	// The last two rows must be exactly the full run's K1 suffix.
	for i, want := range fullKeys[4:] {
		if gotKeys[3+i] != want {
			t.Fatalf("resumed K1 suffix row %d = %q, want %q", i, gotKeys[3+i], want)
		}
	}
}

// TestNLJContinuation_KeyPositionalInnerResume pins the key-positional inner
// position: the saved ordinal is a SNAPSHOT index, so an inner row inserted
// BEFORE the position between transactions shifts it — the resume must
// reposition by the saved inner PK (Java's inner-cursor continuation is
// key-positional through its scan) and emit exactly the unemitted pairs,
// never a duplicate of the last-delivered pair or a skipped one.
func TestNLJContinuation_KeyPositionalInnerResume(t *testing.T) {
	t.Parallel()
	outers := nljTestRows("K", 1)
	inners := nljTestRows("J", 3)
	full := nljTestCursor(t, recordlayer.FromList(outers), inners, plans.JoinInner, nil)
	fullKeys, conts := drainNLJ(t, full)
	if len(fullKeys) != 3 {
		t.Fatalf("fixture: %d rows", len(fullKeys))
	}

	// Split after pair 2 (inner J1 emitted; saved ordinal 2, saved PK J1).
	outerCont, rs, derr := decodeNLJContinuation(conts[1])
	if derr != nil {
		t.Fatalf("decode: %v", derr)
	}
	if rs == nil || len(rs.innerPK) == 0 {
		t.Fatalf("mid-inner state must carry the inner PK, got %+v", rs)
	}

	// Insert a NEW inner row at the FRONT: ordinal 2 now points at J1 (the
	// already-delivered row) — a pure-ordinal resume would re-emit it.
	x := dmap(map[string]any{"J": int64(99)})
	x.PrimaryKey = tuple.Tuple{"J", int64(99)}
	shifted := append([]QueryResult{x}, inners...)
	resumed := nljTestCursor(t, recordlayer.FromListWithContinuation(outers, outerCont), shifted, plans.JoinInner, nil)
	resumed.armResume(outerCont, rs)
	gotKeys, _ := drainNLJ(t, resumed)
	// After J1 in the shifted list comes only J2: exactly one row.
	if len(gotKeys) != 1 || gotKeys[0] != fullKeys[2] {
		t.Fatalf("key-positional resume must emit exactly the unemitted suffix after the saved PK, got %v (want [%v])",
			gotKeys, fullKeys[2])
	}

	// Deleted-PK fallback: the saved row vanished — the ordinal is the
	// documented best-effort residue (no repositioning possible).
	without := []QueryResult{inners[0], inners[2]} // J1 deleted
	resumed2 := nljTestCursor(t, recordlayer.FromListWithContinuation(outers, outerCont), without, plans.JoinInner, nil)
	resumed2.armResume(outerCont, rs)
	got2, _ := drainNLJ(t, resumed2)
	if len(got2) != 0 {
		// ordinal 2 over a 2-row list = past the end → no rows; pin the
		// fallback's exact best-effort shape so a change is deliberate.
		t.Fatalf("deleted-PK fallback: ordinal best-effort expected 0 rows, got %v", got2)
	}
}

// TestNLJContinuation_UnresumableOuterDeclines pins the zero-length-token
// corner: a LEFT NLJ over an outer whose row continuations marshal to NO
// bytes (a StartContinuation-style zero-marshaling token — the
// value+nil-continuation shape itself is banned by NewResultWithValue)
// emits its advanced-outer envelope after the null pad — with nothing to
// restore, that envelope used to marshal to ZERO bytes, which the
// relational driver reads as exhaustion and a fresh executor call reads
// as a full restart. The token must be NON-empty and decline loudly on
// decode.
func TestNLJContinuation_UnresumableOuterDeclines(t *testing.T) {
	t.Parallel()
	// One outer row with an EMPTY continuation, no inner match → LEFT pad,
	// whose advanced-outer continuation has no outer bytes.
	outer := &bareContinuationCursor{rows: nljTestRows("K", 1)}
	c := nljTestCursor(t, outer, nil, plans.JoinLeftOuter, nil)
	res, err := c.OnNext(context.Background())
	if err != nil || !res.HasNext() {
		t.Fatalf("LEFT pad emission: %v", err)
	}
	b, berr := res.GetContinuation().ToBytes()
	if berr != nil {
		t.Fatalf("ToBytes: %v", berr)
	}
	if len(b) == 0 {
		t.Fatal("the advanced-outer envelope must never marshal to zero bytes")
	}
	if _, _, derr := decodeNLJContinuation(b); derr == nil || !strings.Contains(derr.Error(), "no resume position") {
		t.Fatalf("a continuation-less outer position must decline loudly on resume, got %v", derr)
	}
}

// bareContinuationCursor emits rows whose continuations carry NO bytes —
// the zero-marshaling corner (StartContinuation-shaped tokens).
type bareContinuationCursor struct {
	rows   []QueryResult
	pos    int
	closed bool
}

func (p *bareContinuationCursor) OnNext(context.Context) (recordlayer.RecordCursorResult[QueryResult], error) {
	if p.pos < len(p.rows) {
		r := p.rows[p.pos]
		p.pos++
		// A continuation whose bytes marshal EMPTY — the zero-byte
		// envelope corner this test pins.
		return recordlayer.NewResultWithValue(r, &recordlayer.StartContinuation{}), nil
	}
	return recordlayer.NewResultNoNext[QueryResult](
		recordlayer.SourceExhausted, &recordlayer.EndContinuation{},
	), nil
}
func (p *bareContinuationCursor) Close() error   { p.closed = true; return nil }
func (p *bareContinuationCursor) IsClosed() bool { return p.closed }

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

// flakyOuterCursor emits its rows, pauses out-of-band ONCE, then yields MORE
// rows — a re-call after the pause that re-pulls the outer (missing terminal
// replay guard) surfaces the extra rows instead of replaying the pause.
type flakyOuterCursor struct {
	first  []QueryResult
	rest   []QueryResult
	paused bool
	pos    int
	closed bool
}

func (f *flakyOuterCursor) OnNext(context.Context) (recordlayer.RecordCursorResult[QueryResult], error) {
	if f.pos < len(f.first) {
		r := f.first[f.pos]
		f.pos++
		return recordlayer.NewResultWithValue(r, recordlayer.NewBytesContinuation([]byte{byte(f.pos)})), nil
	}
	if !f.paused {
		f.paused = true
		return recordlayer.NewResultNoNext[QueryResult](
			recordlayer.TimeLimitReached, recordlayer.NewBytesContinuation([]byte{0xAB})), nil
	}
	if f.pos < len(f.first)+len(f.rest) {
		r := f.rest[f.pos-len(f.first)]
		f.pos++
		return recordlayer.NewResultWithValue(r, recordlayer.NewBytesContinuation([]byte{byte(f.pos)})), nil
	}
	return recordlayer.NewResultNoNext[QueryResult](
		recordlayer.SourceExhausted, &recordlayer.EndContinuation{},
	), nil
}
func (f *flakyOuterCursor) Close() error   { f.closed = true; return nil }
func (f *flakyOuterCursor) IsClosed() bool { return f.closed }

// TestNLJContinuation_TerminalReplayOnRecall pins Java's cached-terminal
// contract (FlatMapPipelinedCursor:123-125) on the NLJ: a re-call after an
// out-of-band stop replays the identical result — never re-pulls the outer,
// whose next pull here would surface MORE rows.
func TestNLJContinuation_TerminalReplayOnRecall(t *testing.T) {
	t.Parallel()
	rows := nljTestRows("K", 2)
	outer := &flakyOuterCursor{first: rows[:1], rest: rows[1:]}
	c := nljTestCursor(t, outer, nljTestRows("J", 1), plans.JoinInner, nil)
	ctx := context.Background()
	var first recordlayer.RecordCursorResult[QueryResult]
	for {
		res, err := c.OnNext(ctx)
		if err != nil {
			t.Fatalf("drain: %v", err)
		}
		if !res.HasNext() {
			first = res
			break
		}
	}
	if first.GetNoNextReason() != recordlayer.TimeLimitReached {
		t.Fatalf("fixture must stop out-of-band, got %v", first.GetNoNextReason())
	}
	again, err := c.OnNext(ctx)
	if err != nil {
		t.Fatalf("re-call: %v", err)
	}
	if again.HasNext() {
		t.Fatal("re-call after the out-of-band stop must replay the terminal result, not surface more outer rows")
	}
	fb, _ := first.GetContinuation().ToBytes()
	ab, _ := again.GetContinuation().ToBytes()
	if again.GetNoNextReason() != first.GetNoNextReason() || string(fb) != string(ab) {
		t.Fatalf("re-call (%v, %v) must equal the cached terminal (%v, %v)",
			again.GetNoNextReason(), ab, first.GetNoNextReason(), fb)
	}
}
