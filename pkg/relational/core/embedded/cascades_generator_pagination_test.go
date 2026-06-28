package embedded

import (
	"errors"
	"testing"

	"fdb.dev/pkg/recordlayer"
)

// pageContinuationState is the load-bearing decision behind the RFC-127 (audit P0) fix: the SQL
// internal drain (paginatingRows) must decide exhaustion from IsEnd() (≡ NoNextReason.SourceExhausted),
// NEVER from ToBytes()==nil. A non-terminal StartContinuation has ToBytes()==nil, byte-identical to an
// EndContinuation; the old code treated that nil as exhaustion and silently truncated the result set.
// These cases pin the full decision table deterministically (no DB).
func TestPageContinuationState(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name          string
		cont          recordlayer.RecordCursorContinuation
		reason        recordlayer.NoNextReason
		wantExhausted bool
		wantBytes     []byte
		wantErr       bool // ScanLimitReachedError (→ 54F01)
	}{
		{"nil continuation", nil, recordlayer.SourceExhausted, true, nil, false},
		{"end continuation", &recordlayer.EndContinuation{}, recordlayer.SourceExhausted, true, nil, false},
		// Resumable position → keep draining (the normal multi-page path).
		{"bytes continuation", recordlayer.NewBytesContinuation([]byte("pos")), recordlayer.ReturnLimitReached, false, []byte("pos"), false},
		// THE BUG: non-end StartContinuation (nil bytes) must NOT be exhaustion.
		//   out-of-band (scan/time/byte) before any resumable progress → 54F01, not data loss.
		{"start + scan limit", &recordlayer.StartContinuation{}, recordlayer.ScanLimitReached, false, nil, true},
		{"start + time limit", &recordlayer.StartContinuation{}, recordlayer.TimeLimitReached, false, nil, true},
		{"start + byte limit", &recordlayer.StartContinuation{}, recordlayer.ByteLimitReached, false, nil, true},
		//   in-band ReturnLimitReached with zero rows ⟹ LIMIT 0 → clean exhaustion, no data lost.
		{"start + return limit (LIMIT 0)", &recordlayer.StartContinuation{}, recordlayer.ReturnLimitReached, true, nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			exhausted, b, err := pageContinuationState(tc.cont, tc.reason)
			if tc.wantErr {
				var sle *recordlayer.ScanLimitReachedError
				if !errors.As(err, &sle) {
					t.Fatalf("err = %v, want *ScanLimitReachedError (→ 54F01)", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("err = %v, want nil", err)
			}
			if exhausted != tc.wantExhausted {
				t.Errorf("exhausted = %v, want %v", exhausted, tc.wantExhausted)
			}
			if string(b) != string(tc.wantBytes) {
				t.Errorf("bytes = %q, want %q", b, tc.wantBytes)
			}
		})
	}
}

// TestContinuationExhaustionByIsEndNotBytes pins the cursor contract the bug violated: a non-end
// continuation may carry nil bytes, so exhaustion MUST be classified by IsEnd(), never ToBytes()==nil.
// (Note the asymmetry, RFC-127 §4.3: the KVCursor leaf special-cases lastKey==null ⟹ isEnd==true; the
// non-end-with-nil-bytes case is produced by composite cursors — RowLimited/Union/Sort/MapWhile — that
// stop on a limit before progress. StartContinuation is that case.)
func TestContinuationExhaustionByIsEndNotBytes(t *testing.T) {
	t.Parallel()
	start := &recordlayer.StartContinuation{}
	end := &recordlayer.EndContinuation{}

	sb, _ := start.ToBytes()
	eb, _ := end.ToBytes()
	if sb != nil || eb != nil {
		t.Fatalf("expected both Start and End ToBytes()==nil; got start=%v end=%v", sb, eb)
	}
	// The ONLY distinguishing signal:
	if start.IsEnd() {
		t.Error("StartContinuation.IsEnd() = true, want false (non-terminal)")
	}
	if !end.IsEnd() {
		t.Error("EndContinuation.IsEnd() = false, want true (terminal)")
	}
}
