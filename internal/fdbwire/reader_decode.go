package wire

import (
	"encoding/binary"
	"fmt"
	"math"
)

// ReadInt8 reads an int8 from the given vtable slot.
func (r *Reader) ReadInt8(vtableSlot int) int8 {
	off := r.fieldOffset(vtableSlot)
	if off < 4 || off+1 > len(r.object) {
		r.outside(vtableSlot, off)
		return 0
	}
	return int8(r.object[off])
}

// ReadUint8 reads a uint8 from the given vtable slot.
func (r *Reader) ReadUint8(vtableSlot int) uint8 {
	off := r.fieldOffset(vtableSlot)
	if off < 4 || off+1 > len(r.object) {
		r.outside(vtableSlot, off)
		return 0
	}
	return r.object[off]
}

// ReadInt16 reads an int16 from the given vtable slot.
func (r *Reader) ReadInt16(vtableSlot int) int16 {
	off := r.fieldOffset(vtableSlot)
	if off < 4 || off+2 > len(r.object) {
		r.outside(vtableSlot, off)
		return 0
	}
	return int16(binary.LittleEndian.Uint16(r.object[off:]))
}

// ReadUint16 reads a uint16 from the given vtable slot.
func (r *Reader) ReadUint16(vtableSlot int) uint16 {
	off := r.fieldOffset(vtableSlot)
	if off < 4 || off+2 > len(r.object) {
		r.outside(vtableSlot, off)
		return 0
	}
	return binary.LittleEndian.Uint16(r.object[off:])
}

// ReadInt32 reads an int32 from the given vtable slot.
func (r *Reader) ReadInt32(vtableSlot int) int32 {
	off := r.fieldOffset(vtableSlot)
	if off < 4 || off+4 > len(r.object) {
		r.outside(vtableSlot, off)
		return 0
	}
	return int32(binary.LittleEndian.Uint32(r.object[off:]))
}

// ReadUint32 reads a uint32 from the given vtable slot.
func (r *Reader) ReadUint32(vtableSlot int) uint32 {
	off := r.fieldOffset(vtableSlot)
	if off < 4 || off+4 > len(r.object) {
		r.outside(vtableSlot, off)
		return 0
	}
	return binary.LittleEndian.Uint32(r.object[off:])
}

// ReadInt64 reads an int64 from the given vtable slot.
func (r *Reader) ReadInt64(vtableSlot int) int64 {
	off := r.fieldOffset(vtableSlot)
	if off < 4 || off+8 > len(r.object) {
		r.outside(vtableSlot, off)
		return 0
	}
	return int64(binary.LittleEndian.Uint64(r.object[off:]))
}

// ReadUint64 reads a uint64 from the given vtable slot.
func (r *Reader) ReadUint64(vtableSlot int) uint64 {
	off := r.fieldOffset(vtableSlot)
	if off < 4 || off+8 > len(r.object) {
		r.outside(vtableSlot, off)
		return 0
	}
	return binary.LittleEndian.Uint64(r.object[off:])
}

// ReadFloat64 reads a float64 from the given vtable slot (LE IEEE754).
func (r *Reader) ReadFloat64(vtableSlot int) float64 {
	off := r.fieldOffset(vtableSlot)
	if off < 4 || off+8 > len(r.object) {
		r.outside(vtableSlot, off)
		return 0
	}
	return math.Float64frombits(binary.LittleEndian.Uint64(r.object[off:]))
}

// ReadBool reads a bool from the given vtable slot.
func (r *Reader) ReadBool(vtableSlot int) bool {
	off := r.fieldOffset(vtableSlot)
	if off < 4 || off+1 > len(r.object) {
		r.outside(vtableSlot, off)
		return false
	}
	return r.object[off] != 0
}

// ReadUID reads a 16-byte UID from the given vtable slot (inline).
func (r *Reader) ReadUID(vtableSlot int) [16]byte {
	off := r.fieldOffset(vtableSlot)
	if off < 4 || off+16 > len(r.object) {
		r.outside(vtableSlot, off)
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
		r.outside(vtableSlot, off)
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
		r.outside(vtableSlot, off)
		return nil
	}
	length := binary.LittleEndian.Uint32(r.data[target:])
	dataStart := target + 4
	if dataStart+int(length) > len(r.data) {
		r.outside(vtableSlot, off)
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
		r.outside(vtableSlot, off)
		return nil
	}
	relOffset := binary.LittleEndian.Uint32(r.object[off:])
	if relOffset == 0 {
		return nil
	}
	target := r.objPos + int(off) + int(relOffset)
	if target+4 > len(r.data) {
		r.outside(vtableSlot, off)
		return nil
	}
	count := binary.LittleEndian.Uint32(r.data[target:])
	dataStart := target + 4
	if dataStart+int(count)*4 > len(r.data) {
		r.outside(vtableSlot, off)
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
		r.outside(vtableSlot, off)
		return nil
	}
	relOffset := binary.LittleEndian.Uint32(r.object[off:])
	if relOffset == 0 {
		return nil
	}
	target := r.objPos + int(off) + int(relOffset)
	if target+4 > len(r.data) {
		r.outside(vtableSlot, off)
		return nil
	}
	count := binary.LittleEndian.Uint32(r.data[target:])
	dataStart := target + 4
	if dataStart+int(count)*8 > len(r.data) {
		r.outside(vtableSlot, off)
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
	if r.ReadUint8(typeSlot) != 1 {
		return 0, false // tag 1 is the only alternative, as in C++
	}
	valOff := r.fieldOffset(valueSlot)
	if valOff < 4 || int(valOff)+4 > len(r.object) {
		r.outside(valueSlot, valOff)
		return 0, false
	}
	relOffset := binary.LittleEndian.Uint32(r.object[valOff:])
	if relOffset == 0 {
		return 0, false
	}
	target := r.objPos + int(valOff) + int(relOffset)
	if target+4 > len(r.data) {
		r.outside(valueSlot, valOff)
		return 0, false
	}
	return int32(binary.LittleEndian.Uint32(r.data[target:])), true
}

// ReadOptionalString reads an Optional<string>. Returns (value, present).
func (r *Reader) ReadOptionalString(typeSlot, valueSlot int) (string, bool) {
	if r.ReadUint8(typeSlot) != 1 {
		return "", false // tag 1 is the only alternative, as in C++
	}
	valOff := r.fieldOffset(valueSlot)
	if valOff < 4 || int(valOff)+4 > len(r.object) {
		r.outside(valueSlot, valOff)
		return "", false
	}
	relOffset := binary.LittleEndian.Uint32(r.object[valOff:])
	if relOffset == 0 {
		return "", false
	}
	target := r.objPos + int(valOff) + int(relOffset)
	if target+4 > len(r.data) {
		r.outside(valueSlot, valOff)
		return "", false
	}
	length := binary.LittleEndian.Uint32(r.data[target:])
	end := target + 4 + int(length)
	if end > len(r.data) {
		r.outside(valueSlot, valOff)
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
	if off < 4 {
		return nil, fmt.Errorf("wire: nested struct field not present (slot %d)", vtableSlot)
	}
	if off+4 > len(r.object) {
		return nil, &FieldBoundsError{Slot: vtableSlot}
	}
	relOffset := binary.LittleEndian.Uint32(r.object[off:])
	if relOffset == 0 {
		return nil, fmt.Errorf("wire: nested struct RelativeOffset is 0 (slot %d)", vtableSlot)
	}
	targetPos := r.objPos + int(off) + int(relOffset)
	return r.readerAtObject(targetPos)
}

// VectorCountError reports a vector whose declared count cannot fit in the rest
// of the buffer at four bytes per element offset.
type VectorCountError struct {
	Slot     int
	Count    uint32
	Capacity int
}

func (e *VectorCountError) Error() string {
	return fmt.Sprintf("wire: vector in slot %d declares %d elements, buffer holds at most %d", e.Slot, e.Count, e.Capacity)
}

// ReadVectorCount returns the number of elements in a vector-of-struct field.
func (r *Reader) ReadVectorCount(vtableSlot int) (int, error) {
	off := r.fieldOffset(vtableSlot)
	if off < 4 {
		return 0, r.Err() // a genuinely absent field is empty; a truncated entry is not
	}
	if int(off)+4 > len(r.object) {
		return 0, &FieldBoundsError{Slot: vtableSlot}
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
	// Each element offset takes 4 bytes; a larger count is malformed, and reading
	// it would take the bytes after the vector for element offsets.
	maxCount := (len(r.data) - vecStart - 4) / 4
	if int64(count) > int64(maxCount) {
		return 0, &VectorCountError{Slot: vtableSlot, Count: count, Capacity: maxCount}
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

// ReadVectorBytes reads the i-th element of a vector of length-prefixed byte
// strings — C++ VectorRef<KeyRef>/VectorRef<StringRef> with the default
// FlatBuffers strategy (flat_buffers.h save(VectorLike): [uint32 count]
// [RelativeOffset]*, each offset → [uint32 len][data]). NOT the
// VecSerStrategy::String inline format, which is a single dynamic blob.
func (r *Reader) ReadVectorBytes(vtableSlot, index int) ([]byte, error) {
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
	elemRelOffPos := vecStart + 4 + index*4
	if elemRelOffPos+4 > len(r.data) {
		return nil, fmt.Errorf("wire: vector element offset out of bounds")
	}
	elemRelOff := binary.LittleEndian.Uint32(r.data[elemRelOffPos:])
	target := elemRelOffPos + int(elemRelOff)
	if target+4 > len(r.data) {
		return nil, fmt.Errorf("wire: vector element out of bounds")
	}
	length := int(binary.LittleEndian.Uint32(r.data[target:]))
	dataStart := target + 4
	if length < 0 || dataStart+length > len(r.data) {
		return nil, fmt.Errorf("wire: vector element data out of bounds")
	}
	return r.data[dataStart : dataStart+length], nil
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
	dst.fault = 0
	return nil
}

// ReadUIDPair reads a 16-byte UID as two uint64 values (first, second).
func (r *Reader) ReadUIDPair(vtableSlot int) (uint64, uint64) {
	off := r.fieldOffset(vtableSlot)
	if off < 4 || off+16 > len(r.object) {
		r.outside(vtableSlot, off)
		return 0, 0
	}
	first := binary.LittleEndian.Uint64(r.object[off:])
	second := binary.LittleEndian.Uint64(r.object[off+8:])
	return first, second
}

// ReadRelOffRaw reads N raw bytes at a RelativeOffset target.
// Used for variant (union_like) values where the data at the RelOff
// is raw (no length prefix), unlike ReadBytes which expects [len][data].
func (r *Reader) ReadRelOffRaw(vtableSlot int, n int) []byte {
	off := r.fieldOffset(vtableSlot)
	if off < 4 || int(off)+4 > len(r.object) {
		r.outside(vtableSlot, off)
		return nil
	}
	relOffset := binary.LittleEndian.Uint32(r.object[off:])
	if relOffset == 0 {
		return nil
	}
	target := r.objPos + int(off) + int(relOffset)
	if target+n > len(r.data) {
		r.outside(vtableSlot, off)
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
		r.outside(vtableSlot, off)
		return 0
	}
	relOffset := binary.LittleEndian.Uint32(r.object[off:])
	if relOffset == 0 {
		return 0
	}
	target := r.objPos + int(off) + int(relOffset)
	if target+4 > len(r.data) {
		r.outside(vtableSlot, off)
		return 0
	}
	return binary.LittleEndian.Uint32(r.data[target:])
}

// ReadRelOffUint64 reads a uint64 at a RelativeOffset target — the 8-byte sibling
// of ReadRelOffUint32. Used for Optional<scalar> / variant alternatives whose inner
// is an 8-byte scalar written bare out-of-line behind the union RelativeOffset
// (C++ SaveAlternative non-indirection arm, flat_buffers.h:848), e.g.
// Optional<Version> (int64) consistencyCheckStartVersion. Bounds-checked like the
// uint32 sibling: a short/absent buffer returns 0 rather than panicking (the
// generated decode must not panic on a truncated field).
func (r *Reader) ReadRelOffUint64(vtableSlot int) uint64 {
	off := r.fieldOffset(vtableSlot)
	if off < 4 || int(off)+4 > len(r.object) {
		r.outside(vtableSlot, off)
		return 0
	}
	relOffset := binary.LittleEndian.Uint32(r.object[off:])
	if relOffset == 0 {
		return 0
	}
	target := r.objPos + int(off) + int(relOffset)
	if target+8 > len(r.data) {
		r.outside(vtableSlot, off)
		return 0
	}
	return binary.LittleEndian.Uint64(r.data[target:])
}

// ReadIPv4 reads a uint32 IPv4 address from a RelativeOffset field.
// The uint32 is stored little-endian on wire but represents a network-byte-order address.
func (r *Reader) ReadIPv4(vtableSlot int) uint32 {
	off := r.fieldOffset(vtableSlot)
	if off < 4 || int(off)+4 > len(r.object) {
		r.outside(vtableSlot, off)
		return 0
	}
	relOffset := binary.LittleEndian.Uint32(r.object[off:])
	if relOffset == 0 {
		return 0
	}
	target := r.objPos + int(off) + int(relOffset)
	if target+4 > len(r.data) {
		r.outside(vtableSlot, off)
		return 0
	}
	return binary.LittleEndian.Uint32(r.data[target:])
}
