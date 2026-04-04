package wire

import "encoding/binary"

// DirectWriter supports two-pass zero-intermediate-buffer serialization.
// Pass 1: measureEndOff computes total size (arithmetic only, zero alloc).
// Pass 2: writeDirect writes everything into a single pre-allocated buffer.
// Result: exactly 1 allocation (the output buffer) regardless of nesting depth.
type DirectWriter struct {
	buf       []byte
	Cursor    int // next available byte position, moves downward (high → low)
	VtablePos int
	Template  *MessageTemplate
	totalSize int // needed for correct end-offset alignment
}

// WriteBytesOOL writes [len(4)][data][padding] below cursor.
// Returns the byte position of the length prefix (target for RelativeOffset).
// C++ flat_buffers.h:615 WriteToBuffer::visitDynamicSize
func (dw *DirectWriter) WriteBytesOOL(data []byte) int {
	size := len(data) // 0 for nil
	// Match C++: RightAlign(current_buffer_size + size + 4, 4)
	endOff := dw.totalSize - dw.Cursor
	newEndOff := RightAlign(endOff+size+4, 4)
	// The length prefix is at the highest end-offset, data below it.
	// C++: write(&size, start, 4) where start = RightAlign(cbs + size + 4, 4)
	// Then data at start - 4, then start - 4 - size.
	lenPos := dw.totalSize - newEndOff // abs position of length prefix
	binary.LittleEndian.PutUint32(dw.buf[lenPos:], uint32(size))
	copy(dw.buf[lenPos+4:], data)
	dw.Cursor = lenPos
	return lenPos
}

// WriteRawOOL writes [data][padding] below cursor (no length prefix).
// Returns the byte position of the data start.
func (dw *DirectWriter) WriteRawOOL(data []byte) int {
	padded := (len(data) + 3) &^ 3
	start := dw.Cursor - padded
	copy(dw.buf[start:], data)
	dw.Cursor = start
	return start
}

// WriteObject writes the soffset for an object and returns (objPos, obj slice).
// The caller fills in field values directly into the returned obj slice.
// Alignment is computed in end-offset space to match C++ layout exactly.
func (dw *DirectWriter) WriteObject(vt VTable, maxAlign int) (int, []byte) {
	if maxAlign < 4 {
		maxAlign = 4
	}
	objSize := int(vt[1])

	// C++ flat_buffers.h: nested objects just do current_buffer_size += vtable[1].
	// No alignment. Alignment only happens for the root (always to 8, in MarshalFDB).
	endOff := dw.totalSize - dw.Cursor
	newEndOff := endOff + objSize
	objPos := dw.totalSize - newEndOff

	obj := dw.buf[objPos : objPos+objSize]

	// soffset: signed distance from object to its vtable
	vtAddr := dw.VtablePos + dw.Template.vtableOffset(vt)
	binary.LittleEndian.PutUint32(obj, uint32(int32(objPos-vtAddr)))

	dw.Cursor = objPos
	return objPos, obj
}

// ReserveRawOOL reserves `size` bytes below cursor (aligned to 4) and returns
// the start position + a slice into the buffer for direct writing.
// Used for inline vector block construction (header + blobs written by caller).
func (dw *DirectWriter) ReserveRawOOL(size int) (int, []byte) {
	padded := (size + 3) &^ 3
	start := dw.Cursor - padded
	dw.Cursor = start
	return start, dw.buf[start : start+size]
}

// BlobLayout computes the byte layout of a self-contained struct blob.
// Returns (objPos, oolPos, totalSize) relative to blob start.
func BlobLayout(vt VTable) (objPos, oolPos, blobHeaderSize int) {
	vtBytes := len(vt) * 2
	objPos = (vtBytes + 3) &^ 3
	oolPos = (objPos + int(vt[1]) + 3) &^ 3
	blobHeaderSize = vtBytes
	return
}

// WriteBlobVTable writes a vtable + soffset at the given position in buf.
// Returns the object slice (buf[objPos:objPos+objSize]) for field writes.
func WriteBlobVTable(buf []byte, blobStart int, vt VTable) []byte {
	vtBytes := len(vt) * 2
	for i, v := range vt {
		binary.LittleEndian.PutUint16(buf[blobStart+i*2:], v)
	}
	objPos := blobStart + (vtBytes+3)&^3
	objSize := int(vt[1])
	// soffset: distance from object to vtable (always points back to blob start)
	binary.LittleEndian.PutUint32(buf[objPos:], uint32(int32(objPos-blobStart)))
	return buf[objPos : objPos+objSize]
}

// PatchBlobRelOff writes a RelativeOffset within a blob's object.
func PatchBlobRelOff(obj []byte, fieldOff int, objAbsPos int, targetAbsPos int) {
	binary.LittleEndian.PutUint32(obj[fieldOff:], uint32(targetAbsPos-(objAbsPos+fieldOff)))
}

// PatchRelOff writes a RelativeOffset at obj[fieldOff] pointing to targetPos.
func PatchRelOff(obj []byte, fieldOff int, objPos int, targetPos int) {
	binary.LittleEndian.PutUint32(obj[fieldOff:], uint32(targetPos-(objPos+fieldOff)))
}

// MeasureBytesOOL returns the end-offset contribution of a WriteBytes field.
// C++ flat_buffers.h:518 PrecomputeSize::visitDynamicSize — ALWAYS allocates
// at least 4 bytes (the length prefix), even for nil/empty data.
// C++ has an emptyVector optimization (first empty field allocates 4 bytes,
// subsequent reuse the same offset) but we always allocate for simplicity.
// This may over-allocate by 4 bytes for types with multiple nil fields.
func MeasureBytesOOL(endOff int, data []byte) int {
	size := len(data) // 0 for nil
	// C++: RightAlign(current_buffer_size + size + 4, 4)
	return RightAlign(endOff+size+4, 4)
}

// MeasureRawOOL returns the end-offset contribution of a WriteRawOOL field.
// C++ flat_buffers.h:518 visitDynamicSize treats ALL dynamic_size types the same:
// always [len(4)][data][pad], even for empty. Empty data still gets 4 bytes.
func MeasureRawOOL(endOff int, data []byte) int {
	size := len(data) // 0 for nil
	// C++: RightAlign(current_buffer_size + size + 4, 4)
	return RightAlign(endOff+size+4, 4)
}

// MeasureObject returns the end-offset after adding a nested object.
// C++ flat_buffers.h: nested objects just do current_buffer_size += vtable[1].
// No alignment for nested objects — alignment only happens at the root
// (always to 8, in MarshalFDB). The maxAlign parameter is unused but
// kept for API compatibility.
// MeasureObject computes the end-offset after writing an object.
// C++ SaveVisitorLambda (flat_buffers.h:972):
//
//	RightAlign(current_buffer_size + vtable[1] - 4, max(4, fb_align<Members>...)) + 4
func MeasureObject(endOff int, vt VTable, maxAlign int) int {
	if maxAlign < 4 {
		maxAlign = 4
	}
	bodySize := int(vt[1]) - 4
	return rightAlign(endOff+bodySize, maxAlign) + 4
}

// Init initializes a stack-allocated DirectWriter.
func (dw *DirectWriter) Init(buf []byte, totalSize int, vtablePos int, t *MessageTemplate) {
	dw.buf = buf
	dw.Cursor = totalSize
	dw.VtablePos = vtablePos
	dw.Template = t
	dw.totalSize = totalSize
}

// PackedVTablesLen returns the byte length of the pre-packed vtable data.
func (t *MessageTemplate) PackedVTablesLen() int {
	return len(t.packedVTables)
}

// WriteFakeRoot writes the FakeRoot object at the given position.
func (t *MessageTemplate) WriteFakeRoot(buf []byte, fakeRootPos, vtablePos, msgObjPos int) {
	fakeRootVTable := VTable{6, 8, 4}
	fakeRootVTAddr := vtablePos + t.vtableOffset(fakeRootVTable)
	binary.LittleEndian.PutUint32(buf[fakeRootPos:], uint32(int32(fakeRootPos-fakeRootVTAddr)))
	binary.LittleEndian.PutUint32(buf[fakeRootPos+4:], uint32(msgObjPos-(fakeRootPos+4)))
}

// WriteVTablesAndFooter writes the vtable data and root footer.
func (t *MessageTemplate) WriteVTablesAndFooter(buf []byte, vtablePos, fakeRootPos int) {
	copy(buf[vtablePos:], t.packedVTables)
	binary.LittleEndian.PutUint32(buf[0:], uint32(fakeRootPos))
	binary.LittleEndian.PutUint32(buf[4:], t.fileID)
}

// WriteRootUnionFooter writes vtables + footer for root-union layout (no FakeRoot).
// Used by ErrorOr and other union_like_traits types at the root level.
func (t *MessageTemplate) WriteRootUnionFooter(buf []byte, vtablePos, msgObjPos int) {
	copy(buf[vtablePos:], t.packedVTables)
	binary.LittleEndian.PutUint32(buf[0:], uint32(msgObjPos))
	binary.LittleEndian.PutUint32(buf[4:], t.fileID)
}

// MarshalDirect performs two-pass serialization. 1 allocation total.
//
// measureFn: returns the end-offset contributed by all nested content
// (OOL data + nested structs), NOT including the root object itself.
//
// writeFn: writes everything (OOL, nested structs, root object) into
// the DirectWriter's buffer. Returns the root object byte position.
func MarshalDirect(t *MessageTemplate, measureFn func(int) int, writeFn func(*DirectWriter) int) []byte {
	fakeRootVTable := VTable{6, 8, 4}

	// Pass 1: compute total size
	endOff := measureFn(0)
	bodySize := t.msgObjSize - 4
	msgObjEnd := rightAlign(endOff+bodySize, t.maxFieldAlign) + 4
	fakeRootEnd := rightAlign(msgObjEnd+4, 4) + 4
	vtableSize := len(t.packedVTables)
	vtableEnd := fakeRootEnd + vtableSize
	totalSize := rightAlign(vtableEnd+8, 8)

	// Positions
	msgObjPos := totalSize - msgObjEnd
	fakeRootPos := totalSize - fakeRootEnd
	vtablePos := totalSize - vtableEnd

	// THE one allocation
	buf := make([]byte, totalSize)

	// Pass 2: write
	dw := &DirectWriter{
		buf:       buf,
		Cursor:    totalSize,
		VtablePos: vtablePos,
		Template:  t,
		totalSize: totalSize,
	}

	objPos := writeFn(dw)
	_ = msgObjPos // objPos should equal msgObjPos; verified by byte-comparison tests
	_ = objPos

	// FakeRoot
	fakeRootVTAddr := vtablePos + t.vtableOffset(fakeRootVTable)
	binary.LittleEndian.PutUint32(buf[fakeRootPos:], uint32(int32(fakeRootPos-fakeRootVTAddr)))
	binary.LittleEndian.PutUint32(buf[fakeRootPos+4:], uint32(msgObjPos-(fakeRootPos+4)))

	// VTables
	copy(buf[vtablePos:], t.packedVTables)

	// Footer
	binary.LittleEndian.PutUint32(buf[0:], uint32(fakeRootPos))
	binary.LittleEndian.PutUint32(buf[4:], t.fileID)

	return buf
}
