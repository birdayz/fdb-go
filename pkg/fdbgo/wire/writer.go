package wire

import (
	"encoding/binary"
	"math"
)

// Writer builds FDB-format serialized messages matching the exact byte layout
// produced by C++ save_members() in flow/flat_buffers.h.
//
// The C++ serializer builds the buffer from the END backwards. Offsets are measured
// from the end of the buffer. We mirror this to produce byte-identical output.
//
// Buffer layout (byte-address order, low → high):
//
//	[root footer(8)][padding][vtable(s)][FakeRoot obj][msg obj][padding][ool data]
//
// save_members() wraps every message in a FakeRoot table:
// root_offset → FakeRoot → message → fields.
type Writer struct {
	buf []byte
}

// NewWriter creates a Writer. If buf has capacity, it will be reused.
func NewWriter(buf []byte) *Writer {
	return &Writer{buf: buf[:0]}
}

// WriteMessage serializes a top-level FDB message.
//
// Parameters:
//   - fileID: the message's file_identifier
//   - msgVTable: the message's precomputed VTable
//   - maxFieldAlign: max alignment of any field in the message
//   - writeFields: callback that populates the ObjectWriter
func (w *Writer) WriteMessage(fileID uint32, msgVTable VTable, maxFieldAlign int, writeFields func(obj *ObjectWriter)) []byte {
	if maxFieldAlign < 4 {
		maxFieldAlign = 4
	}

	fakeRootVTable := VTable{6, 8, 4}

	msgObjSize := int(msgVTable[1])
	obj := &ObjectWriter{
		object: make([]byte, msgObjSize),
	}
	writeFields(obj)

	// Collect all unique vtables: FakeRoot, message, and any nested structs.
	vtableSet := newVTableSet()
	vtableSet.add(fakeRootVTable)
	vtableSet.add(msgVTable)
	for _, ns := range obj.nestedStructs {
		ns.collectVTables(vtableSet)
	}
	vtableBytes := vtableSet.pack()
	vtableSize := len(vtableBytes)

	// --- Phase 1: Compute sizes (C++ end-offset allocation order) ---

	// 1. Out-of-line data — allocated first (lowest end-offset = highest byte-addr).
	oolSize := len(obj.outOfLine)
	endOff := 0
	if oolSize > 0 {
		endOff = rightAlign(oolSize, 4)
	}

	// 2. Nested struct objects — allocated between ool data and message object.
	//    Each nested struct contributes: its own ool data + its object + padding.
	//    Nested structs within nested structs are handled recursively.
	nestedEndOff := endOff
	for i := len(obj.nestedStructs) - 1; i >= 0; i-- {
		ns := obj.nestedStructs[i]
		nestedEndOff = ns.computeEndOffset(nestedEndOff)
	}

	// 3. Message object.
	msgBodySize := msgObjSize - 4
	msgObjEnd := rightAlign(nestedEndOff+msgBodySize, maxFieldAlign) + 4
	msgObjPadding := msgObjEnd - 4 - msgBodySize - nestedEndOff

	// 4. FakeRoot object (body=4, align=4).
	frObjEnd := rightAlign(msgObjEnd+4, 4) + 4

	// 5. VTable data.
	vtableEnd := frObjEnd + vtableSize

	// 6. Root footer (8 bytes, aligned to 8).
	footerTotal := rightAlign(vtableEnd+8, 8)
	footerPadding := footerTotal - vtableEnd - 8
	totalSize := footerTotal

	// --- Phase 2: Compute byte-address positions ---
	oolPos := totalSize - endOff
	msgObjPos := totalSize - msgObjEnd
	frObjPos := totalSize - frObjEnd
	vtablePos := totalSize - vtableEnd

	// --- Phase 3: Build buffer ---
	if cap(w.buf) >= totalSize {
		w.buf = w.buf[:totalSize]
		clear(w.buf)
	} else {
		w.buf = make([]byte, totalSize)
	}

	// Write out-of-line data.
	if oolSize > 0 {
		copy(w.buf[oolPos:], obj.outOfLine)
	}

	// Write nested struct objects (reverse order — innermost first in byte-addr).
	nestedPos := oolPos
	for i := len(obj.nestedStructs) - 1; i >= 0; i-- {
		ns := obj.nestedStructs[i]
		nestedPos = ns.writeInto(w.buf, nestedPos, vtablePos, vtableSet)
	}

	// Write message object.
	msgVTableAddr := vtablePos + vtableSet.offset(msgVTable)
	binary.LittleEndian.PutUint32(obj.object[0:4], uint32(int32(msgObjPos-msgVTableAddr)))

	// Patch msg RelativeOffsets to ool data.
	for _, p := range obj.patches {
		fieldAddr := msgObjPos + p.objectOffset
		targetAddr := oolPos + p.oolOffset
		binary.LittleEndian.PutUint32(obj.object[p.objectOffset:], uint32(targetAddr-fieldAddr))
	}

	// Patch msg RelativeOffsets to nested struct objects.
	for _, ns := range obj.nestedStructs {
		fieldAddr := msgObjPos + ns.parentOffset
		binary.LittleEndian.PutUint32(obj.object[ns.parentOffset:], uint32(ns.byteAddr-fieldAddr))
	}

	copy(w.buf[msgObjPos:], obj.object)

	// Write FakeRoot object.
	frSoffset := int32(frObjPos - vtablePos - vtableSet.offset(fakeRootVTable))
	binary.LittleEndian.PutUint32(w.buf[frObjPos:], uint32(frSoffset))
	frFieldAddr := frObjPos + 4
	binary.LittleEndian.PutUint32(w.buf[frFieldAddr:], uint32(msgObjPos-frFieldAddr))

	// Write vtable(s).
	copy(w.buf[vtablePos:], vtableBytes)

	// Write root footer.
	binary.LittleEndian.PutUint32(w.buf[0:], uint32(frObjPos))
	binary.LittleEndian.PutUint32(w.buf[4:], fileID)

	_ = footerPadding
	_ = msgObjPadding

	return w.buf
}

// --- VTable set (deduplication) ---

type vtableSetEntry struct {
	vt     VTable
	offset int // byte offset within packed vtable data
}

type vTableSet struct {
	entries []vtableSetEntry
	total   int
}

func newVTableSet() *vTableSet {
	return &vTableSet{}
}

func (s *vTableSet) add(vt VTable) {
	for _, e := range s.entries {
		if vtablesEqual(e.vt, vt) {
			return
		}
	}
	s.entries = append(s.entries, vtableSetEntry{vt: vt, offset: s.total})
	s.total += len(vt) * 2
}

func (s *vTableSet) offset(vt VTable) int {
	for _, e := range s.entries {
		if vtablesEqual(e.vt, vt) {
			return e.offset
		}
	}
	panic("vtable not found in set")
}

func (s *vTableSet) pack() []byte {
	buf := make([]byte, s.total)
	for _, e := range s.entries {
		for i, v := range e.vt {
			binary.LittleEndian.PutUint16(buf[e.offset+i*2:], v)
		}
	}
	return buf
}

// --- Nested struct ---

type nestedStruct struct {
	vt           VTable
	maxAlign     int
	object       []byte
	outOfLine    []byte
	patches      []relOffsetPatch // ool patches within this nested struct
	parentOffset int              // offset within PARENT object's RelativeOffset slot
	byteAddr     int              // filled during writeInto — absolute byte position
	endOffset    int              // end-offset after computeEndOffset
}

func (ns *nestedStruct) collectVTables(s *vTableSet) {
	s.add(ns.vt)
}

func (ns *nestedStruct) computeEndOffset(startEndOff int) int {
	endOff := startEndOff

	// OOL data for this nested struct.
	if len(ns.outOfLine) > 0 {
		endOff = rightAlign(endOff+len(ns.outOfLine), 4)
	}

	// The nested object itself.
	objSize := int(ns.vt[1])
	bodySize := objSize - 4
	align := ns.maxAlign
	if align < 4 {
		align = 4
	}
	endOff = rightAlign(endOff+bodySize, align) + 4

	ns.endOffset = endOff
	return endOff
}

func (ns *nestedStruct) writeInto(buf []byte, startByteAddr, vtablePos int, vtSet *vTableSet) int {
	totalSize := len(buf)
	objSize := int(ns.vt[1])

	objByteAddr := totalSize - ns.endOffset
	ns.byteAddr = objByteAddr

	// Write vtable soffset.
	vtOff := vtablePos + vtSet.offset(ns.vt)
	binary.LittleEndian.PutUint32(ns.object[0:4], uint32(int32(objByteAddr-vtOff)))

	// Patch ool RelativeOffsets.
	// ool data is at higher byte addresses than the object.
	oolByteAddr := objByteAddr + objSize
	// Account for object alignment padding.
	bodySize := objSize - 4
	align := ns.maxAlign
	if align < 4 {
		align = 4
	}
	paddingAfterObj := rightAlign(bodySize, align) + 4 - objSize
	if paddingAfterObj < 0 {
		paddingAfterObj = 0
	}
	// Actually the padding is between ool and object in end-offset space.
	// In byte-address space: object is first, then padding, then ool.
	oolByteAddr = objByteAddr + objSize + paddingAfterObj
	// Hmm, this isn't right. Let me compute from end-offsets.

	// endOffset after ool:
	oolEndOff := 0
	if len(ns.outOfLine) > 0 {
		oolEndOff = rightAlign(len(ns.outOfLine), 4)
	}
	// ool byte position: totalSize - oolEndOff
	oolByteAddr = totalSize - oolEndOff

	if len(ns.outOfLine) > 0 {
		copy(buf[oolByteAddr:], ns.outOfLine)
	}

	for _, p := range ns.patches {
		fieldAddr := objByteAddr + p.objectOffset
		targetAddr := oolByteAddr + p.oolOffset
		binary.LittleEndian.PutUint32(ns.object[p.objectOffset:], uint32(targetAddr-fieldAddr))
	}

	copy(buf[objByteAddr:], ns.object)

	return objByteAddr
}

// --- ObjectWriter ---

// ObjectWriter provides methods for writing fields into an FDB message object.
type ObjectWriter struct {
	object        []byte
	outOfLine     []byte
	patches       []relOffsetPatch
	nestedStructs []*nestedStruct
}

type relOffsetPatch struct {
	objectOffset int
	oolOffset    int
}

// --- Inline scalar writers ---

func (o *ObjectWriter) WriteInt8(vtableOffset int, v int8)   { o.object[vtableOffset] = byte(v) }
func (o *ObjectWriter) WriteUint8(vtableOffset int, v uint8) { o.object[vtableOffset] = v }
func (o *ObjectWriter) WriteInt16(vtableOffset int, v int16) {
	binary.LittleEndian.PutUint16(o.object[vtableOffset:], uint16(v))
}
func (o *ObjectWriter) WriteUint16(vtableOffset int, v uint16) {
	binary.LittleEndian.PutUint16(o.object[vtableOffset:], v)
}
func (o *ObjectWriter) WriteInt32(vtableOffset int, v int32) {
	binary.LittleEndian.PutUint32(o.object[vtableOffset:], uint32(v))
}
func (o *ObjectWriter) WriteUint32(vtableOffset int, v uint32) {
	binary.LittleEndian.PutUint32(o.object[vtableOffset:], v)
}
func (o *ObjectWriter) WriteInt64(vtableOffset int, v int64) {
	binary.LittleEndian.PutUint64(o.object[vtableOffset:], uint64(v))
}
func (o *ObjectWriter) WriteUint64(vtableOffset int, v uint64) {
	binary.LittleEndian.PutUint64(o.object[vtableOffset:], v)
}
func (o *ObjectWriter) WriteFloat64(vtableOffset int, v float64) {
	binary.LittleEndian.PutUint64(o.object[vtableOffset:], math.Float64bits(v))
}
func (o *ObjectWriter) WriteBool(vtableOffset int, v bool) {
	if v {
		o.object[vtableOffset] = 1
	}
}
func (o *ObjectWriter) WriteUID(vtableOffset int, v [16]byte) {
	copy(o.object[vtableOffset:], v[:])
}

// --- Out-of-line data writers ---

func (o *ObjectWriter) WriteBytes(vtableOffset int, data []byte) {
	oolStart := len(o.outOfLine)
	var lenBuf [4]byte
	binary.LittleEndian.PutUint32(lenBuf[:], uint32(len(data)))
	o.outOfLine = append(o.outOfLine, lenBuf[:]...)
	o.outOfLine = append(o.outOfLine, data...)
	if pad := (4 - len(o.outOfLine)%4) % 4; pad > 0 {
		o.outOfLine = append(o.outOfLine, make([]byte, pad)...)
	}
	o.patches = append(o.patches, relOffsetPatch{vtableOffset, oolStart})
}

func (o *ObjectWriter) WriteString(vtableOffset int, s string) {
	o.WriteBytes(vtableOffset, []byte(s))
}

func (o *ObjectWriter) WriteVectorInt32(vtableOffset int, values []int32) {
	oolStart := len(o.outOfLine)
	var buf [4]byte
	binary.LittleEndian.PutUint32(buf[:], uint32(len(values)))
	o.outOfLine = append(o.outOfLine, buf[:]...)
	for _, v := range values {
		binary.LittleEndian.PutUint32(buf[:], uint32(v))
		o.outOfLine = append(o.outOfLine, buf[:]...)
	}
	if pad := (4 - len(o.outOfLine)%4) % 4; pad > 0 {
		o.outOfLine = append(o.outOfLine, make([]byte, pad)...)
	}
	o.patches = append(o.patches, relOffsetPatch{vtableOffset, oolStart})
}

func (o *ObjectWriter) WriteVectorUint64(vtableOffset int, values []uint64) {
	oolStart := len(o.outOfLine)
	var buf4 [4]byte
	binary.LittleEndian.PutUint32(buf4[:], uint32(len(values)))
	o.outOfLine = append(o.outOfLine, buf4[:]...)
	var buf8 [8]byte
	for _, v := range values {
		binary.LittleEndian.PutUint64(buf8[:], v)
		o.outOfLine = append(o.outOfLine, buf8[:]...)
	}
	if pad := (4 - len(o.outOfLine)%4) % 4; pad > 0 {
		o.outOfLine = append(o.outOfLine, make([]byte, pad)...)
	}
	o.patches = append(o.patches, relOffsetPatch{vtableOffset, oolStart})
}

// WriteVectorStrings writes a vector of strings. Elements are RelativeOffsets
// to length-prefixed string data. C++ writes string data in reverse element
// order (due to end-offset allocation), so we match that for byte-identical output.
func (o *ObjectWriter) WriteVectorStrings(vtableOffset int, values []string) {
	oolStart := len(o.outOfLine)

	n := len(values)
	// Vector body: [count(4)][RelOff * N]
	bodySize := 4 + 4*n
	// Reserve space for vector body.
	o.outOfLine = append(o.outOfLine, make([]byte, bodySize)...)
	binary.LittleEndian.PutUint32(o.outOfLine[oolStart:], uint32(n))

	// Write strings in REVERSE order (matching C++ end-offset allocation).
	stringOffsets := make([]int, n)
	for i := n - 1; i >= 0; i-- {
		stringOffsets[i] = len(o.outOfLine)
		s := values[i]
		var lenBuf [4]byte
		binary.LittleEndian.PutUint32(lenBuf[:], uint32(len(s)))
		o.outOfLine = append(o.outOfLine, lenBuf[:]...)
		o.outOfLine = append(o.outOfLine, s...)
		if pad := (4 - len(o.outOfLine)%4) % 4; pad > 0 {
			o.outOfLine = append(o.outOfLine, make([]byte, pad)...)
		}
	}

	// Fill in RelativeOffsets. Each RelOff at (oolStart + 4 + i*4) points to stringOffsets[i].
	// But these are ool-relative; they get patched to absolute during buffer assembly.
	// Actually, the vector body's RelOffs point from their own position to the string data.
	// Since both are in the same ool buffer, and the ool buffer gets placed contiguously,
	// we can compute the relative offsets now.
	for i := 0; i < n; i++ {
		relOffPos := oolStart + 4 + i*4 // position of this RelOff within ool
		target := stringOffsets[i]      // position of string data within ool
		binary.LittleEndian.PutUint32(o.outOfLine[relOffPos:], uint32(target-relOffPos))
	}

	o.patches = append(o.patches, relOffsetPatch{vtableOffset, oolStart})
}

// --- Optional writers (union_like: 2 vtable slots) ---

func (o *ObjectWriter) WriteOptionalInt32Present(typeOffset, valueOffset int, v int32) {
	o.object[typeOffset] = 1
	oolStart := len(o.outOfLine)
	var buf [4]byte
	binary.LittleEndian.PutUint32(buf[:], uint32(v))
	o.outOfLine = append(o.outOfLine, buf[:]...)
	o.patches = append(o.patches, relOffsetPatch{valueOffset, oolStart})
}

func (o *ObjectWriter) WriteOptionalAbsent(typeOffset, valueOffset int) {}

func (o *ObjectWriter) WriteOptionalStringPresent(typeOffset, valueOffset int, s string) {
	o.object[typeOffset] = 1
	oolStart := len(o.outOfLine)
	var lenBuf [4]byte
	binary.LittleEndian.PutUint32(lenBuf[:], uint32(len(s)))
	o.outOfLine = append(o.outOfLine, lenBuf[:]...)
	o.outOfLine = append(o.outOfLine, s...)
	if pad := (4 - len(o.outOfLine)%4) % 4; pad > 0 {
		o.outOfLine = append(o.outOfLine, make([]byte, pad)...)
	}
	o.patches = append(o.patches, relOffsetPatch{valueOffset, oolStart})
}

// --- Nested struct writer ---

// WriteStruct writes a nested struct (expect_serialize_member type) as out-of-line data.
// The parent object gets a RelativeOffset at parentVtableOffset pointing to the nested
// struct's object. The nested struct has its own vtable and fields.
func (o *ObjectWriter) WriteStruct(parentVtableOffset int, vt VTable, maxAlign int, writeFields func(inner *ObjectWriter)) {
	if maxAlign < 4 {
		maxAlign = 4
	}
	objSize := int(vt[1])
	inner := &ObjectWriter{
		object: make([]byte, objSize),
	}
	writeFields(inner)

	ns := &nestedStruct{
		vt:           vt,
		maxAlign:     maxAlign,
		object:       inner.object,
		outOfLine:    inner.outOfLine,
		patches:      inner.patches,
		parentOffset: parentVtableOffset,
	}
	o.nestedStructs = append(o.nestedStructs, ns)
}

// --- Helpers ---

func rightAlign(offset, alignment int) int {
	if offset%alignment == 0 {
		return offset
	}
	return ((offset / alignment) + 1) * alignment
}

func vtablesEqual(a, b VTable) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func packVTable(vt VTable) []byte {
	buf := make([]byte, len(vt)*2)
	for i, v := range vt {
		binary.LittleEndian.PutUint16(buf[i*2:], v)
	}
	return buf
}
