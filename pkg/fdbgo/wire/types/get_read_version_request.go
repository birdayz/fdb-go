package types

import "github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/wire"

// GetReadVersionRequest — fdbclient/GrvProxyInterface.h
// serialize: serializer(ar, transactionCount, flags, tags, debugID, reply, spanContext, maxVersion)
//
// VTable slot mapping:
//
//	vt[2]: transactionCount (uint32)
//	vt[3]: flags (uint32)
//	vt[4]: tags.type (union, absent)
//	vt[5]: tags.value
//	vt[6]: debugID.type (union, absent)
//	vt[7]: reply (serialize_member)
//	vt[8]: spanContext (serialize_member)
//	vt[9]: maxVersion (int64)
type GetReadVersionRequest struct {
	TransactionCount uint32
	Flags            uint32
	MaxVersion       int64
	ReplyFirst       uint64
	ReplySecond      uint64
}

func (m *GetReadVersionRequest) MarshalFDB() []byte {
	vt := GetReadVersionRequestVTable
	w := wire.NewWriter(nil)
	return w.WriteMessage(GetReadVersionRequestFileID, vt, 8, func(obj *wire.ObjectWriter) {
		obj.WriteUint32(int(vt[2]), m.TransactionCount)
		obj.WriteUint32(int(vt[3]), m.Flags)
		obj.WriteInt64(int(vt[9]), m.MaxVersion)
		WriteReplyPromise(obj, int(vt[7]), m.ReplyFirst, m.ReplySecond)
	})
}
