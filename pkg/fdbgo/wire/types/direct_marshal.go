package types

// Hand-written two-pass marshal methods for GetKeyValuesRequest and dependencies.
// Prototype for the direct-write approach: 1 allocation total.
// Once validated, the C++ generator will emit these for all types.

import (
	"encoding/binary"

	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/wire"
)

// --- ReplyPromise: all inline (UID), no OOL, no nested ---

func (m *ReplyPromise) measureEndOff(endOff int) int {
	return wire.MeasureObject(endOff, ReplyPromiseVTable, 8)
}

func (m *ReplyPromise) writeDirect(dw *wire.DirectWriter) int {
	objPos, obj := dw.WriteObject(ReplyPromiseVTable, 8)
	// Token: 16-byte UID inline at offset 4
	copy(obj[int(ReplyPromiseVTable[ReplyPromiseSlotToken+2]):], m.Token[:])
	return objPos
}

// --- SpanContext: all inline (UID + uint64 + uint8), no OOL, no nested ---

func (m *SpanContext) measureEndOff(endOff int) int {
	return wire.MeasureObject(endOff, SpanContextVTable, 8)
}

func (m *SpanContext) writeDirect(dw *wire.DirectWriter) int {
	objPos, obj := dw.WriteObject(SpanContextVTable, 8)
	vt := SpanContextVTable
	copy(obj[int(vt[SpanContextSlotTraceID+2]):], m.TraceID[:])
	binary.LittleEndian.PutUint64(obj[int(vt[SpanContextSlotSpanID+2]):], m.SpanID)
	obj[int(vt[SpanContextSlotFlags+2])] = m.Flags
	return objPos
}

// --- TenantInfo: int64 inline, optional fields skipped in marshal ---

func (m *TenantInfo) measureEndOff(endOff int) int {
	return wire.MeasureObject(endOff, TenantInfoVTable, 8)
}

func (m *TenantInfo) writeDirect(dw *wire.DirectWriter) int {
	objPos, obj := dw.WriteObject(TenantInfoVTable, 8)
	vt := TenantInfoVTable
	binary.LittleEndian.PutUint64(obj[int(vt[TenantInfoSlotTenantId+2]):], uint64(m.TenantId))
	return objPos
}

// --- KeySelectorRef: 1 OOL field (Key), no nested ---

func (m *KeySelectorRef) measureEndOff(endOff int) int {
	endOff = wire.MeasureBytesOOL(endOff, m.Key)
	return wire.MeasureObject(endOff, KeySelectorRefVTable, 8)
}

func (m *KeySelectorRef) writeDirect(dw *wire.DirectWriter) int {
	// OOL
	var keyPos int
	if m.Key != nil {
		keyPos = dw.WriteBytesOOL(m.Key)
	}

	// Object
	vt := KeySelectorRefVTable
	objPos, obj := dw.WriteObject(vt, 8)

	// Fields
	if m.Key != nil {
		wire.PatchRelOff(obj, int(vt[KeySelectorRefSlotKey+2]), objPos, keyPos)
	}
	if m.OrEqual {
		obj[int(vt[KeySelectorRefSlotOrEqual+2])] = 1
	}
	binary.LittleEndian.PutUint32(obj[int(vt[KeySelectorRefSlotOffset+2]):], uint32(m.Offset))

	return objPos
}

// --- GetKeyValuesRequest: 5 nested + 1 OOL ---

func (m *GetKeyValuesRequest) measureEndOffDirect(endOff int) int {
	// OOL fields (same order as MarshalInto's WriteBytes calls)
	endOff = wire.MeasureBytesOOL(endOff, m.SsLatestCommitVersions)

	// Nested structs in REVERSE field order (matches computeEndOffset traversal)
	endOff = m.TenantInfo.measureEndOff(endOff)
	endOff = m.SpanContext.measureEndOff(endOff)
	endOff = m.Reply.measureEndOff(endOff)
	endOff = m.End.measureEndOff(endOff)
	endOff = m.Begin.measureEndOff(endOff)

	return endOff
}

func (m *GetKeyValuesRequest) writeDirectFn(dw *wire.DirectWriter) int {
	// 1. OOL data
	var ssPos int
	if m.SsLatestCommitVersions != nil {
		ssPos = dw.WriteBytesOOL(m.SsLatestCommitVersions)
	}

	// 2. Nested structs (REVERSE field order)
	tenantPos := m.TenantInfo.writeDirect(dw)
	spanPos := m.SpanContext.writeDirect(dw)
	replyPos := m.Reply.writeDirect(dw)
	endPos := m.End.writeDirect(dw)
	beginPos := m.Begin.writeDirect(dw)

	// 3. Root object
	vt := GetKeyValuesRequestVTable
	objPos, obj := dw.WriteObject(vt, 8)

	// Inline scalars
	binary.LittleEndian.PutUint64(obj[int(vt[GetKeyValuesRequestSlotVersion+2]):], uint64(m.Version))
	binary.LittleEndian.PutUint32(obj[int(vt[GetKeyValuesRequestSlotLimit+2]):], uint32(m.Limit))
	binary.LittleEndian.PutUint32(obj[int(vt[GetKeyValuesRequestSlotLimitBytes+2]):], uint32(m.LimitBytes))

	// Nested struct RelativeOffsets
	wire.PatchRelOff(obj, int(vt[GetKeyValuesRequestSlotBegin+2]), objPos, beginPos)
	wire.PatchRelOff(obj, int(vt[GetKeyValuesRequestSlotEnd+2]), objPos, endPos)
	wire.PatchRelOff(obj, int(vt[GetKeyValuesRequestSlotReply+2]), objPos, replyPos)
	wire.PatchRelOff(obj, int(vt[GetKeyValuesRequestSlotSpanContext+2]), objPos, spanPos)
	wire.PatchRelOff(obj, int(vt[GetKeyValuesRequestSlotTenantInfo+2]), objPos, tenantPos)

	// OOL RelativeOffset
	if m.SsLatestCommitVersions != nil {
		wire.PatchRelOff(obj, int(vt[GetKeyValuesRequestSlotSsLatestCommitVersions+2]), objPos, ssPos)
	}

	return objPos
}

// MarshalFDBDirect serializes GetKeyValuesRequest using two-pass direct-write.
// Exactly 1 heap allocation (the output buffer).
func (m *GetKeyValuesRequest) MarshalFDBDirect() []byte {
	t := GetKeyValuesRequestTemplate

	// Pass 1: measure
	endOff := m.measureEndOffDirect(0)

	// Compute layout (mirrors WriteMessagePacked)
	bodySize := int(GetKeyValuesRequestVTable[1]) - 4
	msgObjEnd := ((endOff + bodySize + 7) &^ 7) + 4 // rightAlign(endOff+bodySize, 8) + 4
	fakeRootEnd := ((msgObjEnd + 4 + 3) &^ 3) + 4   // rightAlign(msgObjEnd+4, 4) + 4
	vtableSize := t.PackedVTablesLen()
	vtableEnd := fakeRootEnd + vtableSize
	totalSize := (vtableEnd + 8 + 7) &^ 7 // rightAlign(vtableEnd+8, 8)
	_ = bodySize

	// Positions
	vtablePos := totalSize - vtableEnd
	fakeRootPos := totalSize - fakeRootEnd
	msgObjPos := totalSize - msgObjEnd

	// THE one allocation
	buf := make([]byte, totalSize)

	// Pass 2: write
	dw := wire.DirectWriter{}
	dw.Init(buf, totalSize, vtablePos, t)
	m.writeDirectFn(&dw)

	// FakeRoot
	t.WriteFakeRoot(buf, fakeRootPos, vtablePos, msgObjPos)

	// VTables + footer
	t.WriteVTablesAndFooter(buf, vtablePos, fakeRootPos)

	return buf
}
