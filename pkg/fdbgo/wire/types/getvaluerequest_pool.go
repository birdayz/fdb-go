package types

// getvaluerequest_pool.go — Pooled MarshalFDB for GetValueRequest.
//
// MarshalFDB allocates a fresh []byte buffer on every call. On the read path
// (the hottest read path), this accounts for significant allocations.
// MarshalFDBPooled reuses a caller-provided buffer when capacity is sufficient.

import (
	"encoding/binary"

	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/wire"
)

// MarshalFDBPooled is identical to MarshalFDB but reuses dst if it has
// sufficient capacity. Returns the serialized bytes (subslice of dst or
// a new allocation if dst was too small).
func (m *GetValueRequest) MarshalFDBPooled(dst []byte) []byte {
	t := GetValueRequestTemplate
	packedVT := t.PackedVTables()

	// Pass 1: PrecomputeSize
	ps := wire.NewPrecomputeSize()
	vtNoop := ps.GetMessageWriter(len(packedVT))
	m.precomputeSize(ps)
	{
		n := ps.GetMessageWriter(8)
		n.WriteToAt(ps, wire.RightAlign(ps.CurrentBufferSize+4, 4)+4)
	}
	vtNoop.WriteTo(ps)
	vtableStart := ps.CurrentBufferSize
	{
		n := ps.GetMessageWriter(8)
		n.WriteToAt(ps, wire.RightAlign(ps.CurrentBufferSize+8, 8))
	}
	totalSize := ps.CurrentBufferSize

	// Pass 2: WriteToBuffer — reuse dst if large enough
	var buf []byte
	if cap(dst) >= totalSize {
		buf = dst[:totalSize]
		// Zero the buffer — WriteToBuffer writes sparse data.
		for i := range buf {
			buf[i] = 0
		}
	} else {
		buf = make([]byte, totalSize)
	}
	wb := wire.NewWriteToBuffer(buf, vtableStart, ps.WriteToOffsets)
	vtW := wb.GetMessageWriter(len(packedVT), false)
	vtW.WriteScalar(packedVT, 0)
	rootStart := m.writeToBuffer(wb, vtableStart, t)

	// FakeRoot object
	fakeRootW := wb.GetMessageWriter(8, true)
	fakeRootStart := fakeRootW.FinalLocation
	fakeRootW.WriteRelativeOffset(rootStart, int(wire.FakeRootVTable[2]))
	{
		soff := int32(vtableStart - t.VTableOffset(wire.FakeRootVTable) - fakeRootStart)
		var b [4]byte
		binary.LittleEndian.PutUint32(b[:], uint32(soff))
		fakeRootW.WriteScalar(b[:], 0)
	}
	fakeRootW.WriteToAt(fakeRootStart)

	vtW.WriteTo()
	footerW := wb.GetMessageWriter(8, false)
	footerW.WriteRelativeOffset(fakeRootStart, 0)
	{
		var b [4]byte
		binary.LittleEndian.PutUint32(b[:], GetValueRequestFileID)
		footerW.WriteScalar(b[:], 4)
	}
	footerW.WriteToAt(wire.RightAlign(wb.CurrentBufferSize+8, 8))

	wire.ReleaseWriteToBuffer(wb)
	wire.ReleasePrecomputeSize(ps)

	return buf
}
