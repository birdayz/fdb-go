package types

import "github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/wire"

func (m *ReplyPromise) MarshalFDB() []byte {
	panic("ReplyPromise.MarshalFDB not implemented")
}

// WriteReplyPromise writes a ReplyPromise nested struct with a UID token.
func WriteReplyPromise(obj *wire.ObjectWriter, parentOffset int, first, second uint64) {
	vt := ReplyPromiseVTable
	obj.WriteStruct(parentOffset, vt, 8, func(inner *wire.ObjectWriter) {
		off := int(vt[ReplyPromiseSlotField_0+2])
		inner.WriteUint64(off, first)
		inner.WriteUint64(off+8, second)
	})
}
