package types

// Round-trip fuzz for wire types that the differential fuzzer (cmd/fdb-diff-oracle)
// does NOT cover but that ARE serialized in production: SplitRangeRequest /
// SplitRangeReply / WaitMetricsRequest / WatchValueRequest. Differential fuzzing
// against C++ catches wire-format divergences; round-trip fuzzing catches
// Marshal/Unmarshal symmetry bugs (slot-index typos, padding miscalculations,
// missing fields). Cheaper to maintain than a C++ oracle entry.

import (
	"bytes"
	"testing"

	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/wire"
)

// fuzzData is a tiny consumer that drains random fuzz bytes deterministically
// into struct fields. Mirrors cmd/fdb-diff-oracle/fuzz_test.go's fuzzReader.
type fuzzData struct {
	b []byte
}

func (f *fuzzData) byte() byte {
	if len(f.b) == 0 {
		return 0
	}
	v := f.b[0]
	f.b = f.b[1:]
	return v
}

func (f *fuzzData) bool() bool { return f.byte()&1 == 1 }

func (f *fuzzData) int64() int64 {
	var v int64
	for i := 0; i < 8; i++ {
		v |= int64(f.byte()) << (i * 8)
	}
	return v
}

// bytesN returns up to maxLen bytes; cap protects against pathological inputs.
func (f *fuzzData) bytesN(maxLen int) []byte {
	n := int(f.byte())
	if n > maxLen {
		n = maxLen
	}
	if n > len(f.b) {
		n = len(f.b)
	}
	out := make([]byte, n)
	copy(out, f.b[:n])
	f.b = f.b[n:]
	return out
}

// FuzzSplitRangeRequest_RoundTrip checks that Marshal→Unmarshal preserves
// the scalar fields. Nested types (KeyRangeRef, ReplyPromise, TenantInfo)
// have their own round-trip semantics through their UnmarshalFromReader.
func FuzzSplitRangeRequest_RoundTrip(f *testing.F) {
	f.Add([]byte{1, 2, 3, 0x42, 0, 0, 0, 0, 0, 0, 0, 4, 5, 6, 7})
	f.Add([]byte{0, 0, 0, 0, 0, 0, 0, 0, 0, 0})

	f.Fuzz(func(t *testing.T, data []byte) {
		fd := &fuzzData{b: data}
		begin := fd.bytesN(64)
		end := fd.bytesN(64)
		// C++ KeyRangeRef serialization is lossy for (begin=nonempty, end=empty):
		// the deserializer can't tell that case apart from a single-key range
		// optimized to (first=end, second=empty). Skip — the input is outside
		// FDB's valid KeyRange domain (end must be ≥ begin's successor).
		if len(end) == 0 && len(begin) > 0 {
			t.Skip("C++-undefined: end=empty with begin=nonempty")
		}
		req := &SplitRangeRequest{
			Keys:       KeyRangeRef{Begin: begin, End: end},
			ChunkSize:  fd.int64(),
			TenantInfo: TenantInfo{TenantId: fd.int64()},
		}

		bytes := req.MarshalFDB()

		var got SplitRangeRequest
		if err := got.UnmarshalFDB(bytes); err != nil {
			t.Fatalf("UnmarshalFDB: %v", err)
		}
		if got.ChunkSize != req.ChunkSize {
			t.Errorf("ChunkSize: got %d, want %d", got.ChunkSize, req.ChunkSize)
		}
		if !equalBytes(got.Keys.Begin, req.Keys.Begin) {
			t.Errorf("Keys.Begin: got %x, want %x", got.Keys.Begin, req.Keys.Begin)
		}
		if !equalBytes(got.Keys.End, req.Keys.End) {
			t.Errorf("Keys.End: got %x, want %x", got.Keys.End, req.Keys.End)
		}
		if got.TenantInfo.TenantId != req.TenantInfo.TenantId {
			t.Errorf("TenantId: got %d, want %d", got.TenantInfo.TenantId, req.TenantInfo.TenantId)
		}
	})
}

// FuzzSplitRangeReply_RoundTrip exercises the reply path. SplitPoints
// is a single packed []byte (FDB packs tuple-encoded keys into one buffer);
// the wire-level concern is round-tripping the raw bytes.
func FuzzSplitRangeReply_RoundTrip(f *testing.F) {
	f.Add([]byte{0, 0, 0, 0, 0, 0, 0, 0})
	f.Add([]byte{3, 1, 2, 3, 4, 5, 6, 7, 8})
	f.Add([]byte{})

	f.Fuzz(func(t *testing.T, data []byte) {
		reply := &SplitRangeReply{
			SplitPoints: data,
		}
		bs := reply.MarshalFDB()

		var got SplitRangeReply
		if err := got.UnmarshalFDB(bs); err != nil {
			t.Fatalf("UnmarshalFDB: %v", err)
		}
		if !equalBytes(got.SplitPoints, reply.SplitPoints) {
			t.Errorf("SplitPoints: got %x, want %x", got.SplitPoints, reply.SplitPoints)
		}
	})
}

// FuzzWaitMetricsRequest_RoundTrip covers the GetEstimatedRangeSize path.
func FuzzWaitMetricsRequest_RoundTrip(f *testing.F) {
	f.Add([]byte{2, 0xAB, 0xCD, 2, 0xEF, 0xFE, 0, 0, 0, 0, 0, 0, 0, 0x42})
	f.Add([]byte{0, 0, 0, 0, 0, 0, 0, 0})

	f.Fuzz(func(t *testing.T, data []byte) {
		fd := &fuzzData{b: data}
		begin := fd.bytesN(64)
		end := fd.bytesN(64)
		if len(end) == 0 && len(begin) > 0 {
			t.Skip("C++-undefined: end=empty with begin=nonempty")
		}
		req := &WaitMetricsRequest{
			Keys:       KeyRangeRef{Begin: begin, End: end},
			MinVersion: fd.int64(),
		}

		bytes := req.MarshalFDB()

		var got WaitMetricsRequest
		if err := got.UnmarshalFDB(bytes); err != nil {
			t.Fatalf("UnmarshalFDB: %v", err)
		}
		if got.MinVersion != req.MinVersion {
			t.Errorf("MinVersion: got %d, want %d", got.MinVersion, req.MinVersion)
		}
		if !equalBytes(got.Keys.Begin, req.Keys.Begin) {
			t.Errorf("Keys.Begin: got %x, want %x", got.Keys.Begin, req.Keys.Begin)
		}
		if !equalBytes(got.Keys.End, req.Keys.End) {
			t.Errorf("Keys.End: got %x, want %x", got.Keys.End, req.Keys.End)
		}
	})
}

// FuzzWatchValueRequest_MarshalNoPanic. WatchValueRequest has no
// UnmarshalFDB (server-only), so we can't round-trip. Smoke fuzz: random
// fields, MarshalFDB must not panic and must produce parseable bytes.
func FuzzWatchValueRequest_MarshalNoPanic(f *testing.F) {
	f.Add([]byte{4, 1, 2, 3, 4, 1, 2, 0, 0xAA, 0, 0, 0, 0, 0, 0, 0})
	f.Add([]byte{0})

	f.Fuzz(func(t *testing.T, data []byte) {
		fd := &fuzzData{b: data}
		req := &WatchValueRequest{
			Key:      fd.bytesN(64),
			HasValue: fd.bool(),
			Value:    fd.bytesN(64),
			Version:  fd.int64(),
		}

		// Must not panic.
		bytes := req.MarshalFDB()

		// Bytes must form a valid wire object that the generic Reader can
		// parse without panicking — catches malformed vtable/RelativeOffset
		// emission. We don't decode field values (no Unmarshal exists).
		_, err := wire.NewReader(bytes)
		if err != nil {
			t.Fatalf("MarshalFDB produced unparseable bytes: %v", err)
		}
	})
}

func equalBytes(a, b []byte) bool {
	if len(a) == 0 && len(b) == 0 {
		return true
	}
	return bytes.Equal(a, b)
}
