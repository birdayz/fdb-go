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
	data   []byte // full buffer
	object []byte // slice starting at the MESSAGE object (not FakeRoot)
	vtable []byte // slice starting at the message's vtable
}

// NewReader parses the buffer, navigates through the FakeRoot to the message object.
func NewReader(data []byte) (*Reader, error) {
	if len(data) < 8 {
		return nil, fmt.Errorf("wire: buffer too short (%d bytes, need at least 8)", len(data))
	}

	// Root footer: [root_offset(4)][file_id(4)]
	rootOffset := binary.LittleEndian.Uint32(data[0:4])
	if int(rootOffset)+8 > len(data) {
		return nil, fmt.Errorf("wire: root_offset %d out of bounds (buffer length %d)", rootOffset, len(data))
	}

	// FakeRoot object at data[rootOffset].
	frObj := data[rootOffset:]
	if len(frObj) < 8 {
		return nil, fmt.Errorf("wire: FakeRoot object too short")
	}

	// FakeRoot field[0] at offset 4: RelativeOffset to the message object.
	// The FakeRoot vtable is always [6, 8, 4], so field 0 is at object offset 4.
	msgRelOff := binary.LittleEndian.Uint32(frObj[4:8])
	msgAbsPos := int(rootOffset) + 4 + int(msgRelOff)
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
		data:   data,
		object: msgObj,
		vtable: data[vtableAbsPos:],
	}, nil
}

// FileIdentifier reads the file_identifier from the root footer.
func (r *Reader) FileIdentifier() uint32 {
	return binary.LittleEndian.Uint32(r.data[4:8])
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
	return int(binary.LittleEndian.Uint16(r.vtable[entryIndex*2:]))
}

// FieldPresent returns true if the field at the given vtable slot has a non-zero offset.
func (r *Reader) FieldPresent(vtableSlot int) bool {
	return r.fieldOffset(vtableSlot) >= 4
}

// ReadInt8 reads an int8 from the given vtable slot.
func (r *Reader) ReadInt8(vtableSlot int) int8 {
	off := r.fieldOffset(vtableSlot)
	return int8(r.object[off])
}

// ReadUint8 reads a uint8 from the given vtable slot.
func (r *Reader) ReadUint8(vtableSlot int) uint8 {
	off := r.fieldOffset(vtableSlot)
	return r.object[off]
}

// ReadInt16 reads an int16 from the given vtable slot.
func (r *Reader) ReadInt16(vtableSlot int) int16 {
	off := r.fieldOffset(vtableSlot)
	return int16(binary.LittleEndian.Uint16(r.object[off:]))
}

// ReadUint16 reads a uint16 from the given vtable slot.
func (r *Reader) ReadUint16(vtableSlot int) uint16 {
	off := r.fieldOffset(vtableSlot)
	return binary.LittleEndian.Uint16(r.object[off:])
}

// ReadInt32 reads an int32 from the given vtable slot.
func (r *Reader) ReadInt32(vtableSlot int) int32 {
	off := r.fieldOffset(vtableSlot)
	return int32(binary.LittleEndian.Uint32(r.object[off:]))
}

// ReadUint32 reads a uint32 from the given vtable slot.
func (r *Reader) ReadUint32(vtableSlot int) uint32 {
	off := r.fieldOffset(vtableSlot)
	return binary.LittleEndian.Uint32(r.object[off:])
}

// ReadInt64 reads an int64 from the given vtable slot.
func (r *Reader) ReadInt64(vtableSlot int) int64 {
	off := r.fieldOffset(vtableSlot)
	return int64(binary.LittleEndian.Uint64(r.object[off:]))
}

// ReadUint64 reads a uint64 from the given vtable slot.
func (r *Reader) ReadUint64(vtableSlot int) uint64 {
	off := r.fieldOffset(vtableSlot)
	return binary.LittleEndian.Uint64(r.object[off:])
}

// ReadFloat64 reads a float64 from the given vtable slot (LE IEEE754).
func (r *Reader) ReadFloat64(vtableSlot int) float64 {
	off := r.fieldOffset(vtableSlot)
	return math.Float64frombits(binary.LittleEndian.Uint64(r.object[off:]))
}

// ReadBool reads a bool from the given vtable slot.
func (r *Reader) ReadBool(vtableSlot int) bool {
	off := r.fieldOffset(vtableSlot)
	return r.object[off] != 0
}

// ReadUID reads a 16-byte UID from the given vtable slot (inline).
func (r *Reader) ReadUID(vtableSlot int) [16]byte {
	off := r.fieldOffset(vtableSlot)
	var uid [16]byte
	copy(uid[:], r.object[off:off+16])
	return uid
}

// ReadBytes reads a length-prefixed byte slice from out-of-line data.
// The vtable slot contains a RelativeOffset pointing to [uint32 length][data...].
func (r *Reader) ReadBytes(vtableSlot int) []byte {
	off := r.fieldOffset(vtableSlot)
	if off < 4 {
		return nil
	}
	// Read RelativeOffset at object[off:off+4].
	relOffset := binary.LittleEndian.Uint32(r.object[off:])
	if relOffset == 0 {
		return nil
	}
	// Target = &object[off] + relOffset, in terms of the object slice.
	target := int(off) + int(relOffset)
	// Read length.
	length := binary.LittleEndian.Uint32(r.object[target:])
	// Read data.
	dataStart := target + 4
	return r.object[dataStart : dataStart+int(length)]
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
	if off < 4 {
		return nil
	}
	relOffset := binary.LittleEndian.Uint32(r.object[off:])
	if relOffset == 0 {
		return nil
	}
	target := int(off) + int(relOffset)
	count := binary.LittleEndian.Uint32(r.object[target:])
	result := make([]int32, count)
	dataStart := target + 4
	for i := uint32(0); i < count; i++ {
		result[i] = int32(binary.LittleEndian.Uint32(r.object[dataStart+int(i)*4:]))
	}
	return result
}

// ReadVectorUint64 reads a vector of uint64 from out-of-line data.
func (r *Reader) ReadVectorUint64(vtableSlot int) []uint64 {
	off := r.fieldOffset(vtableSlot)
	if off < 4 {
		return nil
	}
	relOffset := binary.LittleEndian.Uint32(r.object[off:])
	if relOffset == 0 {
		return nil
	}
	target := int(off) + int(relOffset)
	count := binary.LittleEndian.Uint32(r.object[target:])
	result := make([]uint64, count)
	dataStart := target + 4
	for i := uint32(0); i < count; i++ {
		result[i] = binary.LittleEndian.Uint64(r.object[dataStart+int(i)*8:])
	}
	return result
}

// ReadOptionalInt32 reads an Optional<int32>. Returns (value, present).
// Optional uses 2 vtable slots: typeSlot (uint8 tag) and valueSlot (RelativeOffset).
func (r *Reader) ReadOptionalInt32(typeSlot, valueSlot int) (int32, bool) {
	typeOff := r.fieldOffset(typeSlot)
	if typeOff < 4 || r.object[typeOff] == 0 {
		return 0, false
	}
	valOff := r.fieldOffset(valueSlot)
	relOffset := binary.LittleEndian.Uint32(r.object[valOff:])
	target := int(valOff) + int(relOffset)
	return int32(binary.LittleEndian.Uint32(r.object[target:])), true
}

// ReadOptionalString reads an Optional<string>. Returns (value, present).
func (r *Reader) ReadOptionalString(typeSlot, valueSlot int) (string, bool) {
	typeOff := r.fieldOffset(typeSlot)
	if typeOff < 4 || r.object[typeOff] == 0 {
		return "", false
	}
	valOff := r.fieldOffset(valueSlot)
	relOffset := binary.LittleEndian.Uint32(r.object[valOff:])
	target := int(valOff) + int(relOffset)
	length := binary.LittleEndian.Uint32(r.object[target:])
	return string(r.object[target+4 : target+4+int(length)]), true
}
