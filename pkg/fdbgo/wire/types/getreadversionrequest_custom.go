package types

import "github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/wire"

func (m *GetReadVersionRequest) UnmarshalFDB(data []byte) error {
	panic("GetReadVersionRequest.UnmarshalFDB not implemented")
}

func (m *GetReadVersionRequest) MarshalFDB() []byte {
	vt := GetReadVersionRequestVTable
	w := wire.NewWriter(nil)
	return w.WriteMessagePacked(GetReadVersionRequestTemplate,
		func(obj *wire.ObjectWriter) {
			obj.WriteUint32(int(vt[GetReadVersionRequestSlotTransactionCount+2]), m.TransactionCount)
			obj.WriteUint32(int(vt[GetReadVersionRequestSlotFlags+2]), m.Flags)
			obj.WriteInt64(int(vt[GetReadVersionRequestSlotMaxVersion+2]), m.MaxVersion)
			WriteReplyPromise(obj, int(vt[GetReadVersionRequestSlotReply+2]), m.ReplyFirst, m.ReplySecond)
		})
}
