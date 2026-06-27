package types

// Root-union protocol types: ErrorOr<EnsureTable<Void>> and ErrorOr<Error>.
// These are union_like_traits types serialized at the root level (no FakeRoot).
// Layout: [footer][vtables][root ErrorOr obj][nested alternative obj]
//
// Uses the same two-pass PrecomputeSize/WriteToBuffer infrastructure as all
// generated types. Only the MarshalFDB footer differs: union types point the
// root offset directly at the ErrorOr object (no FakeRoot wrapper).
//
// C++ source: flow/include/flow/flow.h union_like_traits<ErrorOr<T>>,
// flat_buffers.h save_with_vtables (union branch).

import (
	"encoding/binary"

	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/wire"
)

// --- ErrorOr vtable: tag(uint8) at slot 0 (offset 8), value(RelOff) at slot 1 (offset 4) ---

var errorOrVTable = wire.VTable{8, 9, 8, 4}

const (
	errorOrSlotTag   = 0 // uint8: 0=NONE, 1=Error, 2=T
	errorOrSlotValue = 1 // RelativeOffset to nested alternative
)

// --- EnsureTable<Void>: empty table, just soffset (no fields) ---

var ensureTableVoidVTable = wire.VTable{4, 4}

// ===================================================================
// VoidReply = ErrorOr<EnsureTable<Void>> (PING response, tag=2)
// ===================================================================

const VoidReplyFileID uint32 = 0x021EAD4A

var voidReplyVTableClosure = []wire.VTable{
	{4, 4},       // EnsureTable<Void>
	{6, 6, 4},    // Error (always in closure even if unused)
	{8, 9, 8, 4}, // ErrorOr
}

var VoidReplyTemplate = wire.NewMessageTemplate(
	VoidReplyFileID, errorOrVTable, 4, voidReplyVTableClosure,
)

type VoidReply struct{}

// precomputeSize — C++ PrecomputeSize pass.
// Serialize order: nested EnsureTable<Void>, then root ErrorOr.
func (m *VoidReply) precomputeSize(ps *wire.PrecomputeSize) int {
	// Nested: EnsureTable<Void> — empty table, just soffset (4 bytes).
	// C++ SaveVisitorLambda: current_buffer_size += vtable[1]
	{
		n := ps.GetMessageWriter(int(ensureTableVoidVTable[1]))
		n.WriteToAt(ps, wire.RightAlign(ps.CurrentBufferSize+int(ensureTableVoidVTable[1])-4, 4)+4)
	}
	// Root: ErrorOr — soffset + reloff + tag byte (9 bytes).
	{
		n := ps.GetMessageWriter(int(errorOrVTable[1]))
		n.WriteToAt(ps, wire.RightAlign(ps.CurrentBufferSize+int(errorOrVTable[1])-4, 4)+4)
	}
	return ps.CurrentBufferSize
}

// writeToBuffer — C++ WriteToBuffer pass.
func (m *VoidReply) writeToBuffer(wb *wire.WriteToBuffer, vtableStart int, tmpl *wire.MessageTemplate) int {
	// Nested: EnsureTable<Void> — empty, just soffset.
	nestedW := wb.GetMessageWriter(int(ensureTableVoidVTable[1]), true)
	nestedStart := nestedW.FinalLocation
	{
		soff := int32(vtableStart - tmpl.VTableOffset(ensureTableVoidVTable) - nestedStart)
		var b [4]byte
		binary.LittleEndian.PutUint32(b[:], uint32(soff))
		nestedW.WriteScalar(b[:], 0)
	}
	nestedW.WriteToAt(nestedStart)

	// Root: ErrorOr — soffset + value reloff + tag.
	rootW := wb.GetMessageWriter(int(errorOrVTable[1]), true)
	rootStart := rootW.FinalLocation
	{
		soff := int32(vtableStart - tmpl.VTableOffset(errorOrVTable) - rootStart)
		var b [4]byte
		binary.LittleEndian.PutUint32(b[:], uint32(soff))
		rootW.WriteScalar(b[:], 0)
	}
	rootW.WriteRelativeOffset(nestedStart, int(errorOrVTable[errorOrSlotValue+2]))
	rootW.WriteScalar([]byte{2}, int(errorOrVTable[errorOrSlotTag+2])) // tag=2 (success)
	rootW.WriteToAt(rootStart)

	return rootStart
}

// MarshalFDB serializes VoidReply using two-pass PrecomputeSize/WriteToBuffer.
// Union root: footer points directly to ErrorOr object (no FakeRoot).
func (m *VoidReply) MarshalFDB() []byte {
	t := VoidReplyTemplate
	packedVT := t.PackedVTables()

	// Pass 1: PrecomputeSize
	ps := wire.NewPrecomputeSize()
	vtNoop := ps.GetMessageWriter(len(packedVT))
	m.precomputeSize(ps)
	// No FakeRoot for union types — ErrorOr IS the root.
	vtNoop.WriteTo(ps)
	vtableStart := ps.CurrentBufferSize
	// Footer: root offset + file ID (8 bytes, aligned to 8).
	{
		n := ps.GetMessageWriter(8)
		n.WriteToAt(ps, wire.RightAlign(ps.CurrentBufferSize+8, 8))
	}
	totalSize := ps.CurrentBufferSize

	// Pass 2: WriteToBuffer
	buf := make([]byte, totalSize)
	wb := wire.NewWriteToBuffer(buf, vtableStart, ps.WriteToOffsets)
	vtW := wb.GetMessageWriter(len(packedVT), false)
	vtW.WriteScalar(packedVT, 0)
	rootStart := m.writeToBuffer(wb, vtableStart, t)

	// Union footer: rootRelOff + fileID (no FakeRoot).
	vtW.WriteTo()
	footerW := wb.GetMessageWriter(8, false)
	footerW.WriteRelativeOffset(rootStart, 0)
	{
		var b [4]byte
		binary.LittleEndian.PutUint32(b[:], VoidReplyFileID)
		footerW.WriteScalar(b[:], 4)
	}
	footerW.WriteToAt(wire.RightAlign(wb.CurrentBufferSize+8, 8))
	wire.ReleaseWriteToBuffer(wb)
	wire.ReleasePrecomputeSize(ps)
	return buf
}

// ===================================================================
// ErrorOrError = ErrorOr<Error> (error response, tag=1)
// ===================================================================

var errorOrErrorVTableClosure = []wire.VTable{
	{6, 6, 4},    // Error
	{8, 9, 8, 4}, // ErrorOr
}

var ErrorOrErrorTemplate = wire.NewMessageTemplate(
	0, errorOrVTable, 4, errorOrErrorVTableClosure,
)

type ErrorOrError struct {
	ErrorCode uint16
}

// precomputeSize — nested Error + root ErrorOr.
func (m *ErrorOrError) precomputeSize(ps *wire.PrecomputeSize) int {
	// Nested: Error — soffset + ErrorCode (6 bytes).
	{
		n := ps.GetMessageWriter(int(ErrorVTable[1]))
		n.WriteToAt(ps, wire.RightAlign(ps.CurrentBufferSize+int(ErrorVTable[1])-4, 4)+4)
	}
	// Root: ErrorOr — soffset + reloff + tag byte (9 bytes).
	{
		n := ps.GetMessageWriter(int(errorOrVTable[1]))
		n.WriteToAt(ps, wire.RightAlign(ps.CurrentBufferSize+int(errorOrVTable[1])-4, 4)+4)
	}
	return ps.CurrentBufferSize
}

// writeToBuffer — nested Error + root ErrorOr.
func (m *ErrorOrError) writeToBuffer(wb *wire.WriteToBuffer, vtableStart int, tmpl *wire.MessageTemplate) int {
	// Nested: Error.
	nestedW := wb.GetMessageWriter(int(ErrorVTable[1]), true)
	nestedStart := nestedW.FinalLocation
	{
		soff := int32(vtableStart - tmpl.VTableOffset(ErrorVTable) - nestedStart)
		var b [4]byte
		binary.LittleEndian.PutUint32(b[:], uint32(soff))
		nestedW.WriteScalar(b[:], 0)
	}
	{
		var b [2]byte
		binary.LittleEndian.PutUint16(b[:], m.ErrorCode)
		nestedW.WriteScalar(b[:], int(ErrorVTable[ErrorSlotErrorCode+2]))
	}
	nestedW.WriteToAt(nestedStart)

	// Root: ErrorOr — tag=1 (Error).
	rootW := wb.GetMessageWriter(int(errorOrVTable[1]), true)
	rootStart := rootW.FinalLocation
	{
		soff := int32(vtableStart - tmpl.VTableOffset(errorOrVTable) - rootStart)
		var b [4]byte
		binary.LittleEndian.PutUint32(b[:], uint32(soff))
		rootW.WriteScalar(b[:], 0)
	}
	rootW.WriteRelativeOffset(nestedStart, int(errorOrVTable[errorOrSlotValue+2]))
	rootW.WriteScalar([]byte{1}, int(errorOrVTable[errorOrSlotTag+2])) // tag=1 (Error)
	rootW.WriteToAt(rootStart)

	return rootStart
}

// ===================================================================
// ErrorOr<reply>(tag=2) success envelopes — fault-injection support.
//
// The Go client only RECEIVES ErrorOr<T> (the RPC promise wraps a reply in the
// union); it never marshals one in production. These hand-written builders let
// fault-injection tests synthesize the exact success-reply frames a storage server
// sends — same root-union category as VoidReply / ErrorOrError above. marshalErrorOrValue
// is the shared two-pass envelope (nested reply via the reply's own writeToBuffer,
// then a tag=2 ErrorOr root pointing at it); the per-type wrappers supply the
// reply, its template (its vtable closure + the ErrorOr root vtable), and its fileID.
// ===================================================================

func marshalErrorOrValue(
	t *wire.MessageTemplate,
	fileID uint32,
	precompute func(*wire.PrecomputeSize) int,
	writeReply func(*wire.WriteToBuffer, int, *wire.MessageTemplate) int,
) []byte {
	packedVT := t.PackedVTables()

	// Pass 1: PrecomputeSize — nested reply (+ its Error), then ErrorOr root.
	ps := wire.NewPrecomputeSize()
	vtNoop := ps.GetMessageWriter(len(packedVT))
	precompute(ps)
	{
		n := ps.GetMessageWriter(int(errorOrVTable[1]))
		n.WriteToAt(ps, wire.RightAlign(ps.CurrentBufferSize+int(errorOrVTable[1])-4, 4)+4)
	}
	vtNoop.WriteTo(ps)
	vtableStart := ps.CurrentBufferSize
	{
		n := ps.GetMessageWriter(8)
		n.WriteToAt(ps, wire.RightAlign(ps.CurrentBufferSize+8, 8))
	}
	totalSize := ps.CurrentBufferSize

	// Pass 2: WriteToBuffer.
	buf := make([]byte, totalSize)
	wb := wire.NewWriteToBuffer(buf, vtableStart, ps.WriteToOffsets)
	vtW := wb.GetMessageWriter(len(packedVT), false)
	vtW.WriteScalar(packedVT, 0)

	replyStart := writeReply(wb, vtableStart, t)

	rootW := wb.GetMessageWriter(int(errorOrVTable[1]), true)
	rootStart := rootW.FinalLocation
	{
		soff := int32(vtableStart - t.VTableOffset(errorOrVTable) - rootStart)
		var b [4]byte
		binary.LittleEndian.PutUint32(b[:], uint32(soff))
		rootW.WriteScalar(b[:], 0)
	}
	rootW.WriteRelativeOffset(replyStart, int(errorOrVTable[errorOrSlotValue+2]))
	rootW.WriteScalar([]byte{2}, int(errorOrVTable[errorOrSlotTag+2])) // tag=2 (value/success)
	rootW.WriteToAt(rootStart)

	// Union footer: rootRelOff + fileID (no FakeRoot).
	vtW.WriteTo()
	footerW := wb.GetMessageWriter(8, false)
	footerW.WriteRelativeOffset(rootStart, 0)
	{
		var b [4]byte
		binary.LittleEndian.PutUint32(b[:], fileID)
		footerW.WriteScalar(b[:], 4)
	}
	footerW.WriteToAt(wire.RightAlign(wb.CurrentBufferSize+8, 8))
	wire.ReleaseWriteToBuffer(wb)
	wire.ReleasePrecomputeSize(ps)
	return buf
}

// errorOrInlineErrorClosure is the nested reply's OWN vtable closure plus the
// ErrorOr root vtable — derived from GetValueReplyVTableClosure (not re-listed) so
// it tracks any schema-extractor regen of the reply's closure automatically. The
// nested reply + its Error are built by GetValueReply.writeToBuffer.
var errorOrInlineErrorClosure = append(append([]wire.VTable{}, GetValueReplyVTableClosure...), errorOrVTable)

var errorOrInlineErrorTemplate = wire.NewMessageTemplate(
	GetValueReplyFileID, errorOrVTable, 8, errorOrInlineErrorClosure,
)

// MarshalErrorOrInlineError builds the ErrorOr<GetValueReply>(tag=2) frame that a
// storage server emits for a read error: a SUCCESSFUL ErrorOr whose nested reply
// carries ONLY the inline LoadBalancedReply.error (Penalty + HasError + Error{code})
// and no value — byte-shape-identical to sendErrorWithPenalty's `Reply r; r.error=err;
// r.penalty=penalty; promise.send(r)` (storageserver.actor.cpp, 7.3.77). Because the
// inline-error slots (Penalty@0, HasError@1, Error@2) are IDENTICAL across
// GetValueReply / GetKeyReply / GetKeyValuesReply (TestLoadBalancedReplyErrorSlots),
// this single frame is decoded identically by all three read parsers, so one builder
// covers all of them. The footer carries GetValueReplyFileID for all three (a real
// server would send the per-RPC fileID), which is fine: ReadErrorOrInto does not
// validate the fileID, and TestMarshalErrorOrInlineError_RoundTrip pins the decode
// through every parser.
func MarshalErrorOrInlineError(code uint16, penalty float64) []byte {
	reply := &GetValueReply{Penalty: penalty, HasError: true, Error: Error{ErrorCode: code}}
	return marshalErrorOrValue(errorOrInlineErrorTemplate, GetValueReplyFileID, reply.precomputeSize, reply.writeToBuffer)
}

var errorOrGetKeyValuesReplyClosure = append(append([]wire.VTable{}, GetKeyValuesReplyVTableClosure...), errorOrVTable)

var errorOrGetKeyValuesReplyTemplate = wire.NewMessageTemplate(
	GetKeyValuesReplyFileID, errorOrVTable, 8, errorOrGetKeyValuesReplyClosure,
)

// MarshalErrorOrValueGetKeyValuesReply wraps a full GetKeyValuesReply in the
// ErrorOr<...>(tag=2) success envelope a storage server sends. Fault injection uses
// it to re-emit a reply decoded off the wire with one field mutated (e.g. More
// flipped to force a getRange continuation) — Data round-trips as an opaque []byte,
// so the rows are preserved.
func MarshalErrorOrValueGetKeyValuesReply(reply *GetKeyValuesReply) []byte {
	return marshalErrorOrValue(errorOrGetKeyValuesReplyTemplate, GetKeyValuesReplyFileID, reply.precomputeSize, reply.writeToBuffer)
}

// MarshalFDB serializes ErrorOrError using two-pass PrecomputeSize/WriteToBuffer.
// Union root: footer points directly to ErrorOr object (no FakeRoot).
func (m *ErrorOrError) MarshalFDB() []byte {
	t := ErrorOrErrorTemplate
	packedVT := t.PackedVTables()

	// Pass 1: PrecomputeSize
	ps := wire.NewPrecomputeSize()
	vtNoop := ps.GetMessageWriter(len(packedVT))
	m.precomputeSize(ps)
	vtNoop.WriteTo(ps)
	vtableStart := ps.CurrentBufferSize
	{
		n := ps.GetMessageWriter(8)
		n.WriteToAt(ps, wire.RightAlign(ps.CurrentBufferSize+8, 8))
	}
	totalSize := ps.CurrentBufferSize

	// Pass 2: WriteToBuffer
	buf := make([]byte, totalSize)
	wb := wire.NewWriteToBuffer(buf, vtableStart, ps.WriteToOffsets)
	vtW := wb.GetMessageWriter(len(packedVT), false)
	vtW.WriteScalar(packedVT, 0)
	rootStart := m.writeToBuffer(wb, vtableStart, t)

	vtW.WriteTo()
	footerW := wb.GetMessageWriter(8, false)
	footerW.WriteRelativeOffset(rootStart, 0)
	// FileID = 0 for ErrorOrError (response, not a request).
	footerW.WriteToAt(wire.RightAlign(wb.CurrentBufferSize+8, 8))
	wire.ReleaseWriteToBuffer(wb)
	wire.ReleasePrecomputeSize(ps)
	return buf
}
