package recordlayer

import (
	"testing"
	"time"
)

// TestScanLimiterStateAnchorAt pins the RFC-198 re-anchor seam: AnchorAt moves
// ONLY the time anchor (the counters survive — they are the transaction's
// cumulative scan budget), ignores a zero instant ("no GRV" must never become
// an epoch-zero anchor that makes every elapsed measurement astronomical), and
// is nil-safe like every other method on the type.
func TestScanLimiterStateAnchorAt(t *testing.T) {
	t.Parallel()

	s := NewScanLimiterState()
	s.AddRecordScanned()
	s.AddBytesScanned(64)

	anchor := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	s.AnchorAt(anchor)
	if got := s.StartTime(); !got.Equal(anchor) {
		t.Fatalf("StartTime after AnchorAt = %v, want %v", got, anchor)
	}
	if s.RecordsScanned() != 1 || s.BytesScanned() != 64 {
		t.Fatalf("AnchorAt disturbed the counters (records=%d bytes=%d, want 1/64): "+
			"the re-anchor must move only the time anchor, never the transaction's "+
			"cumulative scan budget", s.RecordsScanned(), s.BytesScanned())
	}

	s.AnchorAt(time.Time{})
	if got := s.StartTime(); !got.Equal(anchor) {
		t.Fatalf("a zero instant moved the anchor to %v: 'no GRV yet' must be ignored, "+
			"not treated as an epoch-zero anchor", got)
	}

	var nilState *ScanLimiterState
	nilState.AnchorAt(anchor) // must not panic
}
