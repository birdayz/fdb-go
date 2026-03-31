package types

import "github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/wire"

// ReplyPromise — fdbrpc/ReplyPromise.h
// VTable {6, 20, 4}: 1 field — UID token (16 bytes inline at field 0)
//
// WriteReplyPromise writes a ReplyPromise nested struct at the given parent offset.
func WriteReplyPromise(obj *wire.ObjectWriter, parentOffset int, first, second uint64) {
	vt := ReplyPromiseVTable
	obj.WriteStruct(parentOffset, vt, 8, func(inner *wire.ObjectWriter) {
		off := int(vt[2]) // field 0 offset
		inner.WriteUint64(off, first)
		inner.WriteUint64(off+8, second) // UID second half
	})
}
