package wire

import (
	"encoding/binary"
	"fmt"
)

// Reader deserializes an FDB-format message.
//
// The buffer has a FakeRoot wrapper (added by save_members):
//
//	root_offset → FakeRoot object → (RelativeOffset) → message object → fields
//
// NewReader navigates both levels and positions the reader at the message object.
type Reader struct {
	data      []byte // full buffer
	object    []byte // slice starting at the MESSAGE object (not FakeRoot)
	objPos    int    // absolute position of object in data
	vtable    []byte // slice starting at the message's vtable
	headerOff int    // 0 or 8 (protocol version prefix)
	fault     int    // 1 + slot of the first field found outside the buffer (see Err), or 0
}

// NewReader parses the buffer, navigates through the FakeRoot to the message object.
// Handles both formats:
//   - With protocol version prefix: [version(8)][root_offset(4)][file_id(4)][data...]
//   - Without prefix: [root_offset(4)][file_id(4)][data...]
func NewReader(data []byte) (*Reader, error) {
	if len(data) < 8 {
		return nil, fmt.Errorf("wire: buffer too short (%d bytes, need at least 8)", len(data))
	}

	// Detect protocol version prefix.
	// FDB protocol versions are uint64 LE with pattern 0x0FDB00B0_xxxxxxxx.
	// Bytes [0:8] as LE uint64: high byte (data[7]) = 0x0F, data[6] = 0xDB.
	offset := 0
	if len(data) >= 16 && data[7] == 0x0F && data[6] == 0xDB {
		offset = 8
	}

	// Root footer: [root_offset(4)][file_id(4)]
	rootOffset := binary.LittleEndian.Uint32(data[offset : offset+4])
	absRoot := offset + int(rootOffset)
	if absRoot+8 > len(data) {
		return nil, fmt.Errorf("wire: root_offset %d out of bounds (buffer length %d, header offset %d)", rootOffset, len(data), offset)
	}

	// FakeRoot object at data[absRoot].
	frObj := data[absRoot:]
	if len(frObj) < 8 {
		return nil, fmt.Errorf("wire: FakeRoot object too short")
	}

	// FakeRoot field[0] at offset 4: RelativeOffset to the message object.
	// The FakeRoot vtable is always [6, 8, 4], so field 0 is at object offset 4.
	msgRelOff := binary.LittleEndian.Uint32(frObj[4:8])
	msgAbsPos := absRoot + 4 + int(msgRelOff)
	if msgAbsPos+4 > len(data) {
		return nil, fmt.Errorf("wire: message object position %d out of bounds", msgAbsPos)
	}

	// Message object: data[msgAbsPos].
	msgObj := data[msgAbsPos:]
	vtableSoffset := int32(binary.LittleEndian.Uint32(msgObj[0:4]))
	vtableAbsPos := msgAbsPos - int(vtableSoffset)
	if vtableAbsPos < 0 || vtableAbsPos+4 > len(data) {
		return nil, fmt.Errorf("wire: vtable position %d out of bounds", vtableAbsPos)
	}

	return &Reader{
		data:      data,
		object:    msgObj,
		objPos:    msgAbsPos,
		vtable:    data[vtableAbsPos:],
		headerOff: offset,
	}, nil
}

// InitReader initializes a Reader in-place, avoiding heap allocation.
// The caller can stack-allocate: var r wire.Reader; wire.InitReader(data, &r)
// Returns error if the buffer is malformed.
func InitReader(data []byte, r *Reader) error {
	if len(data) < 8 {
		return fmt.Errorf("wire: buffer too short (%d bytes, need at least 8)", len(data))
	}

	offset := 0
	if len(data) >= 16 && data[7] == 0x0F && data[6] == 0xDB {
		offset = 8
	}

	rootOffset := binary.LittleEndian.Uint32(data[offset : offset+4])
	absRoot := offset + int(rootOffset)
	if absRoot+8 > len(data) {
		return fmt.Errorf("wire: root_offset %d out of bounds (buffer length %d)", rootOffset, len(data))
	}

	frObj := data[absRoot:]
	if len(frObj) < 8 {
		return fmt.Errorf("wire: FakeRoot object too short")
	}

	msgRelOff := binary.LittleEndian.Uint32(frObj[4:8])
	msgAbsPos := absRoot + 4 + int(msgRelOff)
	if msgAbsPos+4 > len(data) {
		return fmt.Errorf("wire: message object position %d out of bounds", msgAbsPos)
	}

	msgObj := data[msgAbsPos:]
	vtableSoffset := int32(binary.LittleEndian.Uint32(msgObj[0:4]))
	vtableAbsPos := msgAbsPos - int(vtableSoffset)
	if vtableAbsPos < 0 || vtableAbsPos+4 > len(data) {
		return fmt.Errorf("wire: vtable position %d out of bounds", vtableAbsPos)
	}

	r.data = data
	r.object = msgObj
	r.objPos = msgAbsPos
	r.vtable = data[vtableAbsPos:]
	r.headerOff = offset
	r.fault = 0
	return nil
}

// FileIdentifier reads the file_identifier from the root footer.
func (r *Reader) FileIdentifier() uint32 {
	return binary.LittleEndian.Uint32(r.data[r.headerOff+4 : r.headerOff+8])
}

// VTableLength returns the number of vtable entries (including the 2-entry header).
func (r *Reader) VTableLength() int {
	vtableSize := binary.LittleEndian.Uint16(r.vtable[0:2])
	return int(vtableSize) / 2
}

// fieldOffset returns the byte offset of a field within the object,
// or 0 if the field is not present (vtable entry is 0 or beyond vtable length).
func (r *Reader) fieldOffset(vtableSlot int) int {
	// vtable entries: [0]=vtable_size, [1]=object_size, [2+]=field offsets
	entryIndex := vtableSlot + 2
	vtableLen := r.VTableLength()
	if entryIndex >= vtableLen {
		return 0
	}
	byteOff := entryIndex * 2
	if byteOff+2 > len(r.vtable) {
		// The vtable claims more entries than the buffer holds.
		if r.fault == 0 {
			r.fault = vtableSlot + 1
		}
		return 0
	}
	return int(binary.LittleEndian.Uint16(r.vtable[byteOff:]))
}

// outside records a read of a present field whose bytes lie outside the buffer;
// an absent field (offset below 4) is not one.
func (r *Reader) outside(vtableSlot, off int) {
	if off >= 4 && r.fault == 0 {
		r.fault = vtableSlot + 1
	}
}

// Err reports the first slot a read found outside the buffer (a present field or a
// vtable entry past it); such reads return zero or nil, as for an absent field.
func (r *Reader) Err() error {
	if r.fault == 0 {
		return nil
	}
	return &FieldBoundsError{Slot: r.fault - 1}
}

// FieldPresent returns true if the field at the given vtable slot has a non-zero offset.
func (r *Reader) FieldPresent(vtableSlot int) bool {
	return r.fieldOffset(vtableSlot) >= 4
}

// readerAtObject creates a sub-Reader positioned at a FlatBuffers object within the same buffer.
// Unlike NewReader, this does NOT expect a FakeRoot wrapper or root footer.
// The object starts with a vtable soffset (int32), followed by fields.
func (r *Reader) readerAtObject(objPos int) (*Reader, error) {
	if objPos+4 > len(r.data) {
		return nil, fmt.Errorf("wire: object position %d out of bounds (buf %d)", objPos, len(r.data))
	}
	vtableSoffset := int32(binary.LittleEndian.Uint32(r.data[objPos:]))
	vtableAbsPos := objPos - int(vtableSoffset)
	if vtableAbsPos < 0 || vtableAbsPos+4 > len(r.data) {
		return nil, fmt.Errorf("wire: vtable position %d out of bounds", vtableAbsPos)
	}
	return &Reader{
		data:      r.data,
		object:    r.data[objPos:],
		objPos:    objPos,
		vtable:    r.data[vtableAbsPos:],
		headerOff: r.headerOff,
	}, nil
}

// FDBError is returned when an FDB ErrorOr response contains an error code.
// This is the canonical FDB error type — use errors.As() to extract it from
// wrapped errors. Equivalent to Java's FDBException.
type FDBError struct {
	Code int
}

func (e *FDBError) Error() string {
	if desc, ok := fdbErrorDescriptions[e.Code]; ok {
		return fmt.Sprintf("%s (%d)", desc, e.Code)
	}
	return fmt.Sprintf("fdb error %d", e.Code)
}

// fdbErrorDescriptions maps common FDB error codes to human-readable
// names. Source: flow/error_definitions.h plus Go-side internal codes.
//
// This is a SUBSET of pkg/fdbgo/fdb/error.go's full 343-code map.
// Wire can't import fdb (would create a cycle), so the descriptions
// are duplicated here for the codes the wire layer actually surfaces.
//
// 1006 all_alternatives_failed (canonical FDB) is in the map.
var fdbErrorDescriptions = map[int]string{
	1001: "wrong_shard_server",
	1006: "all_alternatives_failed",
	1007: "transaction_too_old",
	1009: "future_version",
	1020: "not_committed",
	1021: "commit_unknown_result",
	1025: "transaction_cancelled",
	1031: "transaction_timed_out",
	1034: "watches_disabled",
	1037: "process_behind",
	1038: "database_locked",
	1039: "cluster_version_changed",
	1042: "commit_proxy_memory_limit_exceeded",
	1051: "batch_transaction_throttled",
	1062: "change_feed_cancelled",
	1078: "grv_proxy_memory_limit_exceeded",
	1200: "recruitment_failed",
	1213: "tag_throttled",
	1223: "proxy_tag_throttled",
	1235: "transaction_throttled_hot_shard",
	1242: "transaction_rejected_range_locked",
	2000: "client_invalid_operation",
	2004: "key_outside_legal_range",
	2005: "inverted_range",
	2006: "invalid_option_value",
	2012: "range_limits_invalid",
	2015: "future_not_set",
	2017: "used_during_commit",
	2023: "transaction_read_only",
	2101: "transaction_too_large",
	2200: "api_version_unset",
	2201: "api_version_already_set",
	2203: "api_version_not_supported",
}

// (wire.FDBError.Retryable() was removed in RFC-105: dead code — zero callers —
// that carried a fourth, divergent retry-code list. The retry predicates live in
// the client/fdb layers: fdb.IsRetryable (fdb_error_predicate) and
// client.onErrorRetryable (Transaction.onError), each C++-pinned by tests.)

// ErrorOr<T> is serialized as a flow union_like: a root object carrying a uint8
// type tag at vtable slot 0 and a RelativeOffset to the chosen alternative at
// slot 1. The tag is the union index + 1, so 1 = Error, 2 = the value T (0 = the
// empty NONE state). Matches C++ flow.h union_like_traits<ErrorOr<T>> +
// flat_buffers.h save_with_vtables; the Go writer in wire/types/erroror.go emits
// the identical layout. FDB sends every ReplyPromise<T> reply this way.
const (
	errorOrTagSlot   = 0 // uint8 union type tag
	errorOrValueSlot = 1 // RelativeOffset to the chosen alternative (Error or T)
	errorOrTagError  = 1 // Error alternative (union index 0 + 1)
	errorOrTagValue  = 2 // value T alternative (union index 1 + 1)
)

// ReadErrorOr unwraps an ErrorOr<T> response. On the value tag it returns a
// Reader positioned at T; on the error tag it returns the decoded *FDBError.
//
// The success/error decision reads the explicit union tag — it does NOT infer
// from the nested object's field count. A one-field success T (e.g.
// SplitRangeReply, whose only field is SplitPoints) is structurally
// indistinguishable from a one-field Error table, so the old field-count
// heuristic silently misread such successes as errors.
func ReadErrorOr(data []byte) (*Reader, error) {
	r := &Reader{}
	if err := ReadErrorOrInto(data, r); err != nil {
		return nil, err
	}
	return r, nil
}

// ReadErrorOrInto is like ReadErrorOr but writes the success Reader in-place,
// letting the caller stack-allocate it and avoid a heap alloc on the hot path:
//
//	var r wire.Reader
//	if err := wire.ReadErrorOrInto(data, &r); err != nil { ... }
//	if err := reply.UnmarshalFromReader(&r); err != nil { ... }
func ReadErrorOrInto(data []byte, r *Reader) error {
	// Position a Reader at the ErrorOr union root itself — NOT NewReader, which
	// applies the FakeRoot field-0 indirection and would land directly on the
	// alternative, hiding the tag.
	var root Reader
	if err := initReaderAtRootObject(data, &root); err != nil {
		return err
	}
	if !root.FieldPresent(errorOrTagSlot) {
		return fmt.Errorf("wire: ErrorOr response has no union tag")
	}
	switch tag := root.ReadUint8(errorOrTagSlot); tag {
	case errorOrTagError:
		var errObj Reader
		if err := root.readNestedInto(errorOrValueSlot, &errObj); err != nil {
			return fmt.Errorf("wire: ErrorOr error payload: %w", err)
		}
		// Error is a single-field table: uint16 error_code at slot 0.
		code := errObj.ReadUint16(0)
		if err := errObj.Err(); err != nil {
			return &DecodeError{Type: "Error", Err: err}
		}
		return &FDBError{Code: int(code)}
	case errorOrTagValue:
		if err := root.readNestedInto(errorOrValueSlot, r); err != nil {
			return fmt.Errorf("wire: ErrorOr value payload: %w", err)
		}
		return nil
	default:
		return fmt.Errorf("wire: ErrorOr response has unexpected union tag %d", tag)
	}
}

// ReadInlineReplyError decodes the Optional<Error> that LoadBalancedReply-derived
// storage replies carry INLINE (GetValueReply, GetKeyReply, GetKeyValuesReply).
// r must be positioned at the reply object. The optional is two vtable slots:
// a uint8 present-tag at tagSlot and, at tagSlot+1, a RelativeOffset to a nested
// Error table whose uint16 error_code is at the Error table's own slot 0.
// Returns the decoded *FDBError, or nil if the field is absent.
//
// FDB delivers wrong_shard_server / future_version / process_behind for reads
// through this field, not the ErrorOr root (C++ storageserver.actor.cpp
// sendErrorWithPenalty sets reply.error; LoadBalance.actor.h re-throws it). Kept as
// public API; the client reads the generated HasError/Error, which fail a malformed
// error table where this reads it as absent.
func ReadInlineReplyError(r *Reader, tagSlot int) *FDBError {
	// Tag 1 is the Optional's only alternative; any other tag loads as absent in C++.
	if !r.FieldPresent(tagSlot) || r.ReadUint8(tagSlot) != 1 {
		return nil
	}
	var errObj Reader
	if err := r.readNestedInto(tagSlot+1, &errObj); err != nil {
		return nil
	}
	// Error table: uint16 error_code at slot 0.
	code := errObj.ReadUint16(0)
	if errObj.Err() != nil {
		return nil
	}
	return &FDBError{Code: int(code)}
}

// ReaderAtRootObject positions a Reader at the object the footer's root_offset
// points to, WITHOUT NewReader's FakeRoot field-0 indirection. Exposed for tests
// that inspect a union/root object directly (e.g. an ErrorOr root, whose slot 0
// tag + slot 1 nested Error mirror an inline reply error).
func ReaderAtRootObject(data []byte) (*Reader, error) {
	r := &Reader{}
	if err := initReaderAtRootObject(data, r); err != nil {
		return nil, err
	}
	return r, nil
}

// initReaderAtRootObject positions r at the object the footer's root_offset
// points to, WITHOUT NewReader's FakeRoot field-0 indirection. For an ErrorOr<T>
// response that root object is the ErrorOr union itself.
func initReaderAtRootObject(data []byte, r *Reader) error {
	if len(data) < 8 {
		return fmt.Errorf("wire: buffer too short (%d bytes, need at least 8)", len(data))
	}
	offset := 0
	if len(data) >= 16 && data[7] == 0x0F && data[6] == 0xDB {
		offset = 8
	}
	rootOffset := binary.LittleEndian.Uint32(data[offset : offset+4])
	absRoot := offset + int(rootOffset)
	if absRoot+4 > len(data) {
		return fmt.Errorf("wire: root_offset %d out of bounds (buffer length %d)", rootOffset, len(data))
	}
	vtableSoffset := int32(binary.LittleEndian.Uint32(data[absRoot:]))
	vtableAbsPos := absRoot - int(vtableSoffset)
	if vtableAbsPos < 0 || vtableAbsPos+4 > len(data) {
		return fmt.Errorf("wire: vtable position %d out of bounds", vtableAbsPos)
	}
	r.data = data
	r.object = data[absRoot:]
	r.objPos = absRoot
	r.vtable = data[vtableAbsPos:]
	r.headerOff = offset
	r.fault = 0
	return nil
}

// RawData returns the full underlying buffer. Useful for low-level nested parsing.
func (r *Reader) RawData() []byte {
	return r.data
}

// ObjectPos returns the absolute position of this reader's object in the buffer.
func (r *Reader) ObjectPos() int {
	return r.objPos
}

// FieldOffset returns the byte offset of a field within the object (exported version).
// Returns 0 if the field is absent.
func (r *Reader) FieldOffset(vtableSlot int) int {
	return r.fieldOffset(vtableSlot)
}

// ObjectBytes returns the slice of the buffer starting at the object position.
// Fields are at offsets within this slice.
func (r *Reader) ObjectBytes() []byte {
	return r.object
}
