package executor

// RFC-128 §3.3 — unit tests for the LIMIT continuation envelope. These pin the
// encode/decode round-trip and the cross-page resume semantics of
// limitEnvelopeCursor in isolation (no FDB). The end-to-end SQL behavior is
// pinned by the FDB tests in pkg/relational/sqldriver/derived_limit_rfc128_test.go.

import (
	"context"
	"testing"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer"
)

// TestLimitContinuation_RoundTrip verifies encode→decode preserves the inner
// continuation and the remaining offset/limit across all field combinations,
// including the nil-inner case (distinct from a present-but-empty inner).
func TestLimitContinuation_RoundTrip(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name      string
		inner     recordlayer.RecordCursorContinuation
		remOffset int
		remLimit  int
		wantInner []byte // nil means "no inner continuation"
	}{
		{"nil-inner", nil, 5, 3, nil},
		{"start-inner", &recordlayer.StartContinuation{}, 2, 7, nil},
		{"end-inner", &recordlayer.EndContinuation{}, 0, 0, nil},
		{"present-inner", recordlayer.NewBytesContinuation([]byte{0, 0, 0, 4}), 0, 9, []byte{0, 0, 0, 4}},
		{"present-inner-offset", recordlayer.NewBytesContinuation([]byte{1, 2, 3}), 11, 0, []byte{1, 2, 3}},
		{"big-counts", recordlayer.NewBytesContinuation([]byte{9}), 1 << 30, 1 << 29, []byte{9}},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			enc, err := encodeLimitContinuation(tc.inner, tc.remOffset, tc.remLimit)
			if err != nil {
				t.Fatalf("encode: %v", err)
			}
			gotInner, gotOffset, gotLimit, derr := decodeLimitContinuation(enc, -1, -1)
			if derr != nil {
				t.Fatalf("decode: %v", derr)
			}
			if gotOffset != tc.remOffset {
				t.Errorf("remOffset = %d, want %d", gotOffset, tc.remOffset)
			}
			if gotLimit != tc.remLimit {
				t.Errorf("remLimit = %d, want %d", gotLimit, tc.remLimit)
			}
			if string(gotInner) != string(tc.wantInner) {
				t.Errorf("inner = %v, want %v", gotInner, tc.wantInner)
			}
		})
	}
}

// TestLimitContinuation_EmptyIsFullWindow verifies that an empty continuation
// (the first page) yields nil inner + the FULL offset/limit, so a fresh query
// applies the whole window.
func TestLimitContinuation_EmptyIsFullWindow(t *testing.T) {
	t.Parallel()
	inner, off, lim, err := decodeLimitContinuation(nil, 4, 6)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if inner != nil {
		t.Errorf("inner = %v, want nil", inner)
	}
	if off != 4 || lim != 6 {
		t.Errorf("(off,lim) = (%d,%d), want (4,6)", off, lim)
	}
}

// TestLimitContinuation_RejectsGarbage verifies malformed continuations are
// rejected rather than silently mis-decoded.
func TestLimitContinuation_RejectsGarbage(t *testing.T) {
	t.Parallel()
	// Too short.
	if _, _, _, err := decodeLimitContinuation([]byte{1, 2, 3}, 0, 0); err == nil {
		t.Error("expected error for too-short continuation")
	}
	// Wrong version byte.
	bad := make([]byte, 1+8+8+4)
	bad[0] = 99
	if _, _, _, err := decodeLimitContinuation(bad, 0, 0); err == nil {
		t.Error("expected error for wrong version byte")
	}
	// Inner-length mismatch: a valid blob with a trailing extra byte so the
	// declared inner length no longer matches the bytes present.
	mism, _ := encodeLimitContinuation(recordlayer.NewBytesContinuation([]byte{1, 2}), 0, 1)
	mism = append(mism, 0x00) // trailing junk → length field mismatch
	if _, _, _, err := decodeLimitContinuation(mism, 0, 0); err == nil {
		t.Error("expected error for inner-length mismatch")
	}
}

// pageBreakCursor wraps a resumable inner cursor and forces an out-of-band stop
// (ByteLimitReached) after pageSize value rows, returning the inner cursor's
// real continuation — exactly the shape a scan produces when a page budget is
// hit mid-stream. Used to drive limitEnvelopeCursor across simulated pages.
type pageBreakCursor struct {
	inner    recordlayer.RecordCursor[QueryResult]
	pageSize int
	served   int
}

func (c *pageBreakCursor) OnNext(ctx context.Context) (recordlayer.RecordCursorResult[QueryResult], error) {
	if c.served >= c.pageSize {
		// Out-of-band stop carrying the inner's last continuation so a resume
		// picks up from exactly here. We re-read the inner once to grab its
		// current continuation without consuming a value: instead, simplest is
		// to peek — but listCursor has no peek, so we model the page boundary
		// by stopping BEFORE reading and reporting the position via served.
		return recordlayer.NewResultNoNext[QueryResult](
			recordlayer.ByteLimitReached,
			recordlayer.NewBytesContinuation(listPos(c.served)),
		), nil
	}
	r, err := c.inner.OnNext(ctx)
	if err != nil {
		return r, err
	}
	if r.HasNext() {
		c.served++
	}
	return r, nil
}

func (c *pageBreakCursor) Close() error   { return c.inner.Close() }
func (c *pageBreakCursor) IsClosed() bool { return c.inner.IsClosed() }

// listPos encodes a listCursor 4-byte big-endian position continuation.
func listPos(pos int) []byte {
	return []byte{byte(pos >> 24), byte(pos >> 16), byte(pos >> 8), byte(pos)}
}

// TestLimitEnvelope_ResumeAcrossPage_NoReSkip is the core RFC-128 §3.3 proof in
// isolation: a LIMIT 3 OFFSET 2 window over [0..9], with a page that breaks
// mid-window, must NOT re-skip the offset or reset the limit on resume. Expected
// emitted rows (ids): [2, 3, 4]. The page breaks after the cursor has consumed
// 3 inner rows (skipping 0,1; emitting 2). Resume must continue with offset
// already consumed and limit at 2 remaining → emits 3, 4.
func TestLimitEnvelope_ResumeAcrossPage_NoReSkip(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	rows := make([]QueryResult, 10)
	for i := range rows {
		rows[i] = qr("id", int64(i))
	}

	const offset, limit = 2, 3

	// --- Page 1: break after consuming 3 inner rows (skip 0,1; emit 2). ---
	inner1 := recordlayer.FromListWithContinuation(rows, nil)
	page1 := &pageBreakCursor{inner: inner1, pageSize: 3}
	env1 := newLimitEnvelopeCursor(page1, offset, limit)

	var got []int64
	var resumeCont []byte
	for {
		r, err := env1.OnNext(ctx)
		if err != nil {
			t.Fatalf("page1 OnNext: %v", err)
		}
		if r.HasNext() {
			got = append(got, fieldVal(t, r.GetValue(), "id"))
			continue
		}
		// Page boundary. Capture the enveloped continuation for resume.
		cb, err := r.GetContinuation().ToBytes()
		if err != nil {
			t.Fatalf("continuation ToBytes: %v", err)
		}
		resumeCont = cb
		break
	}
	if len(got) != 1 || got[0] != 2 {
		t.Fatalf("page1 emitted %v, want [2] (skipped 0,1; emitted 2 before break)", got)
	}
	if len(resumeCont) == 0 {
		t.Fatal("page1 produced empty resume continuation")
	}

	// --- Resume: decode the envelope, drive a fresh inner from inner cont. ---
	innerCont, remOffset, remLimit, derr := decodeLimitContinuation(resumeCont, offset, limit)
	if derr != nil {
		t.Fatalf("decode resume continuation: %v", derr)
	}
	if remOffset != 0 {
		t.Errorf("remOffset on resume = %d, want 0 (offset already consumed)", remOffset)
	}
	if remLimit != 2 {
		t.Errorf("remLimit on resume = %d, want 2 (one row already emitted)", remLimit)
	}

	inner2 := recordlayer.FromListWithContinuation(rows, innerCont)
	env2 := newLimitEnvelopeCursor(inner2, remOffset, remLimit)
	rest := drainEnvelope(t, ctx, env2)
	got = append(got, rest...)

	want := []int64{2, 3, 4}
	if len(got) != len(want) {
		t.Fatalf("final emitted %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("final emitted %v, want %v", got, want)
		}
	}
}

// TestLimitEnvelope_OffsetSpansPageBreak proves the offset itself can straddle a
// page boundary: LIMIT 2 OFFSET 4 over [0..9], page breaks after 3 inner rows
// (mid-offset). Resume must continue skipping the remaining offset, then emit.
// Expected: [4, 5].
func TestLimitEnvelope_OffsetSpansPageBreak(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	rows := make([]QueryResult, 10)
	for i := range rows {
		rows[i] = qr("id", int64(i))
	}
	const offset, limit = 4, 2

	inner1 := recordlayer.FromListWithContinuation(rows, nil)
	page1 := &pageBreakCursor{inner: inner1, pageSize: 3} // breaks mid-offset
	env1 := newLimitEnvelopeCursor(page1, offset, limit)

	var got []int64
	var resumeCont []byte
	for {
		r, err := env1.OnNext(ctx)
		if err != nil {
			t.Fatalf("page1 OnNext: %v", err)
		}
		if r.HasNext() {
			got = append(got, fieldVal(t, r.GetValue(), "id"))
			continue
		}
		cb, _ := r.GetContinuation().ToBytes()
		resumeCont = cb
		break
	}
	if len(got) != 0 {
		t.Fatalf("page1 emitted %v, want [] (still inside offset)", got)
	}

	innerCont, remOffset, remLimit, derr := decodeLimitContinuation(resumeCont, offset, limit)
	if derr != nil {
		t.Fatalf("decode: %v", derr)
	}
	if remOffset != 1 {
		t.Errorf("remOffset on resume = %d, want 1 (skipped 3 of 4)", remOffset)
	}
	if remLimit != 2 {
		t.Errorf("remLimit on resume = %d, want 2 (none emitted)", remLimit)
	}

	inner2 := recordlayer.FromListWithContinuation(rows, innerCont)
	env2 := newLimitEnvelopeCursor(inner2, remOffset, remLimit)
	got = append(got, drainEnvelope(t, ctx, env2)...)

	want := []int64{4, 5}
	if len(got) != len(want) || got[0] != 4 || got[1] != 5 {
		t.Fatalf("final emitted %v, want %v", got, want)
	}
}

// TestLimitEnvelope_NoPageBreak_FullWindow proves the no-rollover case: the
// whole window fits in one page and the envelope is transparent.
func TestLimitEnvelope_NoPageBreak_FullWindow(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	rows := make([]QueryResult, 10)
	for i := range rows {
		rows[i] = qr("id", int64(i))
	}
	env := newLimitEnvelopeCursor(recordlayer.FromList(rows), 2, 3)
	got := drainEnvelope(t, ctx, env)
	want := []int64{2, 3, 4}
	if len(got) != len(want) || got[0] != 2 || got[1] != 3 || got[2] != 4 {
		t.Fatalf("emitted %v, want %v", got, want)
	}
}

// TestLimitEnvelope_NegativeLimit_OffsetOnly proves a negative limit (LIMIT <0,
// the "pure OFFSET, no cap" form) skips the offset and then emits ALL remaining
// rows — it must NOT collapse to empty. SQL `OFFSET n` without LIMIT is not
// valid in this grammar, so this guards a latent footgun rather than a live
// path.
func TestLimitEnvelope_NegativeLimit_OffsetOnly(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	rows := make([]QueryResult, 5)
	for i := range rows {
		rows[i] = qr("id", int64(i))
	}
	env := newLimitEnvelopeCursor(recordlayer.FromList(rows), 2, -1)
	got := drainEnvelope(t, ctx, env)
	want := []int64{2, 3, 4}
	if len(got) != len(want) || got[0] != 2 || got[1] != 3 || got[2] != 4 {
		t.Fatalf("negative-limit OFFSET 2 emitted %v, want %v", got, want)
	}
}

// TestLimitEnvelope_ZeroLimit proves LIMIT 0 emits nothing and stops cleanly.
func TestLimitEnvelope_ZeroLimit(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	rows := []QueryResult{qr("id", int64(0)), qr("id", int64(1))}
	env := newLimitEnvelopeCursor(recordlayer.FromList(rows), 0, 0)
	got := drainEnvelope(t, ctx, env)
	if len(got) != 0 {
		t.Fatalf("LIMIT 0 emitted %v, want []", got)
	}
	// Terminal is sticky: a second OnNext returns the same no-next result.
	r, err := env.OnNext(ctx)
	if err != nil {
		t.Fatalf("second OnNext: %v", err)
	}
	if r.HasNext() {
		t.Fatal("LIMIT 0 should never have a value")
	}
}

func drainEnvelope(t *testing.T, ctx context.Context, c recordlayer.RecordCursor[QueryResult]) []int64 {
	t.Helper()
	var out []int64
	for {
		r, err := c.OnNext(ctx)
		if err != nil {
			t.Fatalf("OnNext: %v", err)
		}
		if !r.HasNext() {
			return out
		}
		out = append(out, fieldVal(t, r.GetValue(), "id"))
	}
}
