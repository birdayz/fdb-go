package types

import "github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/wire"

// emptyVersionVector is the serialized form of an empty VersionVector.
// C++ VersionVector::getEncodedSize() = sizeof(size_t) + sizeof(Version) = 16.
var emptyVersionVector = make([]byte, 16)

func (m *GetValueRequest) UnmarshalFDB(data []byte) error {
	panic("GetValueRequest.UnmarshalFDB not implemented")
}

func (m *GetValueRequest) MarshalFDB() []byte {
	vt := GetValueRequestVTable
	w := wire.NewWriter(nil)
	return w.WriteMessagePacked(GetValueRequestTemplate,
		func(obj *wire.ObjectWriter) {
			WriteTenantInfo(obj, int(vt[GetValueRequestSlotTenantInfo+2]), m.TenantId)
			obj.WriteStruct(int(vt[GetValueRequestSlotSpanContext+2]), SpanContextVTable, 8, func(inner *wire.ObjectWriter) {})
			WriteReplyPromise(obj, int(vt[GetValueRequestSlotReply+2]), m.ReplyFirst, m.ReplySecond)
			obj.WriteInt64(int(vt[GetValueRequestSlotVersion+2]), m.Version)
			obj.WriteBytes(int(vt[GetValueRequestSlotKey+2]), m.Key)
			obj.WriteBytes(int(vt[GetValueRequestSlotSsLatestCommitVersions+2]), emptyVersionVector)
		})
}
