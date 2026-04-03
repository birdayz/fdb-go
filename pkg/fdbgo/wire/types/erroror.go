package types

// Root-union protocol types: ErrorOr<EnsureTable<Void>> and ErrorOr<Error>.
// These are union_like_traits types serialized at the root level (no FakeRoot).
// Layout: [footer][vtables][root_obj][nested][ool] — root_offset → root_obj directly.
//
// TODO: generate these from the C++ extractor once it supports union_like_traits.

import (
	"encoding/binary"

	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/wire"
)

// --- ErrorOr vtable: tag(uint8) at offset 8, value(RelOff) at offset 4 ---

var errorOrVTable = wire.VTable{8, 9, 8, 4}

// --- EnsureTable<Void>: empty table, just soffset ---

var ensureTableVoidVTable = wire.VTable{4, 4}

// --- VoidReply = ErrorOr<EnsureTable<Void>> (PING response) ---

const VoidReplyFileID uint32 = 0x021EAD4A

var voidReplyVTableClosure = []wire.VTable{
	{4, 4},       // EnsureTable<Void>
	{6, 6, 4},    // appears in C++ vtable closure
	{8, 9, 8, 4}, // ErrorOr
}

var VoidReplyTemplate = wire.NewMessageTemplate(
	VoidReplyFileID, errorOrVTable, 4, voidReplyVTableClosure,
)

// VoidReply is the PING response: ErrorOr<EnsureTable<Void>> with tag=2 (success).
type VoidReply struct{}

func (m *VoidReply) MarshalFDB() []byte {
	t := VoidReplyTemplate
	// No OOL, one nested struct (EnsureTable<Void>).
	endOff := wire.MeasureObject(0, ensureTableVoidVTable, 4) // nested: just soffset
	bodySize := int(errorOrVTable[1]) - 4
	msgObjEnd := ((endOff + bodySize + 3) &^ 3) + 4
	vtableSize := t.PackedVTablesLen()
	vtableEnd := msgObjEnd + vtableSize // no FakeRoot
	totalSize := (vtableEnd + 8 + 7) &^ 7
	vtablePos := totalSize - vtableEnd
	msgObjPos := totalSize - msgObjEnd
	_ = msgObjPos

	buf := make([]byte, totalSize)
	var dw wire.DirectWriter
	dw.Init(buf, totalSize, vtablePos, t)

	// Write nested EnsureTable<Void> (empty object)
	nestedPos, _ := dw.WriteObject(ensureTableVoidVTable, 4)

	// Write root ErrorOr object
	objPos, obj := dw.WriteObject(errorOrVTable, 4)
	obj[int(errorOrVTable[2])] = 2 // tag = 2 (success, not Error)
	wire.PatchRelOff(obj, int(errorOrVTable[3]), objPos, nestedPos)

	t.WriteRootUnionFooter(buf, vtablePos, msgObjPos)
	return buf
}

// --- ErrorOrError = ErrorOr<Error> (error response, test helper) ---

var errorOrErrorVTableClosure = []wire.VTable{
	{6, 6, 4},    // Error
	{8, 9, 8, 4}, // ErrorOr
}

var ErrorOrErrorTemplate = wire.NewMessageTemplate(
	0, errorOrVTable, 4, errorOrErrorVTableClosure,
)

// ErrorOrError wraps an FDB error code in ErrorOr<Error> wire format.
type ErrorOrError struct {
	ErrorCode uint16
}

func (m *ErrorOrError) MarshalFDB() []byte {
	t := ErrorOrErrorTemplate
	endOff := wire.MeasureObject(0, ErrorVTable, 4) // nested Error
	bodySize := int(errorOrVTable[1]) - 4
	msgObjEnd := ((endOff + bodySize + 3) &^ 3) + 4
	vtableSize := t.PackedVTablesLen()
	vtableEnd := msgObjEnd + vtableSize // no FakeRoot
	totalSize := (vtableEnd + 8 + 7) &^ 7
	vtablePos := totalSize - vtableEnd
	msgObjPos := totalSize - msgObjEnd
	_ = msgObjPos

	buf := make([]byte, totalSize)
	var dw wire.DirectWriter
	dw.Init(buf, totalSize, vtablePos, t)

	// Write nested Error object
	nestedPos, errObj := dw.WriteObject(ErrorVTable, 4)
	binary.LittleEndian.PutUint16(errObj[int(ErrorVTable[ErrorSlotErrorCode+2]):], m.ErrorCode)

	// Write root ErrorOr object
	objPos, obj := dw.WriteObject(errorOrVTable, 4)
	obj[int(errorOrVTable[2])] = 1 // tag = 1 (Error)
	wire.PatchRelOff(obj, int(errorOrVTable[3]), objPos, nestedPos)

	t.WriteRootUnionFooter(buf, vtablePos, msgObjPos)
	return buf
}
