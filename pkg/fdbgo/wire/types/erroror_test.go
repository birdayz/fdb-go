package types

import (
	"encoding/binary"
	"errors"
	"testing"

	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/wire"
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
	if hi+2 > len(buf) {
		t.Skipf("buffer too small to exercise over-read (len %d, hi %d)", len(buf), hi)
	}
	if buf[hi] != 0 || buf[hi+1] != 0 {
		t.Skipf("bytes after ErrorCode not zero padding (%d,%d) — layout changed", buf[hi], buf[hi+1])
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
