package types

import "testing"

// TestReadOptions_OptionalPrimitiveScalarRoundTrip pins RFC-117: ReadOptions'
// consistencyCheckStartVersion is Optional<Version> (int64), serialized as a BARE
// out-of-line scalar behind the union RelativeOffset (C++ SaveAlternative,
// flat_buffers.h:848) — NOT a length-prefixed []byte. The extractor used to emit it
// as []byte; this round-trips it as a typed int64 through the nested Optional<ReadOptions>
// carried by GetValueRequest, alongside the Optional<UID> debugID (slot 3) — proving the
// two-Optional nested-table layout (slots 3 and 5) is intact.
//
// Revert-prove: revert optionalInnerIsScalar to UID-only + regen → ConsistencyCheckStartVersion
// returns to []byte and this file fails to compile (int64 literal into a []byte field).
func TestReadOptions_OptionalPrimitiveScalarRoundTrip(t *testing.T) {
	t.Parallel()

	const ccsv int64 = 0x0102030405060708 // a Version with all 8 bytes distinct
	debugID := [16]byte{0xDE, 0xAD, 0xBE, 0xEF, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12}

	req := GetValueRequest{
		Key:        []byte("k"),
		Version:    42,
		HasOptions: true,
		Options: ReadOptions{
			Type:                            7,
			CacheResult:                     true,
			HasDebugID:                      true,
			DebugID:                         debugID,
			HasConsistencyCheckStartVersion: true,
			ConsistencyCheckStartVersion:    ccsv,
			LockAware:                       true,
		},
	}

	var got GetValueRequest
	if err := got.UnmarshalFDB(req.MarshalFDB()); err != nil {
		t.Fatalf("UnmarshalFDB: %v", err)
	}
	if !got.HasOptions {
		t.Fatal("HasOptions lost in round-trip")
	}
	o := got.Options
	if !o.HasConsistencyCheckStartVersion || o.ConsistencyCheckStartVersion != ccsv {
		t.Errorf("ConsistencyCheckStartVersion: got (has=%v, %#x), want (true, %#x)",
			o.HasConsistencyCheckStartVersion, o.ConsistencyCheckStartVersion, ccsv)
	}
	// The sibling Optional<UID> must survive unchanged alongside the new Optional<int64>.
	if !o.HasDebugID || o.DebugID != debugID {
		t.Errorf("DebugID: got (has=%v, %x), want (true, %x)", o.HasDebugID, o.DebugID, debugID)
	}
	if !o.LockAware {
		t.Error("LockAware lost")
	}
}

// TestReadOptions_ConsistencyCheckAbsent: when the Has-tag is false the field is not
// serialized and decodes back absent + zero (the union present-tag gate).
func TestReadOptions_ConsistencyCheckAbsent(t *testing.T) {
	t.Parallel()
	req := GetValueRequest{
		Key:        []byte("k"),
		HasOptions: true,
		Options:    ReadOptions{CacheResult: true}, // no Optionals set
	}
	var got GetValueRequest
	if err := got.UnmarshalFDB(req.MarshalFDB()); err != nil {
		t.Fatalf("UnmarshalFDB: %v", err)
	}
	if got.Options.HasConsistencyCheckStartVersion {
		t.Error("ConsistencyCheckStartVersion must be absent when not set")
	}
	if got.Options.ConsistencyCheckStartVersion != 0 {
		t.Errorf("absent value must be zero, got %#x", got.Options.ConsistencyCheckStartVersion)
	}
}

// TestReadOptions_ConsistencyCheckBoundaries: zero, -1 (all 0xFF), and MaxInt64
// round-trip exactly through the LE 8-byte encoding.
func TestReadOptions_ConsistencyCheckBoundaries(t *testing.T) {
	t.Parallel()
	for _, v := range []int64{0, -1, 1, 1<<63 - 1, -(1 << 62)} {
		req := GetValueRequest{
			Key:        []byte("k"),
			HasOptions: true,
			Options:    ReadOptions{HasConsistencyCheckStartVersion: true, ConsistencyCheckStartVersion: v},
		}
		var got GetValueRequest
		if err := got.UnmarshalFDB(req.MarshalFDB()); err != nil {
			t.Fatalf("v=%#x UnmarshalFDB: %v", v, err)
		}
		if got.Options.ConsistencyCheckStartVersion != v {
			t.Errorf("v=%#x: round-trip got %#x", v, got.Options.ConsistencyCheckStartVersion)
		}
	}
}
