package wire

import (
	"encoding/binary"
	"fmt"
	"math"
	"sort"
	"sync"
)

// MessageTemplate pre-computes everything that is static per message type.
// Created once at init time from a VTableClosure. Eliminates all per-message
// vtable allocation, dedup, sorting, packing, and offset lookup.
//
// Use with Writer.WriteMessagePacked for zero-vtable-alloc serialization.
type MessageTemplate struct {
	fileID        uint32
	msgVTable     VTable
	maxFieldAlign int
	msgObjSize    int

	// Pre-packed vtable bytes — copied directly into the output buffer.
	packedVTables []byte

	// Pre-computed vtable byte offsets. Key = vtable content hash.
	// O(1) lookup instead of linear scan.
	vtOffsets map[vtableKey]int

	// objectPool recycles ObjectWriter backing storage.
	objectPool sync.Pool
}

// NewMessageTemplate pre-computes a MessageTemplate from a vtable closure.
// The closure must include ALL vtables transitively reachable from the message
// (from C++ get_vtableset). Call once at package init.
func NewMessageTemplate(fileID uint32, msgVTable VTable, maxFieldAlign int, closure []VTable) *MessageTemplate {
	if maxFieldAlign < 4 {
		maxFieldAlign = 4
	}

	// Pack vtables exactly once.
	set := newVTableSet()
	set.ordered = true
	for _, vt := range closure {
		set.addOrdered(vt)
	}
	packed := set.pack()

	// Build O(1) offset lookup using full vtable content as key.
	offsets := make(map[vtableKey]int, len(set.entries))
	for _, e := range set.entries {
		offsets[makeVTableKey(e.vt)] = e.offset
	}

	return &MessageTemplate{
		fileID:        fileID,
		msgVTable:     msgVTable,
		maxFieldAlign: maxFieldAlign,
		msgObjSize:    int(msgVTable[1]),
		packedVTables: packed,
		vtOffsets:     offsets,
	}
}

type pooledWriter struct {
	obj   ObjectWriter
	arena writerArena
}

func (t *MessageTemplate) getPooledWriter() *pooledWriter {
	if v := t.objectPool.Get(); v != nil {
		pw := v.(*pooledWriter)
		pw.arena.reset()
		pw.obj = ObjectWriter{
			object:        pw.obj.object[:t.msgObjSize],
			outOfLine:     pw.obj.outOfLine[:0],
			patches:       pw.obj.patches[:0],
			nestedStructs: pw.obj.nestedStructs[:0],
			arena:         &pw.arena,
		}
		clear(pw.obj.object)
		return pw
	}
	pw := &pooledWriter{}
	pw.obj = ObjectWriter{
		object:        make([]byte, t.msgObjSize),
		outOfLine:     make([]byte, 0, 256),
		patches:       make([]relOffsetPatch, 0, 8),
		nestedStructs: make([]*nestedStruct, 0, 4),
		arena:         &pw.arena,
	}
	return pw
}

// vtableKey uniquely identifies a vtable by its full content.
type vtableKey string

func makeVTableKey(vt VTable) vtableKey {
	b := make([]byte, len(vt)*2)
	for i, v := range vt {
		binary.LittleEndian.PutUint16(b[i*2:], v)
	}
	return vtableKey(b)
}

// vtableOffset returns the byte offset of vt within the pre-packed vtable data.
func (t *MessageTemplate) vtableOffset(vt VTable) int {
	key := makeVTableKey(vt)
	if off, ok := t.vtOffsets[key]; ok {
		return off
	}
	panic(fmt.Sprintf("vtable %v not in template closure", vt))
}

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
	return w.WriteMessageWithVTables(fileID, msgVTable, maxFieldAlign, nil, writeFields)
}

// WriteMessagePacked is the fast path using a pre-computed MessageTemplate.
// Zero vtable allocations — uses pre-packed bytes and O(1) offset lookup.
func (w *Writer) WriteMessagePacked(t *MessageTemplate, writeFields func(obj *ObjectWriter)) []byte {
	fakeRootVTable := VTable{6, 8, 4}

	pw := t.getPooledWriter()
	obj := &pw.obj
	writeFields(obj)

	vtableBytes := t.packedVTables
	vtableSize := len(vtableBytes)

	// OOL data.
	oolSize := len(obj.outOfLine)
	endOff := 0
	if oolSize > 0 {
		endOff = rightAlign(oolSize, 4)
	}

	// Nested structs.
	nestedEndOff := endOff
	for i := len(obj.nestedStructs) - 1; i >= 0; i-- {
		nestedEndOff = obj.nestedStructs[i].computeEndOffset(nestedEndOff)
	}

	// Message object.
	bodySize := t.msgObjSize - 4
	msgObjEnd := rightAlign(nestedEndOff+bodySize, t.maxFieldAlign) + 4

	// FakeRoot: vtable {6,8,4}, body = 4 bytes.
	fakeRootEnd := rightAlign(msgObjEnd+4, 4) + 4

	// VTable region.
	vtableEnd := fakeRootEnd + vtableSize

	// Footer (8 bytes, aligned to 8).
	totalSize := rightAlign(vtableEnd+8, 8)

	// Byte positions.
	oolPos := totalSize - endOff
	msgObjPos := totalSize - msgObjEnd
	fakeRootPos := totalSize - fakeRootEnd
	vtablePos := totalSize - vtableEnd

	// Build buffer.
	if cap(w.buf) >= totalSize {
		w.buf = w.buf[:totalSize]
		clear(w.buf)
	} else {
		w.buf = make([]byte, totalSize)
	}

	// OOL data.
	if oolSize > 0 {
		copy(w.buf[oolPos:], obj.outOfLine)
	}

	// Nested structs — use template's O(1) offset lookup.
	nestedPos := oolPos
	for i := len(obj.nestedStructs) - 1; i >= 0; i-- {
		nestedPos = obj.nestedStructs[i].writeIntoPacked(w.buf, nestedPos, vtablePos, t)
	}

	// Message object.
	msgVTAddr := vtablePos + t.vtableOffset(t.msgVTable)
	binary.LittleEndian.PutUint32(obj.object[0:4], uint32(int32(msgObjPos-msgVTAddr)))
	for _, p := range obj.patches {
		fieldAddr := msgObjPos + p.objectOffset
		targetAddr := oolPos + p.oolOffset
		binary.LittleEndian.PutUint32(obj.object[p.objectOffset:], uint32(targetAddr-fieldAddr))
	}
	for _, ns := range obj.nestedStructs {
		fieldAddr := msgObjPos + ns.parentOffset
		binary.LittleEndian.PutUint32(obj.object[ns.parentOffset:], uint32(ns.byteAddr-fieldAddr))
	}
	copy(w.buf[msgObjPos:], obj.object)

	// FakeRoot.
	fakeRootVTAddr := vtablePos + t.vtableOffset(fakeRootVTable)
	binary.LittleEndian.PutUint32(w.buf[fakeRootPos:], uint32(int32(fakeRootPos-fakeRootVTAddr)))
	binary.LittleEndian.PutUint32(w.buf[fakeRootPos+4:], uint32(msgObjPos-(fakeRootPos+4)))

	// VTables.
	copy(w.buf[vtablePos:], vtableBytes)

	// Footer.
	binary.LittleEndian.PutUint32(w.buf[0:], uint32(fakeRootPos))
	binary.LittleEndian.PutUint32(w.buf[4:], t.fileID)

	t.objectPool.Put(pw)
	return w.buf
}

// WriteMessageWithVTables is like WriteMessage but includes additional vtables
// in the vtable set. FDB's C++ get_vtableset includes vtables for ALL types
// transitively reachable from the message type, even inside absent Optional
// fields. Our Writer only discovers vtables for explicitly written nested
// structs. Pass extraVTables to include vtables for types that aren't written
// but must be present in the vtable set.
func (w *Writer) WriteMessageWithVTables(fileID uint32, msgVTable VTable, maxFieldAlign int, extraVTables []VTable, writeFields func(obj *ObjectWriter)) []byte {
	if maxFieldAlign < 4 {
		maxFieldAlign = 4
	}

	fakeRootVTable := VTable{6, 8, 4}

	msgObjSize := int(msgVTable[1])
	obj := &ObjectWriter{
		object: make([]byte, msgObjSize),
	}
	writeFields(obj)

	// Collect all unique vtables. If extraVTables is a complete closure (from C++
	// get_vtableset_impl), use it directly to preserve C++ vtable ordering.
	// The C++ vtable order is determined by std::set<const VTable*> (pointer order),
	// which we can't reproduce. The closure from the test vector captures the
	// exact order the server expects.
	vtableSet := newVTableSet()
	if len(extraVTables) > 0 {
		// Use the closure order — it already contains ALL vtables.
		vtableSet.ordered = true
		for _, vt := range extraVTables {
			vtableSet.addOrdered(vt)
		}
		// Also add any nested struct vtables not in the closure.
		for _, ns := range obj.nestedStructs {
			ns.collectVTables(vtableSet)
		}
	} else {
		vtableSet.add(fakeRootVTable)
		vtableSet.add(msgVTable)
		for _, ns := range obj.nestedStructs {
			ns.collectVTables(vtableSet)
		}
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

// WriteRootObject writes a message where the root object IS the top-level object
// (no FakeRoot wrapper). Used for union_like_traits types like ErrorOr where
// FakeRoot flattens the union into the root.
func (w *Writer) WriteRootObject(fileID uint32, rootVTable VTable, maxFieldAlign int, extraVTables []VTable, writeFields func(obj *ObjectWriter)) []byte {
	if maxFieldAlign < 4 {
		maxFieldAlign = 4
	}

	objSize := int(rootVTable[1])
	obj := &ObjectWriter{
		object: make([]byte, objSize),
	}
	writeFields(obj)

	vtableSet := newVTableSet()
	if len(extraVTables) > 0 {
		vtableSet.ordered = true
		for _, vt := range extraVTables {
			vtableSet.addOrdered(vt)
		}
		for _, ns := range obj.nestedStructs {
			ns.collectVTables(vtableSet)
		}
	} else {
		vtableSet.add(rootVTable)
		for _, ns := range obj.nestedStructs {
			ns.collectVTables(vtableSet)
		}
	}
	vtableBytes := vtableSet.pack()
	vtableSize := len(vtableBytes)

	// OOL data
	oolSize := len(obj.outOfLine)
	endOff := 0
	if oolSize > 0 {
		endOff = rightAlign(oolSize, 4)
	}

	// Nested structs
	nestedEndOff := endOff
	for i := len(obj.nestedStructs) - 1; i >= 0; i-- {
		nestedEndOff = obj.nestedStructs[i].computeEndOffset(nestedEndOff)
	}

	// Root object (no FakeRoot — this IS the root)
	bodySize := objSize - 4
	rootObjEnd := rightAlign(nestedEndOff+bodySize, maxFieldAlign) + 4

	// VTable region
	vtableEnd := rootObjEnd + vtableSize

	// Root footer (8 bytes, aligned to 8)
	footerTotal := rightAlign(vtableEnd+8, 8)
	totalSize := footerTotal

	// Byte positions
	oolPos := totalSize - endOff
	rootObjPos := totalSize - rootObjEnd
	vtablePos := totalSize - vtableEnd

	// Build buffer
	if cap(w.buf) >= totalSize {
		w.buf = w.buf[:totalSize]
		clear(w.buf)
	} else {
		w.buf = make([]byte, totalSize)
	}

	// OOL data
	if oolSize > 0 {
		copy(w.buf[oolPos:], obj.outOfLine)
	}

	// Nested structs
	nestedPos := oolPos
	for i := len(obj.nestedStructs) - 1; i >= 0; i-- {
		nestedPos = obj.nestedStructs[i].writeInto(w.buf, nestedPos, vtablePos, vtableSet)
	}

	// Root object
	rootVTableAddr := vtablePos + vtableSet.offset(rootVTable)
	binary.LittleEndian.PutUint32(obj.object[0:4], uint32(int32(rootObjPos-rootVTableAddr)))
	for _, p := range obj.patches {
		fieldAddr := rootObjPos + p.objectOffset
		targetAddr := oolPos + p.oolOffset
		binary.LittleEndian.PutUint32(obj.object[p.objectOffset:], uint32(targetAddr-fieldAddr))
	}
	for _, ns := range obj.nestedStructs {
		fieldAddr := rootObjPos + ns.parentOffset
		binary.LittleEndian.PutUint32(obj.object[ns.parentOffset:], uint32(ns.byteAddr-fieldAddr))
	}
	copy(w.buf[rootObjPos:], obj.object)

	// VTables
	copy(w.buf[vtablePos:], vtableBytes)

	// Root footer
	binary.LittleEndian.PutUint32(w.buf[0:], uint32(rootObjPos))
	binary.LittleEndian.PutUint32(w.buf[4:], fileID)

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
	ordered bool // true when entries are in C++ closure order (don't re-sort)
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

// addOrdered adds a vtable preserving insertion order, using tolerant
// matching for dedup (handles trailing-zero differences between our vtables
// and C++ closure vtables).
func (s *vTableSet) addOrdered(vt VTable) {
	for _, e := range s.entries {
		if vtablesEqual(e.vt, vt) {
			return
		}
		// Tolerant match: same obj_size + same field offsets.
		if len(e.vt) >= 2 && len(vt) >= 2 && e.vt[1] == vt[1] {
			match := true
			maxLen := len(e.vt)
			if len(vt) > maxLen {
				maxLen = len(vt)
			}
			for i := 2; i < maxLen; i++ {
				va, vb := uint16(0), uint16(0)
				if i < len(e.vt) {
					va = e.vt[i]
				}
				if i < len(vt) {
					vb = vt[i]
				}
				if va != vb {
					match = false
					break
				}
			}
			if match {
				return
			}
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
	// Tolerant match: C++ vtables may trim trailing zeros, changing vt_size.
	// Two vtables match if they have the same obj_size and field offsets.
	for _, e := range s.entries {
		if len(e.vt) >= 2 && len(vt) >= 2 && e.vt[1] == vt[1] {
			match := true
			maxLen := len(e.vt)
			if len(vt) > maxLen {
				maxLen = len(vt)
			}
			for i := 2; i < maxLen; i++ {
				va, vb := uint16(0), uint16(0)
				if i < len(e.vt) {
					va = e.vt[i]
				}
				if i < len(vt) {
					vb = vt[i]
				}
				if va != vb {
					match = false
					break
				}
			}
			if match {
				return e.offset
			}
		}
	}
	panic(fmt.Sprintf("vtable not found in set: %v (set has %d entries)", vt, len(s.entries)))
}

func (s *vTableSet) pack() []byte {
	if s.ordered {
		// Closure order from C++ test vector — don't re-sort.
		buf := make([]byte, s.total)
		for _, e := range s.entries {
			for i, v := range e.vt {
				binary.LittleEndian.PutUint16(buf[e.offset+i*2:], v)
			}
		}
		return buf
	}
	// Sort entries by vtable content (descending by packed bytes) for
	// deterministic output. FDB's C++ uses std::set<const VTable*> which
	// sorts by pointer value — non-deterministic. We sort by content instead,
	// descending so that larger vtables (message-specific) come before smaller
	// ones (FakeRoot [6,8,4]). This matches the typical C++ layout.
	sort.SliceStable(s.entries, func(i, j int) bool {
		a, b := s.entries[i].vt, s.entries[j].vt
		minLen := len(a)
		if len(b) < minLen {
			minLen = len(b)
		}
		for k := 0; k < minLen; k++ {
			if a[k] != b[k] {
				return a[k] > b[k] // descending
			}
		}
		return len(a) > len(b)
	})
	// Recompute offsets after sorting.
	s.total = 0
	for i := range s.entries {
		s.entries[i].offset = s.total
		s.total += len(s.entries[i].vt) * 2
	}
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
	oolEndOff    int              // end-offset at end of OOL region (for writeInto)
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
	ns.oolEndOff = endOff // save for writeInto — OOL occupies [startEndOff, oolEndOff)

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

	objByteAddr := totalSize - ns.endOffset
	ns.byteAddr = objByteAddr

	// Write vtable soffset.
	vtOff := vtablePos + vtSet.offset(ns.vt)
	binary.LittleEndian.PutUint32(ns.object[0:4], uint32(int32(objByteAddr-vtOff)))

	// OOL byte position computed from saved oolEndOff (set during computeEndOffset).
	oolByteAddr := totalSize - ns.oolEndOff

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

// writeIntoPacked is the fast path using MessageTemplate's O(1) vtable lookup.
func (ns *nestedStruct) writeIntoPacked(buf []byte, startByteAddr, vtablePos int, t *MessageTemplate) int {
	totalSize := len(buf)

	objByteAddr := totalSize - ns.endOffset
	ns.byteAddr = objByteAddr

	vtOff := vtablePos + t.vtableOffset(ns.vt)
	binary.LittleEndian.PutUint32(ns.object[0:4], uint32(int32(objByteAddr-vtOff)))

	oolByteAddr := totalSize - ns.oolEndOff

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
	arena         *writerArena // shared arena for nested allocations (nil = use make)
}

// writerArena is a bump allocator for nested ObjectWriter and nestedStruct allocations.
// Pre-allocated once per pool entry, eliminates per-WriteStruct heap allocations.
type writerArena struct {
	objWriters    [8]ObjectWriter // inline storage for nested ObjectWriters
	nestedStructs [8]nestedStruct // inline storage for nestedStruct values
	objBuf        [512]byte       // inline storage for nested object bytes
	objWriterN    int
	nestedStructN int
	objBufPos     int
}

func (a *writerArena) reset() {
	a.objWriterN = 0
	a.nestedStructN = 0
	a.objBufPos = 0
}

func (a *writerArena) allocObjectWriter(objSize int) *ObjectWriter {
	if a.objWriterN < len(a.objWriters) && a.objBufPos+objSize <= len(a.objBuf) {
		ow := &a.objWriters[a.objWriterN]
		a.objWriterN++
		*ow = ObjectWriter{
			object: a.objBuf[a.objBufPos : a.objBufPos+objSize : a.objBufPos+objSize],
			arena:  a,
		}
		// Zero the object bytes.
		clear(ow.object)
		a.objBufPos += objSize
		return ow
	}
	// Fallback: heap allocate.
	return &ObjectWriter{object: make([]byte, objSize), arena: a}
}

func (a *writerArena) allocNestedStruct() *nestedStruct {
	if a.nestedStructN < len(a.nestedStructs) {
		ns := &a.nestedStructs[a.nestedStructN]
		a.nestedStructN++
		*ns = nestedStruct{}
		return ns
	}
	return &nestedStruct{}
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

// UIDFromParts constructs a [16]byte UID from two uint64 halves (little-endian).
func UIDFromParts(first, second uint64) [16]byte {
	var uid [16]byte
	binary.LittleEndian.PutUint64(uid[:8], first)
	binary.LittleEndian.PutUint64(uid[8:], second)
	return uid
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

// WriteRawOOL writes raw out-of-line data without a length prefix.
// Use this for VectorRef data that already includes the element count header.
func (o *ObjectWriter) WriteRawOOL(vtableOffset int, data []byte) {
	oolStart := len(o.outOfLine)
	o.outOfLine = append(o.outOfLine, data...)
	if pad := (4 - len(o.outOfLine)%4) % 4; pad > 0 {
		o.outOfLine = append(o.outOfLine, make([]byte, pad)...)
	}
	o.patches = append(o.patches, relOffsetPatch{vtableOffset, oolStart})
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

	var inner *ObjectWriter
	if o.arena != nil {
		inner = o.arena.allocObjectWriter(objSize)
	} else {
		inner = &ObjectWriter{object: make([]byte, objSize)}
	}
	writeFields(inner)

	var ns *nestedStruct
	if o.arena != nil {
		ns = o.arena.allocNestedStruct()
	} else {
		ns = &nestedStruct{}
	}
	*ns = nestedStruct{
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

// vtablesMatch checks if two vtables represent the same type, tolerating
// C++'s trailing-zero trimming (vt_size differs but field offsets match).
func vtablesMatch(a, b VTable) bool {
	if len(a) < 2 || len(b) < 2 {
		return vtablesEqual(a, b)
	}
	if a[1] != b[1] { // obj_size must match
		return false
	}
	maxLen := len(a)
	if len(b) > maxLen {
		maxLen = len(b)
	}
	for i := 2; i < maxLen; i++ {
		va, vb := uint16(0), uint16(0)
		if i < len(a) {
			va = a[i]
		}
		if i < len(b) {
			vb = b[i]
		}
		if va != vb {
			return false
		}
	}
	return true
}

// MarshalStructBlob produces a standalone FlatBuffers object blob suitable for
// embedding in a VectorRef<serialize_member>. Layout:
//
//	[vtable bytes][padding to 4][soffset(4) + field data][OOL data]
//
// The soffset at the start of the object points back to the vtable.
// OOL RelativeOffsets are patched to point forward into the OOL region.
func MarshalStructBlob(vt VTable, fn func(*ObjectWriter)) []byte {
	objSize := int(vt[1])
	obj := &ObjectWriter{
		object: make([]byte, objSize),
	}
	fn(obj)

	vtBytes := len(vt) * 2
	vtPos := 0
	objPos := (vtBytes + 3) &^ 3 // align to 4
	objEnd := objPos + objSize

	// OOL data goes after object (forward from object)
	oolPos := (objEnd + 3) &^ 3
	oolSize := len(obj.outOfLine)
	total := oolPos + oolSize
	total = (total + 3) &^ 3

	buf := make([]byte, total)

	// Write vtable
	for i, v := range vt {
		binary.LittleEndian.PutUint16(buf[vtPos+i*2:], v)
	}

	// Write soffset (distance from object to vtable)
	binary.LittleEndian.PutUint32(buf[objPos:], uint32(int32(objPos-vtPos)))

	// Write fields (skip soffset at [0:4])
	copy(buf[objPos+4:], obj.object[4:])

	// Write OOL and patch RelativeOffsets
	if oolSize > 0 {
		copy(buf[oolPos:], obj.outOfLine)
		for _, p := range obj.patches {
			fieldAddr := objPos + p.objectOffset
			targetAddr := oolPos + p.oolOffset
			binary.LittleEndian.PutUint32(buf[fieldAddr:], uint32(targetAddr-fieldAddr))
		}
	}

	return buf
}

// PackVectorOfStructBlobs packs pre-marshaled struct blobs into VectorRef<serialize_member> format:
//
//	[count(4)][RelOff_0(4)]...[RelOff_N-1(4)][blob_0][blob_1]...
//
// Each RelOff points from its own position to the blob's object (after the vtable).
func PackVectorOfStructBlobs(blobs [][]byte) []byte {
	n := len(blobs)
	headerSize := 4 + n*4

	pos := headerSize
	positions := make([]int, n)
	for i, blob := range blobs {
		pos = (pos + 3) &^ 3
		positions[i] = pos
		pos += len(blob)
	}
	total := (pos + 3) &^ 3

	buf := make([]byte, total)
	binary.LittleEndian.PutUint32(buf, uint32(n))

	for i, blob := range blobs {
		copy(buf[positions[i]:], blob)

		// RelOff points to the blob's object (after vtable + padding).
		vtSize := int(binary.LittleEndian.Uint16(blob[0:]))
		objPosInBlob := (vtSize + 3) &^ 3

		relOffPos := 4 + i*4
		targetInBuf := positions[i] + objPosInBlob
		binary.LittleEndian.PutUint32(buf[relOffPos:], uint32(targetInBuf-relOffPos))
	}

	return buf
}

func packVTable(vt VTable) []byte {
	buf := make([]byte, len(vt)*2)
	for i, v := range vt {
		binary.LittleEndian.PutUint16(buf[i*2:], v)
	}
	return buf
}
