package types

// Manual port of C++ save_with_vtables for GetKeyServerLocationsRequest.
// Every step references the C++ source by file and line number.
// This validates our serializer.go primitives produce byte-identical output to C++.

import (
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"os"
	"testing"

	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/wire"
)

// TestManualMarshal_GetKeyServerLocationsRequest_basic does a full two-pass
// serialization matching C++ detail::save (flat_buffers.h:1311).
func TestManualMarshal_GetKeyServerLocationsRequest_basic(t *testing.T) {
	// Load C++ ground truth. Fatal on absence: testdata.json is a committed
	// artifact declared as a bazel data dep (no-skip rule).
	data, err := os.ReadFile("testdata.json")
	if err != nil {
		t.Fatalf("testdata.json not found (missing bazel data dep?): %v", err)
	}
	var vecs []testVectorEntry
	if err := json.Unmarshal(data, &vecs); err != nil {
		t.Fatalf("parse testdata.json: %v", err)
	}
	var cppBytes []byte
	var replyTok [16]byte
	for _, v := range vecs {
		if v.Name == "GetKeyServerLocationsRequest_basic" {
			cppBytes, _ = hex.DecodeString(v.Hex)
			cppBytes = cppBytes[8:] // strip version prefix
			replyTok = vectorReplyToken(t, v)
		}
	}
	if cppBytes == nil {
		t.Fatal("test vector not found")
	}
	t.Logf("C++ size: %d", len(cppBytes))

	// --- Input data (must match C++ test vector) ---
	beginKey := []byte("test_key")
	limit := int32(100)
	reverse := false
	tenantId := int64(-1)
	minTenantVersion := int64(-1)
	// Reply token: pinned in the vector (the extractor pre-binds the promise
	// to a fixed endpoint), written into the reply object in pass 2 below.
	// SpanContext: all zeros (default).

	// --- VTables (from generated code, extracted by C++ extractor) ---
	gkslrVT := GetKeyServerLocationsRequestVTable
	replyVT := ReplyPromiseVTable
	spanVT := SpanContextVTable
	tenantVT := TenantInfoVTable
	_ = replyVT
	_ = spanVT
	_ = tenantVT

	// --- Packed VTables (use the generated template's packed vtables) ---
	tmpl := GetKeyServerLocationsRequestTemplate
	packedVT := tmpl.PackedVTables()
	// vtSet = tmpl for GetOffset lookups (via VTableOffset)
	t.Logf("Packed VTables: %d bytes", len(packedVT))

	// ====== PASS 1: PrecomputeSize ======
	// C++ detail::save (flat_buffers.h:1311-1316)
	ps := wire.NewPrecomputeSize()

	// C++ save_with_vtables (flat_buffers.h:810):
	//   vtable_writer = writer.getMessageWriter(packed_tables.size())
	vtableNoop := ps.GetMessageWriter(len(packedVT))

	// C++ save_helper → SaveVisitorLambda::operator() for the root type
	// serializer(ar, begin, end, limit, reverse, reply, spanContext, tenant, minTenantVersion, arena)
	//
	// We process each field in serializer order.

	// Field: begin (DynamicSize, "test_key")
	// C++ visitDynamicSize (flat_buffers.h:515)
	ps.VisitDynamicSize(len(beginKey))
	t.Logf("After begin: cbs=%d", ps.CurrentBufferSize)

	// Field: end (Optional<KeyRef>, absent)
	// C++ union_like: writes tag=0, skips value. No cbs change.

	// Field: limit (scalar int32) — inline in object, no cbs change
	// Field: reverse (scalar bool) — inline, no cbs change

	// Field: reply (ReplyPromise, nested struct)
	// C++ _SizeOf<ReplyPromise>::size == 0 → save_helper recurses
	// ReplyPromise::serialize(ar, token) where token is UID (16 bytes inline)
	// ReplyPromise SaveVisitorLambda: self = getMessageWriter(vtable[1]=20)
	replyNoop := ps.GetMessageWriter(int(replyVT[1]))
	// No fields need OOL. Just align the object:
	// C++ line 972: RightAlign(cbs + vtable[1] - 4, max(4, fb_align<UID>)) + 4
	// UID has fb_align = 8 (2x uint64)
	replyStart := wire.RightAlign(ps.CurrentBufferSize+int(replyVT[1])-4, 8) + 4
	replyNoop.WriteToAt(ps, replyStart)
	t.Logf("After reply: cbs=%d replyStart=%d", ps.CurrentBufferSize, replyStart)

	// Field: spanContext (SpanContext, nested struct)
	// SpanContext::serialize(ar, traceID, spanID, m_Flags)
	// traceID=UID(fb_align=8), spanID=uint64(fb_align=8), m_Flags=uint8(fb_align=1)
	spanNoop := ps.GetMessageWriter(int(spanVT[1]))
	spanStart := wire.RightAlign(ps.CurrentBufferSize+int(spanVT[1])-4, 8) + 4
	spanNoop.WriteToAt(ps, spanStart)
	t.Logf("After span: cbs=%d spanStart=%d", ps.CurrentBufferSize, spanStart)

	// Field: tenant (TenantInfo, nested struct)
	// TenantInfo::serialize(ar, tenantId, token, arena)
	// tenantId=int64(fb_align=8), token=Optional<WipedString>(union_like, absent), arena=Arena(size=0)
	tenantNoop := ps.GetMessageWriter(int(tenantVT[1]))
	tenantStart := wire.RightAlign(ps.CurrentBufferSize+int(tenantVT[1])-4, 8) + 4
	tenantNoop.WriteToAt(ps, tenantStart)
	t.Logf("After tenant: cbs=%d tenantStart=%d", ps.CurrentBufferSize, tenantStart)

	// Field: minTenantVersion (scalar int64) — inline, no cbs change
	// Field: arena (Arena, size=0) — skip

	// Root object alignment:
	// C++ line 972: RightAlign(cbs + vtable[1] - 4, max(4, fb_align<Members>...)) + 4
	// Members: begin(RelOff→4), end(union→4), limit(int32→4), reverse(bool→1),
	//          reply(RelOff→4), spanContext(RelOff→4), tenant(RelOff→4), minTenantVersion(int64→8), arena(0)
	// max align = max(4, 4, 4, 4, 1, 4, 4, 4, 8) = 8
	rootNoop := ps.GetMessageWriter(int(gkslrVT[1]))
	rootStart := wire.RightAlign(ps.CurrentBufferSize+int(gkslrVT[1])-4, 8) + 4
	rootNoop.WriteToAt(ps, rootStart)
	t.Logf("After root: cbs=%d rootStart=%d", ps.CurrentBufferSize, rootStart)

	// save_helper wraps the root in a fake_root (vtable {6,8,4}, objSize=8).
	// The fake_root has one field: RelativeOffset to the actual root message.
	// C++ SaveVisitorLambda for fake_root: RightAlign(cbs + 8 - 4, max(4, 4)) + 4
	fakeRootObjNoop := ps.GetMessageWriter(8) // fake_root objSize = 8
	fakeRootObjStart := wire.RightAlign(ps.CurrentBufferSize+8-4, 4) + 4
	fakeRootObjNoop.WriteToAt(ps, fakeRootObjStart)
	t.Logf("After fakeRoot obj: cbs=%d fakeRootObjStart=%d", ps.CurrentBufferSize, fakeRootObjStart)

	// save_helper returns RelativeOffset{cbs} for the fake_root.
	rootRelOff := ps.CurrentBufferSize

	// C++ save_with_vtables line 812: vtable_writer.writeTo(writer)
	vtableNoop.WriteTo(ps)
	vtableStart := ps.CurrentBufferSize
	t.Logf("After vtables: cbs=%d vtableStart=%d", ps.CurrentBufferSize, vtableStart)

	// C++ line 817-820: root_writer = footer: [rootOff(4)][fileId(4)] = 8 bytes
	// Aligned to 8 bytes.
	footerSize := 8
	footerNoop := ps.GetMessageWriter(footerSize)
	footerStart := wire.RightAlign(ps.CurrentBufferSize+footerSize, 8)
	footerNoop.WriteToAt(ps, footerStart)
	t.Logf("After footer: cbs=%d footerStart=%d", ps.CurrentBufferSize, footerStart)

	totalSize := ps.CurrentBufferSize
	// C++ bytes already had 8-byte version prefix stripped
	t.Logf("Total size: %d (Go) vs %d (C++ without prefix)", totalSize, len(cppBytes))

	if totalSize != len(cppBytes) {
		// Fatal: the pass-2 byte-compare below indexes cppBytes by buf's
		// length — comparing differently-sized buffers is meaningless and
		// would panic on the longer side.
		t.Fatalf("SIZE MISMATCH: Go=%d C++=%d (delta=%d; C++ raw size with 8-byte IncludeVersion prefix is %d)",
			totalSize, len(cppBytes), totalSize-len(cppBytes), len(cppBytes)+8)
	}

	// ====== PASS 2: WriteToBuffer ======
	// C++ detail::save (flat_buffers.h:1319-1323)
	buf := make([]byte, totalSize)
	wb := wire.NewWriteToBuffer(buf, vtableStart, ps.WriteToOffsets)

	// vtable_writer
	vtableW := wb.GetMessageWriter(len(packedVT), false)
	// C++: vtable_writer.write(&packed_tables[0], 0, packed_tables.size())
	vtableW.WriteScalar(packedVT, 0)

	// --- Field: begin (DynamicSize) ---
	beginOff, _ := wb.VisitDynamicSize(beginKey)
	t.Logf("begin OOL at endOff=%d", beginOff)

	// --- Field: reply (nested struct) ---
	replyW := wb.GetMessageWriter(int(replyVT[1]), true)
	// Token: the vector's pinned 16-byte UID at the slot-0 field offset.
	replyW.WriteScalar(replyTok[:], int(replyVT[ReplyPromiseSlotToken+2]))
	// soffset: distance from reply object to its vtable
	// C++ line 972-975: vtable_offset = writer.vtable_start - vtableset->getOffset(&vtable)
	//                   relative = vtable_offset - start
	//                   self.write(&relative, 0, sizeof(relative))
	replyVTOff := vtableStart - tmpl.VTableOffset(ReplyPromiseVTable)
	replySOff := int32(replyVTOff - replyStart)
	var soffBuf [4]byte
	binary.LittleEndian.PutUint32(soffBuf[:], uint32(replySOff))
	replyW.WriteScalar(soffBuf[:], 0)
	// self.writeTo(writer, start)
	replyW.WriteToAt(replyStart)

	// --- Field: spanContext (nested struct) ---
	spanW := wb.GetMessageWriter(int(spanVT[1]), true)
	spanVTOff := vtableStart - tmpl.VTableOffset(SpanContextVTable)
	spanSOff := int32(spanVTOff - spanStart)
	binary.LittleEndian.PutUint32(soffBuf[:], uint32(spanSOff))
	spanW.WriteScalar(soffBuf[:], 0)
	spanW.WriteToAt(spanStart)

	// --- Field: tenant (nested struct) ---
	tenantW := wb.GetMessageWriter(int(tenantVT[1]), true)
	// TenantInfo fields: tenantId at offset 4 (int64)
	var tenantIdBuf [8]byte
	binary.LittleEndian.PutUint64(tenantIdBuf[:], uint64(tenantId))
	tenantW.WriteScalar(tenantIdBuf[:], int(tenantVT[2])) // slot 0 offset
	tenantVTOff := vtableStart - tmpl.VTableOffset(TenantInfoVTable)
	tenantSOff := int32(tenantVTOff - tenantStart)
	binary.LittleEndian.PutUint32(soffBuf[:], uint32(tenantSOff))
	tenantW.WriteScalar(soffBuf[:], 0)
	tenantW.WriteToAt(tenantStart)

	// --- Root object ---
	rootW := wb.GetMessageWriter(int(gkslrVT[1]), true)
	// Scalars:
	var limBuf [4]byte
	binary.LittleEndian.PutUint32(limBuf[:], uint32(limit))
	// Use slot constants + 2 to index vtable (vtable[slot+2] = field offset)
	rootW.WriteScalar(limBuf[:], int(gkslrVT[GetKeyServerLocationsRequestSlotLimit+2]))
	if reverse {
		rootW.WriteScalar([]byte{1}, int(gkslrVT[GetKeyServerLocationsRequestSlotReverse+2]))
	}
	var mvBuf [8]byte
	binary.LittleEndian.PutUint64(mvBuf[:], uint64(minTenantVersion))
	rootW.WriteScalar(mvBuf[:], int(gkslrVT[GetKeyServerLocationsRequestSlotMinTenantVersion+2]))

	// RelativeOffsets for nested/OOL objects:
	rootW.WriteRelativeOffset(beginOff, int(gkslrVT[GetKeyServerLocationsRequestSlotBegin+2]))
	rootW.WriteRelativeOffset(replyStart, int(gkslrVT[GetKeyServerLocationsRequestSlotReply+2]))
	rootW.WriteRelativeOffset(spanStart, int(gkslrVT[GetKeyServerLocationsRequestSlotSpanContext+2]))
	rootW.WriteRelativeOffset(tenantStart, int(gkslrVT[GetKeyServerLocationsRequestSlotTenant+2]))

	// soffset
	rootVTOff := vtableStart - tmpl.VTableOffset(GetKeyServerLocationsRequestVTable)
	rootSOff := int32(rootVTOff - rootStart)
	binary.LittleEndian.PutUint32(soffBuf[:], uint32(rootSOff))
	rootW.WriteScalar(soffBuf[:], 0)

	rootW.WriteToAt(rootStart)

	// --- FakeRoot object (wraps root message) ---
	// vtable {6, 8, 4}, objSize=8, one field: RelativeOffset to root message
	fakeRootVT := wire.VTable{6, 8, 4}
	fakeRootW := wb.GetMessageWriter(8, true)
	// Field 0 at offset 4: RelativeOffset to root message object
	fakeRootW.WriteRelativeOffset(rootStart, int(fakeRootVT[2])) // field 0 offset = 4
	// soffset
	fakeRootVTOff := vtableStart - tmpl.VTableOffset(fakeRootVT)
	fakeRootSOff := int32(fakeRootVTOff - fakeRootObjStart)
	binary.LittleEndian.PutUint32(soffBuf[:], uint32(fakeRootSOff))
	fakeRootW.WriteScalar(soffBuf[:], 0)
	fakeRootW.WriteToAt(fakeRootObjStart)

	// vtable_writer.writeTo(writer)
	vtableW.WriteTo()

	// Footer: [rootRelOff as RelativeOffset][fileID]
	footerW := wb.GetMessageWriter(footerSize, false)
	footerW.WriteRelativeOffset(rootRelOff, 0)
	var fidBuf [4]byte
	binary.LittleEndian.PutUint32(fidBuf[:], GetKeyServerLocationsRequestFileID)
	footerW.WriteScalar(fidBuf[:], 4)
	footerW.WriteToAt(footerStart)

	// Zero padding between fakeRoot and preceding content
	// C++ line 821: writer.write(&zeros, cbs - root_writer_size, padding)
	// padding = fakeRootStart - (vtableStart + packedVT size + rootWriterSize)
	// Actually padding = fakeRootStart - (cbs_before_fakeRoot + rootWriterSize)
	// We can skip for now — buffer was zeroed initially.

	t.Logf("Go  hex: %s", hex.EncodeToString(buf))
	t.Logf("C++ hex: %s", hex.EncodeToString(cppBytes))

	// BYTE-IDENTITY, zero tolerance: with the reply token pinned there is no
	// legitimate source of difference left.
	mismatches := 0
	shown := 0
	for i := 0; i < len(buf); i++ {
		if buf[i] != cppBytes[i] {
			if shown < 5 {
				t.Errorf("byte %d: Go=0x%02x C++=0x%02x", i, buf[i], cppBytes[i])
				shown++
			}
			mismatches++
		}
	}
	if mismatches > shown {
		t.Errorf("... and %d more byte mismatches", mismatches-shown)
	}
}
