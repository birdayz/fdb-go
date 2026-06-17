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

func (f *fuzzData) int32() int32 {
	var v int32
	for i := 0; i < 4; i++ {
		v |= int32(f.byte()) << (i * 8)
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

// uid16 drains 16 bytes into a [16]byte for the Optional<UID> DebugID field.
func (f *fuzzData) uid16() [16]byte {
	var out [16]byte
	for i := range out {
		out[i] = f.byte()
	}
	return out
}

// FuzzWatchValueRequest_RoundTrip pins Marshal→Unmarshal symmetry for
// WatchValueRequest now that the schema extractor emits the symmetric
// UnmarshalFDB/UnmarshalFromReader pair. The critical field is DebugID
// (Optional<UID> — a 16-byte bare scalar behind the union RelativeOffset);
// a slot/offset bug in either direction shows up as a round-trip mismatch.
func FuzzWatchValueRequest_RoundTrip(f *testing.F) {
	f.Add([]byte{4, 1, 2, 3, 4, 1, 2, 0, 0xAA, 0, 0, 0, 0, 0, 0, 0})
	f.Add([]byte{0})

	f.Fuzz(func(t *testing.T, data []byte) {
		fd := &fuzzData{b: data}
		hasValue := fd.bool()
		hasTags := fd.bool()
		hasDebugID := fd.bool()
		req := &WatchValueRequest{
			Key:        fd.bytesN(64),
			HasValue:   hasValue,
			Version:    fd.int64(),
			HasTags:    hasTags,
			HasDebugID: hasDebugID,
			DebugID:    fd.uid16(),
		}
		if hasValue {
			req.Value = fd.bytesN(64)
		}
		if hasTags {
			req.Tags = fd.bytesN(64)
		}

		buf := req.MarshalFDB()
		if _, err := wire.NewReader(buf); err != nil {
			t.Fatalf("MarshalFDB produced unparseable bytes: %v", err)
		}

		var got WatchValueRequest
		if err := got.UnmarshalFDB(buf); err != nil {
			t.Fatalf("UnmarshalFDB: %v", err)
		}
		if !equalBytes(got.Key, req.Key) {
			t.Errorf("Key: got %x want %x", got.Key, req.Key)
		}
		if got.Version != req.Version {
			t.Errorf("Version: got %d want %d", got.Version, req.Version)
		}
		if got.HasValue != req.HasValue {
			t.Errorf("HasValue: got %v want %v", got.HasValue, req.HasValue)
		}
		if req.HasValue && !equalBytes(got.Value, req.Value) {
			t.Errorf("Value: got %x want %x", got.Value, req.Value)
		}
		if got.HasTags != req.HasTags {
			t.Errorf("HasTags: got %v want %v", got.HasTags, req.HasTags)
		}
		if req.HasTags && !equalBytes(got.Tags, req.Tags) {
			t.Errorf("Tags: got %x want %x", got.Tags, req.Tags)
		}
		if got.HasDebugID != req.HasDebugID {
			t.Errorf("HasDebugID: got %v want %v", got.HasDebugID, req.HasDebugID)
		}
		if req.HasDebugID && got.DebugID != req.DebugID {
			t.Errorf("DebugID: got %x want %x", got.DebugID, req.DebugID)
		}
	})
}

// TestWatchValueRequest_RoundTrip is the deterministic counterpart: a fully
// populated WatchValueRequest (non-zero DebugID, SpanContext, TenantInfo) must
// round-trip every field. Pins the new symmetric UnmarshalFDB and the
// Optional<UID> [16]byte DebugID slot specifically.
func TestWatchValueRequest_RoundTrip(t *testing.T) {
	t.Parallel()
	debugID := [16]byte{0xDE, 0xAD, 0xBE, 0xEF, 1, 2, 3, 4, 5, 6, 7, 8, 0xCA, 0xFE, 0xBA, 0xBE}
	req := &WatchValueRequest{
		Key:         []byte("watched-key"),
		Version:     0x0123456789ABCDEF,
		HasDebugID:  true,
		DebugID:     debugID,
		Reply:       ReplyPromise{Token: [16]byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16}},
		SpanContext: SpanContext{TraceID: [16]byte{16, 15, 14, 13, 12, 11, 10, 9, 8, 7, 6, 5, 4, 3, 2, 1}, SpanID: 0xCAFE, Flags: 1},
		TenantInfo:  TenantInfo{TenantId: 42},
	}
	buf := req.MarshalFDB()

	var got WatchValueRequest
	if err := got.UnmarshalFDB(buf); err != nil {
		t.Fatalf("UnmarshalFDB: %v", err)
	}
	if !bytes.Equal(got.Key, req.Key) {
		t.Errorf("Key: got %q want %q", got.Key, req.Key)
	}
	if got.Version != req.Version {
		t.Errorf("Version: got %#x want %#x", got.Version, req.Version)
	}
	if !got.HasDebugID {
		t.Error("HasDebugID: got false, want true")
	}
	if got.DebugID != debugID {
		t.Errorf("DebugID: got %x want %x", got.DebugID, debugID)
	}
	if got.Reply.Token != req.Reply.Token {
		t.Errorf("Reply.Token: got %x want %x", got.Reply.Token, req.Reply.Token)
	}
	if got.SpanContext.TraceID != req.SpanContext.TraceID ||
		got.SpanContext.SpanID != req.SpanContext.SpanID ||
		got.SpanContext.Flags != req.SpanContext.Flags {
		t.Errorf("SpanContext: got %+v want %+v", got.SpanContext, req.SpanContext)
	}
	if got.TenantInfo.TenantId != req.TenantInfo.TenantId {
		t.Errorf("TenantInfo.TenantId: got %d want %d", got.TenantInfo.TenantId, req.TenantInfo.TenantId)
	}
}

// TestWatchValueReply_RoundTrip pins the WatchValueReply symmetric methods.
func TestWatchValueReply_RoundTrip(t *testing.T) {
	t.Parallel()
	reply := &WatchValueReply{Version: 0x7FFFFFFFFFFFFFFF, Cached: true}
	buf := reply.MarshalFDB()

	var got WatchValueReply
	if err := got.UnmarshalFDB(buf); err != nil {
		t.Fatalf("UnmarshalFDB: %v", err)
	}
	if got.Version != reply.Version {
		t.Errorf("Version: got %#x want %#x", got.Version, reply.Version)
	}
	if got.Cached != reply.Cached {
		t.Errorf("Cached: got %v want %v", got.Cached, reply.Cached)
	}
}

func equalBytes(a, b []byte) bool {
	if len(a) == 0 && len(b) == 0 {
		return true
	}
	return bytes.Equal(a, b)
}

// FuzzKeyRangeRef_SingleKeyOptimization specifically forces the
// (begin, begin+\\x00) shape that triggers the optimized writer branch
// AND the corresponding reader-side reconstruction. Without the custom
// override on the reader (keyrangeref_custom.go), this case would have
// round-tripped as (begin, end) swapped — that is the regression we are
// pinning. Wrapped in SplitRangeRequest because KeyRangeRef has no
// public MarshalFDB; the existing TestKeyRangeRef_RoundTrip uses the
// same wrapper for the same reason.
func FuzzKeyRangeRef_SingleKeyOptimization(f *testing.F) {
	f.Add([]byte{0x42})
	f.Add([]byte{1, 2, 3, 4})
	f.Add([]byte{0xFF, 0xFF, 0xFF})

	f.Fuzz(func(t *testing.T, begin []byte) {
		if len(begin) > 1024 {
			begin = begin[:1024]
		}
		end := append(append([]byte{}, begin...), 0)
		// Sanity: this input MUST trigger the optimization. If our helper
		// disagrees, the test is no longer pinning the right path and we
		// should hear about it.
		if !keyRangeEqualsKeyAfter(begin, end) {
			t.Fatalf("setup: begin=%x end=%x not recognized as single-key range", begin, end)
		}

		req := &SplitRangeRequest{Keys: KeyRangeRef{Begin: begin, End: end}, ChunkSize: 1}
		bs := req.MarshalFDB()
		var got SplitRangeRequest
		if err := got.UnmarshalFDB(bs); err != nil {
			t.Fatalf("UnmarshalFDB: %v", err)
		}
		if !equalBytes(got.Keys.Begin, begin) {
			t.Errorf("Begin: got %x, want %x", got.Keys.Begin, begin)
		}
		if !equalBytes(got.Keys.End, end) {
			t.Errorf("End: got %x, want %x", got.Keys.End, end)
		}
	})
}

// FuzzGetKeyRequest_RoundTrip pins KeySelectorRef + Version round-trip
// through the smallest wrapper that exposes both. KeySelectorRef does
// not have a top-level MarshalFDB; the read path also does not exercise
// the wrapper. GetKey is on the read hot path so the round-trip matters.
func FuzzGetKeyRequest_RoundTrip(f *testing.F) {
	f.Add([]byte{3, 1, 2, 3, 1, 0xFF, 0xFF, 0xFF, 0x7F, 8, 0, 0, 0, 0, 0, 0, 0})
	f.Add([]byte{0, 0, 0, 0, 0, 0})

	f.Fuzz(func(t *testing.T, data []byte) {
		fd := &fuzzData{b: data}
		req := &GetKeyRequest{
			Sel: KeySelectorRef{
				Key:     fd.bytesN(64),
				OrEqual: fd.bool(),
				Offset:  fd.int32(),
			},
			Version:    fd.int64(),
			TenantInfo: TenantInfo{TenantId: fd.int64()},
		}
		bs := req.MarshalFDB()

		var got GetKeyRequest
		if err := got.UnmarshalFDB(bs); err != nil {
			t.Fatalf("UnmarshalFDB: %v", err)
		}
		if !equalBytes(got.Sel.Key, req.Sel.Key) {
			t.Errorf("Sel.Key: got %x, want %x", got.Sel.Key, req.Sel.Key)
		}
		if got.Sel.OrEqual != req.Sel.OrEqual {
			t.Errorf("Sel.OrEqual: got %v, want %v", got.Sel.OrEqual, req.Sel.OrEqual)
		}
		if got.Sel.Offset != req.Sel.Offset {
			t.Errorf("Sel.Offset: got %d, want %d", got.Sel.Offset, req.Sel.Offset)
		}
		if got.Version != req.Version {
			t.Errorf("Version: got %d, want %d", got.Version, req.Version)
		}
		if got.TenantInfo.TenantId != req.TenantInfo.TenantId {
			t.Errorf("TenantId: got %d, want %d", got.TenantInfo.TenantId, req.TenantInfo.TenantId)
		}
	})
}

// FuzzGetKeyServerLocationsReply_RoundTrip — the diff-oracle skips this
// type because constructing the deeply-nested vector<pair<KeyRangeRef,
// vector<StorageServerInterface>>> shape on the C++ side is impractical.
// The Go side stores all four wire-serialized fields as opaque []byte blobs
// (the parent reply layer; the inner vectors are decoded later via the
// location-cache helpers). Round-tripping the outer wrapper is still
// valuable: catches vtable/slot-index typos and padding bugs without
// needing the inner payload to be valid.
//
// NOTE on the Arena field: in FDB's C++ wire protocol, Arena is a
// memory-ownership marker carried alongside the deserialized message —
// it is NOT serialized into outbound bytes (the generated MarshalFDB
// correctly omits it from precomputeSize and writeToBuffer). After a
// round-trip, Arena is always empty regardless of input. This fuzzer
// reflects that contract by leaving Arena unset on the input side.
func FuzzGetKeyServerLocationsReply_RoundTrip(f *testing.F) {
	f.Add([]byte{4, 1, 2, 3, 4, 0, 0, 4, 5, 6, 7, 8})
	f.Add([]byte{0, 0, 0, 0})

	f.Fuzz(func(t *testing.T, data []byte) {
		fd := &fuzzData{b: data}
		reply := &GetKeyServerLocationsReply{
			Results:           fd.bytesN(64),
			ResultsTssMapping: fd.bytesN(64),
			ResultsTagMapping: fd.bytesN(64),
		}
		bs := reply.MarshalFDB()

		var got GetKeyServerLocationsReply
		if err := got.UnmarshalFDB(bs); err != nil {
			t.Fatalf("UnmarshalFDB: %v", err)
		}
		if !equalBytes(got.Results, reply.Results) {
			t.Errorf("Results: got %x, want %x", got.Results, reply.Results)
		}
		if !equalBytes(got.ResultsTssMapping, reply.ResultsTssMapping) {
			t.Errorf("ResultsTssMapping: got %x, want %x", got.ResultsTssMapping, reply.ResultsTssMapping)
		}
		if !equalBytes(got.ResultsTagMapping, reply.ResultsTagMapping) {
			t.Errorf("ResultsTagMapping: got %x, want %x", got.ResultsTagMapping, reply.ResultsTagMapping)
		}
		if len(got.Arena) != 0 {
			// Wire contract: Arena must always come back empty regardless of
			// what the Go struct held going in. This pin catches a future
			// generator change that would (incorrectly) emit Arena into the
			// outbound bytes.
			t.Errorf("Arena should be empty after round-trip, got %x", got.Arena)
		}
	})
}
