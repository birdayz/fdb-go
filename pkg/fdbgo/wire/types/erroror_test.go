package types

import (
	"encoding/binary"
	"errors"
	"strings"
	"testing"

	"fdb.dev/pkg/fdbgo/wire"
)

// footerRootObject returns the absolute position of the object the footer's
// root_offset points to — for an ErrorOr<T> buffer that is the ErrorOr union
// root itself. Mirrors wire.initReaderAtRootObject (unexported), so the tests
// can poke the union tag without importing wire internals.
func footerRootObject(data []byte) int {
	offset := 0
	if len(data) >= 16 && data[7] == 0x0F && data[6] == 0xDB {
		offset = 8
	}
	rootOffset := binary.LittleEndian.Uint32(data[offset : offset+4])
	return offset + int(rootOffset)
}

// ErrorOr layout (vtable {8,9,8,4}): tag uint8 at object offset 8, value
// RelativeOffset at object offset 4.
const (
	errorOrTagByteOffset   = 8
	errorOrValueRelOffByte = 4
)

// TestReadErrorOr_ErrorTag pins that an ErrorOr<Error> (tag=1) decodes to the
// embedded FDB error code, read as a uint16.
func TestReadErrorOr_ErrorTag(t *testing.T) {
	t.Parallel()
	for _, code := range []uint16{1, 1009, 1234, 0xFFFF} {
		buf := (&ErrorOrError{ErrorCode: code}).MarshalFDB()
		_, err := wire.ReadErrorOr(buf)
		var fdbErr *wire.FDBError
		if !errors.As(err, &fdbErr) {
			t.Fatalf("code %d: ReadErrorOr returned %v, want *wire.FDBError", code, err)
		}
		if fdbErr.Code != int(code) {
			t.Errorf("code %d: decoded %d", code, fdbErr.Code)
		}
	}
}

// TestReadErrorOr_OneFieldSuccess is the regression test for the silent
// data-loss bug (RFC-010 #8). An ErrorOr<Error> buffer's nested Error is a
// one-field table; flipping ONLY the union tag from 1 (Error) to 2 (value)
// turns it into an ErrorOr<T> success whose value T has exactly one field —
// structurally identical to SplitRangeReply. The old field-count heuristic
// (nfields<=1 && field0 present ⇒ error) misread such a success as an error;
// the tag-based decode must treat it as success.
func TestReadErrorOr_OneFieldSuccess(t *testing.T) {
	t.Parallel()
	buf := (&ErrorOrError{ErrorCode: 1234}).MarshalFDB()

	tagPos := footerRootObject(buf) + errorOrTagByteOffset
	if buf[tagPos] != 1 {
		t.Fatalf("expected Error tag (1) at offset %d, got %d", tagPos, buf[tagPos])
	}

	// Sanity: as an error, it decodes to 1234 (and is misclassified by field
	// count too — both an Error and a 1-field success have one field).
	if _, err := wire.ReadErrorOr(buf); err == nil {
		t.Fatal("tag=1 should decode as an error")
	}

	// Flip tag 1 (Error) -> 2 (value). Now it is a one-field SUCCESS.
	buf[tagPos] = 2
	r, err := wire.ReadErrorOr(buf)
	if err != nil {
		t.Fatalf("one-field success misclassified as error: %v", err)
	}
	if r == nil {
		t.Fatal("expected a Reader positioned at the success value")
	}
}

// TestReadErrorOr_ZeroFieldSuccess pins that a VoidReply (ErrorOr<EnsureTable<Void>>,
// tag=2, nested has zero fields) decodes as success — not "empty ErrorOr response".
func TestReadErrorOr_ZeroFieldSuccess(t *testing.T) {
	t.Parallel()
	buf := (&VoidReply{}).MarshalFDB()
	if _, err := wire.ReadErrorOr(buf); err != nil {
		t.Fatalf("VoidReply success decoded as error: %v", err)
	}
}

// TestReadInlineReplyError decodes the inline LoadBalancedReply.error
// (Optional<Error>) that storage read replies carry (RFC-010 #1). An ErrorOrError
// buffer's root object is byte-identical to that inline field: a uint8 present-tag
// at slot 0 and a RelativeOffset to a nested Error table at slot 1. The real
// replies put the same structure at slot 1/2 (the generated SlotError constant);
// this exercises the nested-Error-table decode the generated reply.Error mis-handles.
func TestReadInlineReplyError(t *testing.T) {
	t.Parallel()
	for _, code := range []uint16{1001, 1009, 1037, 0xFFFF} {
		buf := (&ErrorOrError{ErrorCode: code}).MarshalFDB()
		r, err := wire.ReaderAtRootObject(buf)
		if err != nil {
			t.Fatalf("code %d: ReaderAtRootObject: %v", code, err)
		}
		ferr := wire.ReadInlineReplyError(r, 0) // ErrorOr root: tag@0, nested Error@1
		if ferr == nil {
			t.Fatalf("code %d: expected inline error, got nil", code)
		}
		if ferr.Code != int(code) {
			t.Errorf("code %d: decoded %d", code, ferr.Code)
		}
	}
}

// TestMarshalErrorOrInlineError_RoundTrip validates the fault-injection envelope
// (RFC-118): MarshalErrorOrInlineError must produce a frame that the production
// read-reply decode path (ReadErrorOrInto → UnmarshalFromReader →
// ReadInlineReplyError) reads as a SUCCESS ErrorOr whose nested reply carries the
// inline error. It must decode identically through all three read reply types'
// SlotError (the single frame serves all three parsers). Penalty must survive too.
func TestMarshalErrorOrInlineError_RoundTrip(t *testing.T) {
	t.Parallel()
	for _, code := range []uint16{1001, 1009, 1037} {
		buf := MarshalErrorOrInlineError(code, 1.5)

		// The ErrorOr root must be a SUCCESS union (tag=value), NOT the error tag —
		// the error rides the INLINE field, exactly as the storage server sends it.
		r, err := wire.ReadErrorOr(buf)
		if err != nil {
			t.Fatalf("code %d: ReadErrorOr (expected success tag) returned error: %v", code, err)
		}
		// Penalty (slot 0) round-trips.
		var reply GetValueReply
		reply.UnmarshalFromReader(r)
		if reply.Penalty != 1.5 {
			t.Errorf("code %d: Penalty = %v, want 1.5", code, reply.Penalty)
		}
		// The inline error decodes at every read reply's SlotError (all == 1).
		for name, slot := range map[string]int{
			"GetValueReply":     GetValueReplySlotError,
			"GetKeyReply":       GetKeyReplySlotError,
			"GetKeyValuesReply": GetKeyValuesReplySlotError,
		} {
			r2, err := wire.ReadErrorOr(buf)
			if err != nil {
				t.Fatalf("code %d/%s: ReadErrorOr: %v", code, name, err)
			}
			ferr := wire.ReadInlineReplyError(r2, slot)
			if ferr == nil {
				t.Fatalf("code %d/%s: expected inline error at slot %d, got nil", code, name, slot)
			}
			if ferr.Code != int(code) {
				t.Errorf("code %d/%s: decoded inline code %d", code, name, ferr.Code)
			}
		}
	}
}

// TestReadInlineReplyError_Absent: a reply with no inline error returns nil (the
// common success case must not be misread as an error). VoidReply success lands
// on EnsureTable<Void>, which has no error field at the queried slot.
func TestReadInlineReplyError_Absent(t *testing.T) {
	t.Parallel()
	buf := (&VoidReply{}).MarshalFDB()
	r, err := wire.ReadErrorOr(buf) // success → nested EnsureTable<Void>
	if err != nil {
		t.Fatalf("VoidReply ReadErrorOr: %v", err)
	}
	if ferr := wire.ReadInlineReplyError(r, 1); ferr != nil {
		t.Errorf("absent inline error: expected nil, got %v", ferr)
	}
}

// TestReadErrorOr_ErrorCodeIsUint16 proves the error code is read as a 2-byte
// uint16, not a 4-byte int32 that would over-read into adjacent bytes. We set
// the two bytes immediately following the uint16 ErrorCode to non-zero and
// assert the decoded code is unchanged. (A 4-byte read would fold those bytes
// into the code.)
func TestReadErrorOr_ErrorCodeIsUint16(t *testing.T) {
	t.Parallel()
	buf := (&ErrorOrError{ErrorCode: 1009}).MarshalFDB()

	root := footerRootObject(buf)
	// Nested Error object: follow the value RelativeOffset at the root.
	valOff := root + errorOrValueRelOffByte
	errObj := valOff + int(binary.LittleEndian.Uint32(buf[valOff:]))
	// Error vtable {6,6,4}: ErrorCode uint16 at object offset 4; the two bytes
	// at offsets 6..7 are the int32-over-read region.
	hi := errObj + 6
	// The two bytes after the uint16 ErrorCode are this object's zero padding;
	// the layout is fixed by ErrorOrError.MarshalFDB, so these guards can never
	// legitimately fire — a failure means the layout changed and the over-read
	// guarantee must be re-examined (NOT skipped).
	if hi+2 > len(buf) {
		t.Fatalf("buffer too small to exercise over-read (len %d, hi %d) — layout changed", len(buf), hi)
	}
	if buf[hi] != 0 || buf[hi+1] != 0 {
		t.Fatalf("bytes after ErrorCode expected to be zero padding, got (%#x,%#x) — layout changed", buf[hi], buf[hi+1])
	}
	buf[hi], buf[hi+1] = 0xAB, 0xCD

	_, err := wire.ReadErrorOr(buf)
	var fdbErr *wire.FDBError
	if !errors.As(err, &fdbErr) {
		t.Fatalf("ReadErrorOr returned %v, want *wire.FDBError", err)
	}
	if fdbErr.Code != 1009 {
		t.Errorf("over-read: decoded %d, want 1009 (read 4 bytes instead of 2?)", fdbErr.Code)
	}
}

// TestReadErrorOr_NoneTag pins that an ErrorOr with union tag 0 (NONE / empty)
// is rejected, not silently treated as success. Edge case for #8.
func TestReadErrorOr_NoneTag(t *testing.T) {
	t.Parallel()
	buf := (&ErrorOrError{ErrorCode: 1234}).MarshalFDB()
	buf[footerRootObject(buf)+errorOrTagByteOffset] = 0 // tag 1 (Error) -> 0 (NONE)
	if _, err := wire.ReadErrorOr(buf); err == nil {
		t.Fatal("ErrorOr with NONE tag should be rejected, got success")
	}
}

// TestReadInlineReplyError_Malformed pins the deliberate safe false-negative: a
// present error tag whose nested-Error RelativeOffset is unreadable (here zeroed)
// yields nil rather than a bogus FDBError or a panic. Edge case for #1.
func TestReadInlineReplyError_Malformed(t *testing.T) {
	t.Parallel()
	buf := (&ErrorOrError{ErrorCode: 1234}).MarshalFDB()
	root := footerRootObject(buf)
	// Tag stays present (1) at root+8; zero the value RelativeOffset at root+4.
	for i := root + errorOrValueRelOffByte; i < root+errorOrValueRelOffByte+4; i++ {
		buf[i] = 0
	}
	r, err := wire.ReaderAtRootObject(buf)
	if err != nil {
		t.Fatalf("ReaderAtRootObject: %v", err)
	}
	if ferr := wire.ReadInlineReplyError(r, 0); ferr != nil {
		t.Errorf("malformed inline error: want nil (safe false-negative), got %v", ferr)
	}
}

// TestLoadBalancedReplyErrorSlots pins the wire-layout assumption #1 depends on:
// every LoadBalancedReply-derived read reply carries its Optional<Error>
// present-tag at slot 1 (so the nested Error table is at slot 2), and the Error
// table's uint16 code is at slot 0. A schema-extractor regen that shifted these
// would silently break inline wrong-shard decode — this catches it.
func TestLoadBalancedReplyErrorSlots(t *testing.T) {
	t.Parallel()
	if GetValueReplySlotError != 1 || GetKeyReplySlotError != 1 || GetKeyValuesReplySlotError != 1 {
		t.Errorf("LoadBalancedReply error tag slot drifted: GetValue=%d GetKey=%d GetKeyValues=%d, want all 1",
			GetValueReplySlotError, GetKeyReplySlotError, GetKeyValuesReplySlotError)
	}
	if ErrorSlotErrorCode != 0 {
		t.Errorf("Error.ErrorCode slot = %d, want 0", ErrorSlotErrorCode)
	}
}

// TestReadErrorOr_MalformedPayload pins the malformed-payload arms of ReadErrorOr
// (#8): for both the Error (tag=1) and value (tag=2) alternatives,
// a corrupt/zeroed value RelativeOffset yields a deterministic, tag-specific
// error — never a panic or a false success.
func TestReadErrorOr_MalformedPayload(t *testing.T) {
	t.Parallel()
	for _, tag := range []byte{1, 2} { // 1=Error, 2=value
		buf := (&ErrorOrError{ErrorCode: 1234}).MarshalFDB()
		root := footerRootObject(buf)
		buf[root+errorOrTagByteOffset] = tag
		// Zero the value RelativeOffset (slot 1, object offset 4) so the nested
		// navigation fails.
		for i := root + errorOrValueRelOffByte; i < root+errorOrValueRelOffByte+4; i++ {
			buf[i] = 0
		}
		_, err := wire.ReadErrorOr(buf)
		if err == nil {
			t.Fatalf("tag=%d: corrupt payload must error, got success", tag)
		}
		want := "error payload"
		if tag == 2 {
			want = "value payload"
		}
		if !strings.Contains(err.Error(), want) {
			t.Errorf("tag=%d: error %q, want substring %q", tag, err.Error(), want)
		}
	}
}
