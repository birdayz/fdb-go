package types

import "github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/wire"

// MarshalGetReadVersionRequest builds a GetReadVersionRequest from parameters.
func MarshalGetReadVersionRequest(
	transactionCount, flags uint32,
	maxVersion int64,
	replyFirst, replySecond uint64,
) []byte {
	vt := GetReadVersionRequestVTable
	w := wire.NewWriter(nil)
	return w.WriteMessagePacked(GetReadVersionRequestTemplate,
		func(obj *wire.ObjectWriter) {
			obj.WriteUint32(int(vt[GetReadVersionRequestSlotTransactionCount+2]), transactionCount)
			obj.WriteUint32(int(vt[GetReadVersionRequestSlotFlags+2]), flags)
			obj.WriteInt64(int(vt[GetReadVersionRequestSlotMaxVersion+2]), maxVersion)
			WriteReplyPromise(obj, int(vt[GetReadVersionRequestSlotReply+2]), wire.UIDFromParts(replyFirst, replySecond))
		})
}
