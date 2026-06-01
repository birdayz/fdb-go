package wire

import (
	"encoding/binary"
	"fmt"
	"math"
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
		return 0 // corrupted vtable: declared size > actual data
	}
	return int(binary.LittleEndian.Uint16(r.vtable[byteOff:]))
}

// FieldPresent returns true if the field at the given vtable slot has a non-zero offset.
func (r *Reader) FieldPresent(vtableSlot int) bool {
	return r.fieldOffset(vtableSlot) >= 4
}

// ReadInt8 reads an int8 from the given vtable slot.
func (r *Reader) ReadInt8(vtableSlot int) int8 {
	off := r.fieldOffset(vtableSlot)
	if off < 4 || off+1 > len(r.object) {
		return 0
	}
	return int8(r.object[off])
}

// ReadUint8 reads a uint8 from the given vtable slot.
func (r *Reader) ReadUint8(vtableSlot int) uint8 {
	off := r.fieldOffset(vtableSlot)
	if off < 4 || off+1 > len(r.object) {
		return 0
	}
	return r.object[off]
}

// ReadInt16 reads an int16 from the given vtable slot.
func (r *Reader) ReadInt16(vtableSlot int) int16 {
	off := r.fieldOffset(vtableSlot)
	if off < 4 || off+2 > len(r.object) {
		return 0
	}
	return int16(binary.LittleEndian.Uint16(r.object[off:]))
}

// ReadUint16 reads a uint16 from the given vtable slot.
func (r *Reader) ReadUint16(vtableSlot int) uint16 {
	off := r.fieldOffset(vtableSlot)
	if off < 4 || off+2 > len(r.object) {
		return 0
	}
	return binary.LittleEndian.Uint16(r.object[off:])
}

// ReadInt32 reads an int32 from the given vtable slot.
func (r *Reader) ReadInt32(vtableSlot int) int32 {
	off := r.fieldOffset(vtableSlot)
	if off < 4 || off+4 > len(r.object) {
		return 0
	}
	return int32(binary.LittleEndian.Uint32(r.object[off:]))
}

// ReadUint32 reads a uint32 from the given vtable slot.
func (r *Reader) ReadUint32(vtableSlot int) uint32 {
	off := r.fieldOffset(vtableSlot)
	if off < 4 || off+4 > len(r.object) {
		return 0
	}
	return binary.LittleEndian.Uint32(r.object[off:])
}

// ReadInt64 reads an int64 from the given vtable slot.
func (r *Reader) ReadInt64(vtableSlot int) int64 {
	off := r.fieldOffset(vtableSlot)
	if off < 4 || off+8 > len(r.object) {
		return 0
	}
	return int64(binary.LittleEndian.Uint64(r.object[off:]))
}

// ReadUint64 reads a uint64 from the given vtable slot.
func (r *Reader) ReadUint64(vtableSlot int) uint64 {
	off := r.fieldOffset(vtableSlot)
	if off < 4 || off+8 > len(r.object) {
		return 0
	}
	return binary.LittleEndian.Uint64(r.object[off:])
}

// ReadFloat64 reads a float64 from the given vtable slot (LE IEEE754).
func (r *Reader) ReadFloat64(vtableSlot int) float64 {
	off := r.fieldOffset(vtableSlot)
	if off < 4 || off+8 > len(r.object) {
		return 0
	}
	return math.Float64frombits(binary.LittleEndian.Uint64(r.object[off:]))
}

// ReadBool reads a bool from the given vtable slot.
func (r *Reader) ReadBool(vtableSlot int) bool {
	off := r.fieldOffset(vtableSlot)
	if off < 4 || off+1 > len(r.object) {
		return false
	}
	return r.object[off] != 0
}

// ReadUID reads a 16-byte UID from the given vtable slot (inline).
func (r *Reader) ReadUID(vtableSlot int) [16]byte {
	off := r.fieldOffset(vtableSlot)
	if off < 4 || off+16 > len(r.object) {
		return [16]byte{}
	}
	var uid [16]byte
	copy(uid[:], r.object[off:off+16])
	return uid
}

// ReadBytes reads a length-prefixed byte slice from out-of-line data.
// The vtable slot contains a RelativeOffset pointing to [uint32 length][data...].
func (r *Reader) ReadBytes(vtableSlot int) []byte {
	off := r.fieldOffset(vtableSlot)
	if off < 4 || int(off)+4 > len(r.object) {
		return nil
	}
	// Read RelativeOffset at object[off:off+4].
	relOffset := binary.LittleEndian.Uint32(r.object[off:])
	if relOffset == 0 {
		return nil
	}
	// RelativeOffset resolves against the full buffer: target = objPos + off + relOffset.
	target := r.objPos + int(off) + int(relOffset)
	if target+4 > len(r.data) {
		return nil
	}
	length := binary.LittleEndian.Uint32(r.data[target:])
	dataStart := target + 4
	if dataStart+int(length) > len(r.data) {
		return nil
	}
	return r.data[dataStart : dataStart+int(length)]
}

// ReadString reads a length-prefixed string from out-of-line data.
func (r *Reader) ReadString(vtableSlot int) string {
	b := r.ReadBytes(vtableSlot)
	if b == nil {
		return ""
	}
	return string(b)
}

// ReadVectorInt32 reads a vector of int32 from out-of-line data.
// Wire format: RelativeOffset → [uint32 count][int32 elem0][int32 elem1]...
func (r *Reader) ReadVectorInt32(vtableSlot int) []int32 {
	off := r.fieldOffset(vtableSlot)
	if off < 4 || int(off)+4 > len(r.object) {
		return nil
	}
	relOffset := binary.LittleEndian.Uint32(r.object[off:])
	if relOffset == 0 {
		return nil
	}
	target := r.objPos + int(off) + int(relOffset)
	if target+4 > len(r.data) {
		return nil
	}
	count := binary.LittleEndian.Uint32(r.data[target:])
	dataStart := target + 4
	if dataStart+int(count)*4 > len(r.data) {
		return nil
	}
	result := make([]int32, count)
	for i := uint32(0); i < count; i++ {
		result[i] = int32(binary.LittleEndian.Uint32(r.data[dataStart+int(i)*4:]))
	}
	return result
}

// ReadVectorUint64 reads a vector of uint64 from out-of-line data.
func (r *Reader) ReadVectorUint64(vtableSlot int) []uint64 {
	off := r.fieldOffset(vtableSlot)
	if off < 4 || int(off)+4 > len(r.object) {
		return nil
	}
	relOffset := binary.LittleEndian.Uint32(r.object[off:])
	if relOffset == 0 {
		return nil
	}
	target := r.objPos + int(off) + int(relOffset)
	if target+4 > len(r.data) {
		return nil
	}
	count := binary.LittleEndian.Uint32(r.data[target:])
	dataStart := target + 4
	if dataStart+int(count)*8 > len(r.data) {
		return nil
	}
	result := make([]uint64, count)
	for i := uint32(0); i < count; i++ {
		result[i] = binary.LittleEndian.Uint64(r.data[dataStart+int(i)*8:])
	}
	return result
}

// ReadOptionalInt32 reads an Optional<int32>. Returns (value, present).
// Optional uses 2 vtable slots: typeSlot (uint8 tag) and valueSlot (RelativeOffset).
func (r *Reader) ReadOptionalInt32(typeSlot, valueSlot int) (int32, bool) {
	typeOff := r.fieldOffset(typeSlot)
	if typeOff < 4 || typeOff+1 > len(r.object) || r.object[typeOff] == 0 {
		return 0, false
	}
	valOff := r.fieldOffset(valueSlot)
	if valOff < 4 || int(valOff)+4 > len(r.object) {
		return 0, false
	}
	relOffset := binary.LittleEndian.Uint32(r.object[valOff:])
	if relOffset == 0 {
		return 0, false
	}
	target := r.objPos + int(valOff) + int(relOffset)
	if target+4 > len(r.data) {
		return 0, false
	}
	return int32(binary.LittleEndian.Uint32(r.data[target:])), true
}

// ReadOptionalString reads an Optional<string>. Returns (value, present).
func (r *Reader) ReadOptionalString(typeSlot, valueSlot int) (string, bool) {
	typeOff := r.fieldOffset(typeSlot)
	if typeOff < 4 || typeOff+1 > len(r.object) || r.object[typeOff] == 0 {
		return "", false
	}
	valOff := r.fieldOffset(valueSlot)
	if valOff < 4 || int(valOff)+4 > len(r.object) {
		return "", false
	}
	relOffset := binary.LittleEndian.Uint32(r.object[valOff:])
	if relOffset == 0 {
		return "", false
	}
	target := r.objPos + int(valOff) + int(relOffset)
	if target+4 > len(r.data) {
		return "", false
	}
	length := binary.LittleEndian.Uint32(r.data[target:])
	end := target + 4 + int(length)
	if end > len(r.data) {
		return "", false
	}
	return string(r.data[target+4 : end]), true
}

// --- Nested struct and vector-of-struct readers ---
//
// FDB's FlatBuffers uses RelativeOffsets for nested objects:
//   - Nested struct field: vtable slot → 4-byte RelativeOffset → FlatBuffers object (vtable soffset + fields)
//   - Vector of structs: vtable slot → RelativeOffset → [count(4)][RelativeOffset(4) × count]
//     where each element's RelativeOffset points to a FlatBuffers object.

// ReadNestedReader returns a sub-Reader positioned at a nested struct object.
// The vtable slot must contain a RelativeOffset pointing to the struct.
func (r *Reader) ReadNestedReader(vtableSlot int) (*Reader, error) {
	off := r.fieldOffset(vtableSlot)
	if off < 4 || int(off)+4 > len(r.object) {
		return nil, fmt.Errorf("wire: nested struct field not present (slot %d)", vtableSlot)
	}
	relOffset := binary.LittleEndian.Uint32(r.object[off:])
	if relOffset == 0 {
		return nil, fmt.Errorf("wire: nested struct RelativeOffset is 0 (slot %d)", vtableSlot)
	}
	targetPos := r.objPos + int(off) + int(relOffset)
	return r.readerAtObject(targetPos)
}

// ReadVectorCount returns the number of elements in a vector-of-struct field.
func (r *Reader) ReadVectorCount(vtableSlot int) (int, error) {
	off := r.fieldOffset(vtableSlot)
	if off < 4 || int(off)+4 > len(r.object) {
		return 0, nil // absent field = empty vector
	}
	relOffset := binary.LittleEndian.Uint32(r.object[off:])
	if relOffset == 0 {
		return 0, nil
	}
	vecStart := r.objPos + int(off) + int(relOffset)
	if vecStart+4 > len(r.data) {
		return 0, fmt.Errorf("wire: vector start out of bounds (pos %d, buf %d)", vecStart, len(r.data))
	}
	count := binary.LittleEndian.Uint32(r.data[vecStart:])
	// Cap count to prevent OOM from crafted wire data.
	// Each vector element needs at least 4 bytes (RelativeOffset).
	maxCount := (len(r.data) - vecStart - 4) / 4
	if maxCount < 0 {
		maxCount = 0
	}
	if int(count) > maxCount {
		count = uint32(maxCount)
	}
	return int(count), nil
}

// ReadVectorElementReader returns a sub-Reader for the i-th element of a vector-of-struct.
// Elements are 4-byte RelativeOffsets at vector_start+4+i*4, each pointing to a FlatBuffers object.
func (r *Reader) ReadVectorElementReader(vtableSlot, index int) (*Reader, error) {
	off := r.fieldOffset(vtableSlot)
	if off < 4 || int(off)+4 > len(r.object) {
		return nil, fmt.Errorf("wire: vector field not present (slot %d)", vtableSlot)
	}
	relOffset := binary.LittleEndian.Uint32(r.object[off:])
	if relOffset == 0 {
		return nil, fmt.Errorf("wire: vector RelativeOffset is 0 (slot %d)", vtableSlot)
	}
	vecStart := r.objPos + int(off) + int(relOffset)
	if vecStart+4 > len(r.data) {
		return nil, fmt.Errorf("wire: vector start out of bounds")
	}
	count := int(binary.LittleEndian.Uint32(r.data[vecStart:]))
	if index < 0 || index >= count {
		return nil, fmt.Errorf("wire: vector index %d out of range [0, %d)", index, count)
	}
	// Each element is a 4-byte RelativeOffset.
	elemRelOffPos := vecStart + 4 + index*4
	if elemRelOffPos+4 > len(r.data) {
		return nil, fmt.Errorf("wire: vector element offset out of bounds")
	}
	elemRelOff := binary.LittleEndian.Uint32(r.data[elemRelOffPos:])
	elemObjPos := elemRelOffPos + int(elemRelOff)
	return r.readerAtObject(elemObjPos)
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
// Two non-canonical entries:
//   - 1006 all_alternatives_failed (canonical FDB) is in the map.
//   - 1200 is intentionally remapped to "all_proxies_unreachable"
//     (Go-internal — the Go client uses code 1200 as
//     ErrAllProxiesUnreachable, distinct from C++'s 1200=recruitment_failed
//     which never surfaces over the wire to a Go client).
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
	// 1200 is the Go-internal ErrAllProxiesUnreachable (NOT C++'s
	// 1200=recruitment_failed; see pkg/fdbgo/client/transaction.go).
	1200: "all_proxies_unreachable",
	1213: "tag_throttled",
	1223: "proxy_tag_throttled",
	1235: "transaction_throttled_hot_shard",
	1242: "transaction_rejected_range_locked",
	2000: "client_invalid_operation",
	2004: "key_outside_legal_range",
	2005: "inverted_range",
	2006: "invalid_option_value",
	2015: "future_not_set",
	2017: "used_during_commit",
	2101: "transaction_too_large",
}

// Retryable returns true if this wire-level error should trigger the
// client's retry loop.
//
// SUPERSET of fdb.IsRetryable (in pkg/fdbgo/fdb/error.go). The fdb
// package mirrors C++'s `fdb_error_predicate(RETRYABLE, code)` exactly
// — 12 canonical codes. This wire-level predicate adds four more:
//
//   - 1006 all_alternatives_failed (Layer 2 retry — covers locality
//     selection failures the Go client retries above the wire layer)
//   - 1200 all_proxies_unreachable (Go-internal — synthesised when the
//     client's proxy pool is exhausted; never appears in real FDB
//     responses)
//   - 1235 transaction_throttled_hot_shard (FDB 7.4+; not in our
//     pinned error_definitions.h but the C++ fdb_c.cpp adds it)
//   - 1242 transaction_rejected_range_locked (same)
//
// Why the wire-side superset: the wire Reader sees codes the FDB
// public API never surfaces (Go-internal 1200), and the client's
// retry loop is the right place to be lenient. If a non-retryable
// error gets retried it eventually surfaces to the caller anyway.
//
// Source: flow/error_definitions.h + fdb_c.cpp fdb_error_predicate().
// Cross-reference: pkg/fdbgo/fdb/error.go::IsRetryable for the strict
// FDB-public-contract version.
func (e *FDBError) Retryable() bool {
	switch e.Code {
	// MAYBE_COMMITTED (canonical)
	case 1021, // commit_unknown_result
		1039: // cluster_version_changed
		return true
	// RETRYABLE_NOT_COMMITTED (canonical)
	case 1007, // transaction_too_old
		1009, // future_version
		1020, // not_committed (conflict)
		1037, // process_behind
		1038, // database_locked
		1042, // commit_proxy_memory_limit_exceeded
		1051, // batch_transaction_throttled
		1078, // grv_proxy_memory_limit_exceeded
		1213, // tag_throttled
		1223, // proxy_tag_throttled
		// Wire-side additions (see method doc comment for rationale):
		1006, // all_alternatives_failed (Layer 2 retry)
		1200, // all_proxies_unreachable (Go-internal)
		1235, // transaction_throttled_hot_shard (FDB 7.4+)
		1242: // transaction_rejected_range_locked (FDB 7.4+)
		return true
	}
	return false
}

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
//	reply.UnmarshalFromReader(&r)
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
		return &FDBError{Code: int(errObj.ReadUint16(0))}
	case errorOrTagValue:
		if err := root.readNestedInto(errorOrValueSlot, r); err != nil {
			return fmt.Errorf("wire: ErrorOr value payload: %w", err)
		}
		return nil
	default:
		return fmt.Errorf("wire: ErrorOr response has unexpected union tag %d", tag)
	}
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
	return nil
}

// readNestedInto follows the RelativeOffset at the given slot to a nested
// FlatBuffers object and writes the sub-Reader into dst (no heap alloc, unlike
// ReadNestedReader). dst may alias r's buffer.
func (r *Reader) readNestedInto(vtableSlot int, dst *Reader) error {
	off := r.fieldOffset(vtableSlot)
	if off < 4 || int(off)+4 > len(r.object) {
		return fmt.Errorf("wire: nested struct field not present (slot %d)", vtableSlot)
	}
	relOffset := binary.LittleEndian.Uint32(r.object[off:])
	if relOffset == 0 {
		return fmt.Errorf("wire: nested struct RelativeOffset is 0 (slot %d)", vtableSlot)
	}
	targetPos := r.objPos + int(off) + int(relOffset)
	if targetPos+4 > len(r.data) {
		return fmt.Errorf("wire: object position %d out of bounds (buf %d)", targetPos, len(r.data))
	}
	vtableSoffset := int32(binary.LittleEndian.Uint32(r.data[targetPos:]))
	vtableAbsPos := targetPos - int(vtableSoffset)
	if vtableAbsPos < 0 || vtableAbsPos+4 > len(r.data) {
		return fmt.Errorf("wire: vtable position %d out of bounds", vtableAbsPos)
	}
	dst.data = r.data
	dst.object = r.data[targetPos:]
	dst.objPos = targetPos
	dst.vtable = r.data[vtableAbsPos:]
	dst.headerOff = r.headerOff
	return nil
}

// ReadUIDPair reads a 16-byte UID as two uint64 values (first, second).
func (r *Reader) ReadUIDPair(vtableSlot int) (uint64, uint64) {
	off := r.fieldOffset(vtableSlot)
	if off < 4 || off+16 > len(r.object) {
		return 0, 0
	}
	first := binary.LittleEndian.Uint64(r.object[off:])
	second := binary.LittleEndian.Uint64(r.object[off+8:])
	return first, second
}

// ReadIPv4 reads a uint32 IPv4 address from a RelativeOffset field and returns it
// as a "host:0" string. The uint32 is stored little-endian on wire but represents
// a network-byte-order IPv4 address.
// ReadRelOffRaw reads N raw bytes at a RelativeOffset target.
// Used for variant (union_like) values where the data at the RelOff
// is raw (no length prefix), unlike ReadBytes which expects [len][data].
func (r *Reader) ReadRelOffRaw(vtableSlot int, n int) []byte {
	off := r.fieldOffset(vtableSlot)
	if off < 4 || int(off)+4 > len(r.object) {
		return nil
	}
	relOffset := binary.LittleEndian.Uint32(r.object[off:])
	if relOffset == 0 {
		return nil
	}
	target := r.objPos + int(off) + int(relOffset)
	if target+n > len(r.data) {
		return nil
	}
	result := make([]byte, n)
	copy(result, r.data[target:target+n])
	return result
}

// ReadRelOffUint32 reads a uint32 at a RelativeOffset target.
// Used for variant alternatives that are scalar uint32 (e.g., IPv4 in IPAddress).
func (r *Reader) ReadRelOffUint32(vtableSlot int) uint32 {
	off := r.fieldOffset(vtableSlot)
	if off < 4 || int(off)+4 > len(r.object) {
		return 0
	}
	relOffset := binary.LittleEndian.Uint32(r.object[off:])
	if relOffset == 0 {
		return 0
	}
	target := r.objPos + int(off) + int(relOffset)
	if target+4 > len(r.data) {
		return 0
	}
	return binary.LittleEndian.Uint32(r.data[target:])
}

func (r *Reader) ReadIPv4(vtableSlot int) uint32 {
	off := r.fieldOffset(vtableSlot)
	if off < 4 || int(off)+4 > len(r.object) {
		return 0
	}
	relOffset := binary.LittleEndian.Uint32(r.object[off:])
	if relOffset == 0 {
		return 0
	}
	target := r.objPos + int(off) + int(relOffset)
	if target+4 > len(r.data) {
		return 0
	}
	return binary.LittleEndian.Uint32(r.data[target:])
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
