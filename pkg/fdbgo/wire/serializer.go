// serializer.go — Mechanical port of C++ FDB FlatBuffers serialization.
//
// Every type and function references the C++ original by name and location:
//   Source: flow/include/flow/flat_buffers.h (FDB 7.3.75)
//
// The C++ serializer uses a two-pass approach:
//   Pass 1 (PrecomputeSize): compute total buffer size, record object positions
//   Pass 2 (WriteToBuffer): write data into pre-allocated buffer
//
// Both passes walk the same field tree. Generated code calls these primitives.

package wire

import (
	"encoding/binary"
	"sync"
)

// RightAlign — C++ flat_buffers.h:58
func RightAlign(offset, alignment int) int {
	if offset%alignment == 0 {
		return offset
	}
	return ((offset / alignment) + 1) * alignment
}

// PrecomputeSize — C++ flat_buffers.h:502
// Pass 1: computes total buffer size by accumulating field sizes.
// All offsets are measured from the END of the buffer.
type PrecomputeSize struct {
	CurrentBufferSize int
	WriteToOffsets    []int // records final position for each getMessageWriter call

	// C++ flat_buffers.h:551 — empty vector sentinel optimization.
	// Only the first empty dynamic_size field allocates 4 bytes;
	// subsequent ones re-use the same offset.
	EmptyVectorOffset int // -1 = not yet allocated
}

var precomputeSizePool = sync.Pool{
	New: func() any {
		return &PrecomputeSize{
			WriteToOffsets: make([]int, 0, 32),
		}
	},
}

func NewPrecomputeSize() *PrecomputeSize {
	ps := precomputeSizePool.Get().(*PrecomputeSize)
	ps.CurrentBufferSize = 0
	ps.WriteToOffsets = ps.WriteToOffsets[:0]
	ps.EmptyVectorOffset = -1
	return ps
}

// ReleasePrecomputeSize returns a PrecomputeSize to the pool for reuse.
func ReleasePrecomputeSize(ps *PrecomputeSize) {
	precomputeSizePool.Put(ps)
}

// Write — C++ PrecomputeSize::write (flat_buffers.h:512)
func (ps *PrecomputeSize) Write(offset int) {
	if offset > ps.CurrentBufferSize {
		ps.CurrentBufferSize = offset
	}
}

// VisitDynamicSize — C++ PrecomputeSize::visitDynamicSize (flat_buffers.h:515)
// For bytes/string fields. Returns true if the field is empty and reuses
// the existing empty vector sentinel (caller should skip writing).
func (ps *PrecomputeSize) VisitDynamicSize(size int) bool {
	if size == 0 && ps.EmptyVectorOffset != -1 {
		return true // re-use existing empty vector
	}
	start := RightAlign(ps.CurrentBufferSize+size+4, 4)
	if start > ps.CurrentBufferSize {
		ps.CurrentBufferSize = start
	}
	if size == 0 {
		ps.EmptyVectorOffset = ps.CurrentBufferSize
	}
	return false
}

// SizeNoop — C++ PrecomputeSize::Noop (flat_buffers.h:527)
// Represents a message writer during the size computation pass.
type SizeNoop struct {
	Size         int
	WriteToIndex int
}

// GetMessageWriter — C++ PrecomputeSize::getMessageWriter (flat_buffers.h:538)
func (ps *PrecomputeSize) GetMessageWriter(size int) SizeNoop {
	idx := len(ps.WriteToOffsets)
	ps.WriteToOffsets = append(ps.WriteToOffsets, 0)
	return SizeNoop{Size: size, WriteToIndex: idx}
}

// WriteTo — C++ PrecomputeSize::Noop::writeTo(writer) (flat_buffers.h:533)
// Default: places the object at current_buffer_size + size.
func (n SizeNoop) WriteTo(ps *PrecomputeSize) {
	n.WriteToAt(ps, ps.CurrentBufferSize+n.Size)
}

// WriteToAt — C++ PrecomputeSize::Noop::writeTo(writer, offset) (flat_buffers.h:528)
// Places the object at a specific offset.
func (n SizeNoop) WriteToAt(ps *PrecomputeSize, offset int) {
	ps.Write(offset)
	ps.WriteToOffsets[n.WriteToIndex] = offset
}

// SaveObjectSize — C++ SaveVisitorLambda::operator() lines 972-977
// Computes the aligned position for an object with the given vtable.
// objSize = vtable[1], maxAlign = max(4, fb_align<Members>...)
func (ps *PrecomputeSize) SaveObjectSize(objSize int, maxAlign int) int {
	padding := 0
	start := RightAlign(ps.CurrentBufferSize+objSize-4, maxAlign)
	padding = start - (ps.CurrentBufferSize + objSize - 4)
	_ = padding
	return start + 4
}

// WriteToBuffer — C++ flat_buffers.h:569
// Pass 2: writes data into a pre-allocated buffer.
// All offsets are measured from the END of the buffer (written right-to-left).
type WriteToBuffer struct {
	Buf               []byte
	BufferLength      int // = len(Buf)
	VTableStart       int // byte offset of vtable region from end
	CurrentBufferSize int
	WriteToOffsets    []int // from PrecomputeSize pass
	WriteToIdx        int   // current position in WriteToOffsets

	// C++ flat_buffers.h:637
	EmptyVectorOffset int // -1 = not yet allocated
}

var writeToBufferPool = sync.Pool{
	New: func() any {
		return &WriteToBuffer{}
	},
}

func NewWriteToBuffer(buf []byte, vtableStart int, offsets []int) *WriteToBuffer {
	wb := writeToBufferPool.Get().(*WriteToBuffer)
	wb.Buf = buf
	wb.BufferLength = len(buf)
	wb.VTableStart = vtableStart
	wb.CurrentBufferSize = 0
	wb.WriteToOffsets = offsets
	wb.WriteToIdx = 0
	wb.EmptyVectorOffset = -1
	return wb
}

// ReleaseWriteToBuffer returns a WriteToBuffer to the pool for reuse.
func ReleaseWriteToBuffer(wb *WriteToBuffer) {
	wb.Buf = nil // don't hold reference to large buffer
	wb.WriteToOffsets = nil
	writeToBufferPool.Put(wb)
}

// Write — C++ WriteToBuffer::write (flat_buffers.h:569)
// Writes src at position measured from end of buffer.
func (wb *WriteToBuffer) Write(src []byte, offset int) {
	pos := wb.BufferLength - offset
	copy(wb.Buf[pos:], src)
	if offset > wb.CurrentBufferSize {
		wb.CurrentBufferSize = offset
	}
}

// WriteUint32 writes a uint32 at position measured from end of buffer.
func (wb *WriteToBuffer) WriteUint32(val uint32, offset int) {
	pos := wb.BufferLength - offset
	binary.LittleEndian.PutUint32(wb.Buf[pos:], val)
	if offset > wb.CurrentBufferSize {
		wb.CurrentBufferSize = offset
	}
}

// WriteUint64 writes a uint64 at position measured from end of buffer — the 8-byte
// sibling of WriteUint32, for a bare out-of-line scalar behind a union RelativeOffset
// (C++ SaveAlternative non-indirection arm, flat_buffers.h:848).
func (wb *WriteToBuffer) WriteUint64(val uint64, offset int) {
	pos := wb.BufferLength - offset
	binary.LittleEndian.PutUint64(wb.Buf[pos:], val)
	if offset > wb.CurrentBufferSize {
		wb.CurrentBufferSize = offset
	}
}

// WriteZeros writes zero bytes at position measured from end of buffer.
func (wb *WriteToBuffer) WriteZeros(offset, length int) {
	pos := wb.BufferLength - offset
	for i := 0; i < length && pos+i < len(wb.Buf); i++ {
		wb.Buf[pos+i] = 0
	}
}

// BufferMessageWriter — C++ WriteToBuffer::MessageWriter (flat_buffers.h:583)
type BufferMessageWriter struct {
	WB            *WriteToBuffer
	FinalLocation int // from writeToOffsets
	Size          int
}

// GetMessageWriter — C++ WriteToBuffer::getMessageWriter (flat_buffers.h:603)
func (wb *WriteToBuffer) GetMessageWriter(size int, zeroed bool) BufferMessageWriter {
	finalLoc := wb.WriteToOffsets[wb.WriteToIdx]
	wb.WriteToIdx++
	m := BufferMessageWriter{WB: wb, FinalLocation: finalLoc, Size: size}
	if zeroed {
		// C++ memset(&buffer[buffer_length - m.finalLocation], 0, size)
		pos := wb.BufferLength - m.FinalLocation
		for i := 0; i < size && pos+i < len(wb.Buf); i++ {
			wb.Buf[pos+i] = 0
		}
	}
	return m
}

// WriteScalar — C++ MessageWriter::write for non-RelativeOffset types (flat_buffers.h:591)
func (mw BufferMessageWriter) WriteScalar(src []byte, offset int) {
	pos := mw.WB.BufferLength - (mw.FinalLocation - offset)
	copy(mw.WB.Buf[pos:], src)
}

// WriteRelativeOffset — C++ MessageWriter::write for RelativeOffset (flat_buffers.h:586)
// Converts the end-of-buffer relative offset to a forward relative offset.
func (mw BufferMessageWriter) WriteRelativeOffset(reloff int, fieldOffset int) {
	// C++: uint32_t fixed_offset = finalLocation - offset - src->value;
	fixedOffset := uint32(mw.FinalLocation - fieldOffset - reloff)
	pos := mw.WB.BufferLength - (mw.FinalLocation - fieldOffset)
	binary.LittleEndian.PutUint32(mw.WB.Buf[pos:], fixedOffset)
}

// WriteTo — C++ MessageWriter::writeTo(writer) (flat_buffers.h:594)
func (mw BufferMessageWriter) WriteTo() {
	mw.WB.CurrentBufferSize += mw.Size
}

// WriteToAt — C++ MessageWriter::writeTo(writer, offset) (flat_buffers.h:595)
func (mw BufferMessageWriter) WriteToAt(offset int) {
	if offset > mw.WB.CurrentBufferSize {
		mw.WB.CurrentBufferSize = offset
	}
}

// VisitDynamicSize — C++ WriteToBuffer::visitDynamicSize (flat_buffers.h:615)
// Writes a dynamic-size field (bytes/string) into the buffer.
// Returns the end-of-buffer offset for the RelativeOffset, or -1 if reused empty.
func (wb *WriteToBuffer) VisitDynamicSize(data []byte) (int, bool) {
	size := len(data)
	if size == 0 && wb.EmptyVectorOffset != -1 {
		return wb.EmptyVectorOffset, true
	}
	padding := 0
	start := RightAlign(wb.CurrentBufferSize+size+4, 4)
	padding = start - (wb.CurrentBufferSize + size + 4)
	// C++: write(&size, start, 4)
	wb.WriteUint32(uint32(size), start)
	// C++: dynamic_size_traits<T>::save(&buffer[buffer_length - start + 4], t)
	start -= 4
	pos := wb.BufferLength - start
	copy(wb.Buf[pos:], data)
	// C++: memset(..., 0, padding)
	start -= size
	if padding > 0 {
		wb.WriteZeros(start, padding)
	}
	if size == 0 {
		wb.EmptyVectorOffset = wb.CurrentBufferSize
	}
	// Return the offset for the RelativeOffset (points to the length prefix)
	return wb.CurrentBufferSize, false
}

// FakeRootVTable is the vtable for the FakeRoot wrapper object.
// C++ uses fake_root<T> with vtable {6, 8, 4} — always the same.
var FakeRootVTable = VTable{6, 8, 4}
