package wire

import (
	"encoding/hex"
	"encoding/json"
	"os"
	"testing"
)

type testVec struct {
	Name   string `json:"name"`
	FileID uint32 `json:"file_id"`
	Size   int    `json:"size"`
	Hex    string `json:"hex"`
}

// TestPrecomputeSize_GetReadVersionRequest manually walks through the
// C++ PrecomputeSize logic for GetReadVersionRequest to verify our
// new serializer produces the same buffer size as C++.
//
// C++ serialize order: transactionCount, flags, tags, debugID, reply, spanContext, maxVersion
// C++ source: CommitProxyInterface.h, struct GetReadVersionRequest
func TestPrecomputeSize_GetReadVersionRequest(t *testing.T) {
	// Load C++ ground truth
	data, err := os.ReadFile("types/testdata.json")
	if err != nil {
		t.Skipf("testdata.json not found: %v", err)
	}
	var vecs []testVec
	json.Unmarshal(data, &vecs)
	var cppSize int
	for _, v := range vecs {
		if v.Name == "GetReadVersionRequest_causal_risky" {
			cppSize = v.Size - 8 // strip 8-byte version prefix
		}
	}
	if cppSize == 0 {
		t.Fatal("test vector not found")
	}
	t.Logf("C++ size (without prefix): %d", cppSize)

	// --- Pass 1: PrecomputeSize ---
	// Mirrors C++ SaveVisitorLambda::operator() for:
	//   serializer(ar, transactionCount, flags, tags, debugID, reply, spanContext, maxVersion)

	ps := NewPrecomputeSize()

	// VTables and constants
	grvVT := VTable{20, 37, 12, 16, 20, 36, 24, 28, 32, 4} // GetReadVersionRequest
	replyVT := VTable{6, 20, 4}                            // ReplyPromise
	spanVT := VTable{10, 29, 4, 20, 28}                    // SpanContext
	fakeRootVT := VTable{6, 8, 4}                          // FakeRoot (always)

	// Pack vtables (C++ get_vtableset_impl, flat_buffers.h:769)
	// VTableSet collects all unique vtables traversed during serialization.
	// For GetReadVersionRequest: grvVT, replyVT, spanVT, plus FakeRoot.
	// Plus the debugID vtable (even though debugID is absent, C++ traverses it).
	// The debugID Optional<UID> has union_like_traits which adds a vtable for
	// the UID alternative.
	// UID has fb_size=16 (struct_like), its vtable comes from struct_like_traits.
	// Let's use the closure from the generated code.
	closure := []VTable{
		{6, 20, 4},                              // ReplyPromise
		{6, 8, 4},                               // FakeRoot
		{8, 12, 4, 8},                           // UID (from Optional<UID> debugID)
		{10, 29, 4, 20, 28},                     // SpanContext
		{20, 37, 12, 16, 20, 36, 24, 28, 32, 4}, // GetReadVersionRequest
	}
	_ = fakeRootVT

	// Pack vtables
	vtSet := newVTableSet()
	for _, vt := range closure {
		vtSet.add(vt)
	}
	packedVTables := vtSet.pack()
	t.Logf("Packed vtables: %d bytes", len(packedVTables))

	// C++ save_with_vtables (flat_buffers.h:810):
	//   vtable_writer = writer.getMessageWriter(packed_tables.size())
	vtableWriter := ps.GetMessageWriter(len(packedVTables))

	// C++ save_helper → SaveVisitorLambda::operator()
	// For GetReadVersionRequest, process each field:

	// 1. transactionCount (uint32, scalar fb_size=4)
	//    C++: save_helper returns RelativeOffset{writer.current_buffer_size}
	//    Then self.write(&result, vtable[i++], sizeof(result))
	//    Scalars: save_helper just returns the current position as a RelativeOffset.
	//    Actually no — for scalars that fit in the object, they're written inline.
	//    The for_each dispatch: _SizeOf<uint32_t>::size = 4 (non-zero)
	//    So: auto result = save_helper(member, writer, vtableset, ctx)
	//    save_helper for scalar: returns RelativeOffset{0} (the value is written inline)
	//    Actually... save_helper for scalars just writes the value inline into self.
	//    Let me re-read the C++ for scalars.

	// C++ save_helper (flat_buffers.h:1278):
	//   For types where _SizeOf<T>::size > 0 && !is_dynamic_size:
	//     return RelativeOffset{writer.current_buffer_size}
	//   Then the caller writes the scalar value into self at vtable[i]
	//
	// Wait, I need to look at save_helper more carefully.
	// For scalar types like uint32_t:
	//   _SizeOf<uint32_t>::size = 4
	//   It's NOT a nested struct, NOT dynamic_size
	//   save_helper returns a RelativeOffset containing the raw value
	//   The MessageWriter::write for non-RelativeOffset just copies bytes
	//
	// Actually for plain scalars, the value IS the data (not an offset).
	// self.write(&result, vtable[i], sizeof(result)) copies the scalar bytes.
	// This doesn't affect PrecomputeSize because scalars don't change current_buffer_size.

	// Scalars don't affect PrecomputeSize — they're inline in the object.
	// i advances: i=2 (transactionCount), i=3 (flags)

	// 3. tags (TransactionTagMap, dynamic_size, empty)
	//    C++: writer.visitDynamicSize(tags) — tags is empty
	tagsEmpty := ps.VisitDynamicSize(0) // size=0, first empty → allocates 4 bytes
	t.Logf("After tags (empty): currentBufferSize=%d reused=%v", ps.CurrentBufferSize, tagsEmpty)

	// 4. debugID (Optional<UID>, union_like, absent)
	//    C++: self.write(&fb_type_tag, vtable[i++], 1) — writes tag=0 (absent)
	//    i++ for the value slot (skipped since absent)
	//    No effect on PrecomputeSize for absent optional.

	// 5. reply (ReplyPromise, nested struct)
	//    C++ _SizeOf<ReplyPromise>::size = 0 (nested structs have size 0)
	//    → save_helper recurses into ReplyPromise's SaveVisitorLambda
	//    ReplyPromise serializes just `token` (UID, 16 bytes inline, [16]byte)
	//    ReplyPromise vtable[1] = 20, maxAlign = max(4, fb_align<UID>) = max(4, 8) = 8
	replyObj := ps.GetMessageWriter(int(replyVT[1]))
	// UID token is inline (16 bytes), no OOL data.
	// Object alignment: RightAlign(cbs + 20 - 4, 8) + 4
	replyStart := ps.SaveObjectSize(int(replyVT[1]), 8)
	replyObj.WriteToAt(ps, replyStart)
	t.Logf("After reply: currentBufferSize=%d replyStart=%d", ps.CurrentBufferSize, replyStart)

	// 6. spanContext (SpanContext, nested struct)
	//    SpanContext serializes: traceID(UID, 16B inline), spanID(uint64, 8B inline), m_Flags(uint8, 1B inline)
	//    vtable[1] = 29, maxAlign = max(4, 8, 8, 1) = 8
	spanObj := ps.GetMessageWriter(int(spanVT[1]))
	spanStart := ps.SaveObjectSize(int(spanVT[1]), 8)
	spanObj.WriteToAt(ps, spanStart)
	t.Logf("After spanContext: currentBufferSize=%d spanStart=%d", ps.CurrentBufferSize, spanStart)

	// 7. maxVersion (int64, scalar fb_size=8) — inline, no effect on cbs

	// Now: align the GetReadVersionRequest object itself
	//    vtable[1] = 37, maxAlign = 8
	grvObj := ps.GetMessageWriter(int(grvVT[1]))
	grvStart := ps.SaveObjectSize(int(grvVT[1]), 8)
	grvObj.WriteToAt(ps, grvStart)
	t.Logf("After GRV object: currentBufferSize=%d grvStart=%d", ps.CurrentBufferSize, grvStart)

	// save_helper returns RelativeOffset{ps.CurrentBufferSize} for the root

	// C++ save_with_vtables (flat_buffers.h:812-814):
	//   vtable_writer.writeTo(writer)
	//   *vtable_start = writer.current_buffer_size
	vtableWriter.WriteTo(ps)
	vtableStart := ps.CurrentBufferSize
	t.Logf("After vtables: currentBufferSize=%d vtableStart=%d", ps.CurrentBufferSize, vtableStart)

	// C++ line 818-820: root_writer (FakeRoot: rootOffset + fileID = 8 bytes)
	rootWriterSize := 8
	rootWriter := ps.GetMessageWriter(rootWriterSize)
	rootStart := RightAlign(ps.CurrentBufferSize+rootWriterSize, 8)
	rootWriter.WriteToAt(ps, rootStart)
	t.Logf("After FakeRoot: currentBufferSize=%d rootStart=%d", ps.CurrentBufferSize, rootStart)

	// Total size = current_buffer_size after root
	totalSize := ps.CurrentBufferSize
	t.Logf("Total size: %d (Go) vs %d (C++)", totalSize, cppSize)

	if totalSize != cppSize {
		t.Errorf("SIZE MISMATCH: Go=%d C++=%d (delta=%d)", totalSize, cppSize, totalSize-cppSize)
	} else {
		t.Logf("SIZE MATCHES C++")
	}

	// Also verify with C++ hex dump
	if cppSize > 0 {
		for _, v := range vecs {
			if v.Name == "GetReadVersionRequest_causal_risky" {
				cppBytes, _ := hex.DecodeString(v.Hex)
				cppBytes = cppBytes[8:] // strip prefix
				t.Logf("C++ first 16 bytes: %x", cppBytes[:16])
				t.Logf("C++ rootOff=%d fileId=%d",
					int(cppBytes[0])|int(cppBytes[1])<<8|int(cppBytes[2])<<16|int(cppBytes[3])<<24,
					int(cppBytes[4])|int(cppBytes[5])<<8|int(cppBytes[6])<<16|int(cppBytes[7])<<24)
			}
		}
	}
}
