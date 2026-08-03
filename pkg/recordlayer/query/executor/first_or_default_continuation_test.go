package executor

// The first-or-default operator's continuation namespace holds three mutually
// exclusive states, one of which carries an ARBITRARY inner-leg continuation as
// its payload. Arbitrary means the payload can collide with any fixed marker in
// the same namespace, so the encoding has to keep them disjoint by construction
// rather than by luck. These tests pin that property directly, because the
// failure it prevents is silent: a checkpoint misread as "consumed" makes the
// operator flow nothing where its inner still had rows, which inverts an EXISTS.

import (
	"bytes"
	"errors"
	"testing"

	"fdb.dev/pkg/recordlayer"
)

// TestFirstOrDefaultContinuation_ConsumedTokenValuedInnerIsNotConsumed is the
// collision that the tagging exists for. The consumed marker is the single byte
// 0x01; an inner leg is perfectly entitled to produce a continuation whose bytes
// are exactly 0x01. Before tagging, the operator passed anything that was not
// the marker straight through to its inner — so that inner continuation WAS the
// marker, and resuming it would have reported the cursor exhausted.
func TestFirstOrDefaultContinuation_ConsumedTokenValuedInnerIsNotConsumed(t *testing.T) {
	t.Parallel()

	innerBytes := []byte{0x01} // byte-identical to singleResultConsumedToken
	encoded := encodeFirstOrDefaultCheckpoint(innerBytes)

	if bytes.Equal(encoded, singleResultConsumedToken) {
		t.Fatalf("a checkpoint on inner continuation %v encoded to the consumed "+
			"token %v — the two states are not disjoint", innerBytes, singleResultConsumedToken)
	}

	resume, got, err := decodeFirstOrDefaultContinuation(encoded)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resume != fodResumeInner {
		t.Fatalf("resume = %v, want fodResumeInner — an inner continuation of 0x01 "+
			"read as %v, which would report the cursor exhausted and flow no row "+
			"where the inner still had rows", resume, resume)
	}
	if !bytes.Equal(got, innerBytes) {
		t.Fatalf("decoded inner continuation = %v, want %v", got, innerBytes)
	}
}

// TestFirstOrDefaultContinuation_RestartTokenValuedInnerIsNotRestart is the same
// collision against the OTHER fixed marker. A restart re-runs the whole leg, so
// a checkpoint misread as a restart is not silent data loss but it is unbounded
// re-scanning — the livelock the checkpoint exists to avoid.
func TestFirstOrDefaultContinuation_RestartTokenValuedInnerIsNotRestart(t *testing.T) {
	t.Parallel()

	innerBytes := []byte{fodTagRestart}
	resume, got, err := decodeFirstOrDefaultContinuation(
		encodeFirstOrDefaultCheckpoint(innerBytes))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resume != fodResumeInner || !bytes.Equal(got, innerBytes) {
		t.Fatalf("resume = %v, inner = %v; want fodResumeInner with inner %v",
			resume, got, innerBytes)
	}
}

func TestFirstOrDefaultContinuation_RoundTrips(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name       string
		in         []byte
		wantResume fodResume
		wantInner  []byte
	}{
		{"empty is a fresh start", nil, fodResumeStart, nil},
		{"consumed marker", singleResultConsumedToken, fodResumeConsumed, nil},
		{"restart marker", []byte{fodTagRestart}, fodResumeStart, nil},
		{"checkpoint", encodeFirstOrDefaultCheckpoint([]byte{9, 8, 7}), fodResumeInner, []byte{9, 8, 7}},
		// A payload-free checkpoint means "stopped at the very beginning",
		// which is the same instruction as a restart.
		{"empty checkpoint", []byte{fodTagCheckpoint}, fodResumeStart, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			resume, inner, err := decodeFirstOrDefaultContinuation(tc.in)
			if err != nil {
				t.Fatalf("decode(%v): %v", tc.in, err)
			}
			if resume != tc.wantResume {
				t.Fatalf("decode(%v) resume = %v, want %v", tc.in, resume, tc.wantResume)
			}
			if !bytes.Equal(inner, tc.wantInner) {
				t.Fatalf("decode(%v) inner = %v, want %v", tc.in, inner, tc.wantInner)
			}
		})
	}
}

// TestFirstOrDefaultContinuation_CorruptSurfaces pins that an unrecognised
// namespace member is an error, never a silent fresh start — restarting on a
// corrupt token re-emits rows the caller already consumed.
func TestFirstOrDefaultContinuation_CorruptSurfaces(t *testing.T) {
	t.Parallel()

	for _, in := range [][]byte{
		{0x7f},                 // unknown tag
		{fodTagConsumed, 0x00}, // marker with a payload
		{fodTagRestart, 0x00},  // marker with a payload
	} {
		_, _, err := decodeFirstOrDefaultContinuation(in)
		if err == nil {
			t.Fatalf("decode(%v) returned no error — a corrupt continuation must "+
				"surface, not restart the leg and re-emit consumed rows", in)
		}
		var parseErr *recordlayer.ContinuationParseError
		if !errors.As(err, &parseErr) {
			t.Fatalf("decode(%v) error = %T (%v), want *recordlayer.ContinuationParseError",
				in, err, err)
		}
	}
}
