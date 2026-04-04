package types

// keyrangeref_custom.go — Custom serialize logic for KeyRangeRef.
//
// C++ KeyRangeRef::serialize (FDBTypes.h:385-395) has an optimization:
// when begin + '\x00' == end (single-key range), it serializes as
// (end, empty) instead of (begin, end), saving len(begin)+4 bytes.
// On deserialization, begin is reconstructed from end[:-1].
//
// This matches the C++ behavior exactly. Without this, our serialized
// output is larger than C++ by 4+ bytes per single-key KeyRangeRef.

import (
	"encoding/binary"

	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/wire"
)

// equalsKeyAfter returns true if begin + '\x00' == end.
// C++ FDBTypes.h: equalsKeyAfter(begin, end)
func keyRangeEqualsKeyAfter(begin, end []byte) bool {
	if len(end) != len(begin)+1 {
		return false
	}
	if end[len(end)-1] != 0 {
		return false
	}
	for i := range begin {
		if begin[i] != end[i] {
			return false
		}
	}
	return true
}

// precomputeSize replaces the generated precomputeSize for KeyRangeRef.
func (m *KeyRangeRef) precomputeSize(ps *wire.PrecomputeSize) int {
	if keyRangeEqualsKeyAfter(m.Begin, m.End) {
		// C++: serializer(ar, end, empty) — only serialize end, empty for begin
		ps.VisitDynamicSize(len(m.End))
		ps.VisitDynamicSize(0) // empty
	} else {
		ps.VisitDynamicSize(len(m.Begin))
		ps.VisitDynamicSize(len(m.End))
	}
	{
		n := ps.GetMessageWriter(int(KeyRangeRefVTable[1]))
		n.WriteToAt(ps, wire.RightAlign(ps.CurrentBufferSize+int(KeyRangeRefVTable[1])-4, 4)+4)
	}
	return ps.CurrentBufferSize
}

// writeToBuffer replaces the generated writeToBuffer for KeyRangeRef.
func (m *KeyRangeRef) writeToBuffer(wb *wire.WriteToBuffer, vtableStart int, tmpl *wire.MessageTemplate) int {
	var beginOff, endOff int
	if keyRangeEqualsKeyAfter(m.Begin, m.End) {
		// C++: serialize(end, empty) — swap field order
		endOff, _ = wb.VisitDynamicSize(m.End)
		beginOff, _ = wb.VisitDynamicSize(nil) // empty
	} else {
		beginOff, _ = wb.VisitDynamicSize(m.Begin)
		endOff, _ = wb.VisitDynamicSize(m.End)
	}

	selfW := wb.GetMessageWriter(int(KeyRangeRefVTable[1]), true)
	selfStart := selfW.FinalLocation
	vt := KeyRangeRefVTable

	// soffset
	{
		soff := int32(vtableStart - tmpl.VTableOffset(KeyRangeRefVTable) - selfStart)
		var b [4]byte
		binary.LittleEndian.PutUint32(b[:], uint32(soff))
		selfW.WriteScalar(b[:], 0)
	}

	// RelativeOffsets — note: C++ swaps begin/end in the optimized case,
	// but the vtable slots stay the same (slot 0 = first arg, slot 1 = second arg).
	// When optimized: slot 0 = end, slot 1 = empty.
	// When normal: slot 0 = begin, slot 1 = end.
	selfW.WriteRelativeOffset(beginOff, int(vt[KeyRangeRefSlotBegin+2]))
	selfW.WriteRelativeOffset(endOff, int(vt[KeyRangeRefSlotEnd+2]))

	selfW.WriteToAt(selfStart)
	return selfStart
}
