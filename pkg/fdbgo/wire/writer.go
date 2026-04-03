package wire

import (
	"encoding/binary"
	"fmt"
)

// MessageTemplate pre-computes everything that is static per message type.
// Created once at init time from a VTableClosure. Used by DirectWriter
// for vtable offset lookup and pre-packed vtable bytes.
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
	for _, vt := range closure {
		set.add(vt)
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

// UIDFromParts constructs a [16]byte UID from two uint64 halves (little-endian).
func UIDFromParts(first, second uint64) [16]byte {
	var uid [16]byte
	binary.LittleEndian.PutUint64(uid[:8], first)
	binary.LittleEndian.PutUint64(uid[8:], second)
	return uid
}

// rightAlign rounds v up to the next multiple of alignment.
func rightAlign(offset, alignment int) int {
	return (offset + alignment - 1) &^ (alignment - 1)
}

// --- VTable set (used by NewMessageTemplate at init time) ---

type vtableSetEntry struct {
	vt     VTable
	offset int
}

type vTableSet struct {
	entries []vtableSetEntry
	seen    map[vtableKey]bool
}

func newVTableSet() *vTableSet {
	return &vTableSet{seen: make(map[vtableKey]bool)}
}

func (s *vTableSet) add(vt VTable) {
	key := makeVTableKey(vt)
	if s.seen[key] {
		return
	}
	s.seen[key] = true
	s.entries = append(s.entries, vtableSetEntry{vt: vt})
}

// pack serializes all vtables into a contiguous byte buffer.
// Vtables are written in insertion order (matching C++ closure order).
func (s *vTableSet) pack() []byte {
	totalBytes := 0
	for _, e := range s.entries {
		totalBytes += len(e.vt) * 2
	}

	buf := make([]byte, totalBytes)
	pos := 0
	for i := range s.entries {
		for _, v := range s.entries[i].vt {
			binary.LittleEndian.PutUint16(buf[pos:], v)
			pos += 2
		}
		s.entries[i].offset = pos - len(s.entries[i].vt)*2
	}

	return buf
}

// Exported VTableSet for testing.
type VTableSet = vTableSet

func NewVTableSetForTest() *VTableSet { return newVTableSet() }
func (s *VTableSet) Add(vt VTable)    { s.add(vt) }
func (s *VTableSet) Pack() []byte     { return s.pack() }

func (s *VTableSet) GetOffset(vt VTable) int {
	key := makeVTableKey(vt)
	for _, e := range s.entries {
		if makeVTableKey(e.vt) == key {
			return e.offset
		}
	}
	return -1
}

// PackedVTables returns the pre-packed vtable bytes.
func (t *MessageTemplate) PackedVTables() []byte { return t.packedVTables }

// VTableOffset returns the byte offset of vt within packed vtable data (exported for testing).
func (t *MessageTemplate) VTableOffset(vt VTable) int { return t.vtableOffset(vt) }
