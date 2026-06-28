package types

// keyrangeref_custom.go — Custom serialize logic for KeyRangeRef.
//
// C++ KeyRangeRef::serialize (FDBTypes.h:385-395) has an optimization:
// when begin + '\x00' == end (single-key range), it serializes as
// (end, empty) instead of (begin, end), saving len(begin)+4 bytes.
// On deserialization, begin is reconstructed from end[:-1].
//
// Both directions must be customized — without an override on
// UnmarshalFromReader, single-key-optimized payloads emitted by C++
// (or by our own writer) read back as Begin/End swapped. Caught by
// FuzzSplitRangeRequest_RoundTrip 2026-04-25.

import (
	"encoding/binary"

	"fdb.dev/pkg/fdbgo/wire"
)

// UnmarshalFromReader replaces the generated reader to invert the
// single-key serialize optimization. Slot 1 (End) being empty signals
// "first slot carries End; reconstruct Begin = End[:-1]".
//
// Matches C++ KeyRangeRef::serialize deserialization branch.
func (m *KeyRangeRef) UnmarshalFromReader(r *wire.Reader) {
	var first, second []byte
	if r.FieldPresent(KeyRangeRefSlotBegin) {
		first = r.ReadBytes(KeyRangeRefSlotBegin)
	}
	if r.FieldPresent(KeyRangeRefSlotEnd) {
		second = r.ReadBytes(KeyRangeRefSlotEnd)
	}
	if len(second) == 0 && len(first) > 0 {
		// Single-key optimization: writer emitted (end, empty).
		// Reconstruct: begin = end[:-1], end = first.
		// Note: m.Begin and m.End share the same backing array — same
		// zero-copy convention as the rest of wire deserialization.
		// Callers that mutate must copy first.
		m.Begin = first[:len(first)-1]
		m.End = first
	} else {
		m.Begin = first
		m.End = second
	}
}

// UnmarshalFDB constructs a Reader and delegates.
func (m *KeyRangeRef) UnmarshalFDB(data []byte) error {
	r, err := wire.NewReader(data)
	if err != nil {
		return err
	}
	m.UnmarshalFromReader(r)
	return nil
}

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

	// C++ serializer(ar, first, second) writes first to slot 0, second to slot 1.
	// Normal: serializer(ar, begin, end) → slot 0 = begin, slot 1 = end.
	// Optimized: serializer(ar, end, empty) → slot 0 = end, slot 1 = empty.
	// Our variables: when optimized, endOff = end data, beginOff = empty data.
	if keyRangeEqualsKeyAfter(m.Begin, m.End) {
		// Optimized: slot 0 = end (endOff), slot 1 = empty (beginOff)
		selfW.WriteRelativeOffset(endOff, int(vt[KeyRangeRefSlotBegin+2]))
		selfW.WriteRelativeOffset(beginOff, int(vt[KeyRangeRefSlotEnd+2]))
	} else {
		selfW.WriteRelativeOffset(beginOff, int(vt[KeyRangeRefSlotBegin+2]))
		selfW.WriteRelativeOffset(endOff, int(vt[KeyRangeRefSlotEnd+2]))
	}

	selfW.WriteToAt(selfStart)
	return selfStart
}
