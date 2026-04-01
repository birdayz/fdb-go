package types

import "github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/wire"

func (m *GetKeyRequest) UnmarshalFDB(data []byte) error {
	panic("GetKeyRequest.UnmarshalFDB not implemented")
}

func (m *GetKeyRequest) MarshalFDB() []byte {
	vt := GetKeyRequestVTable
	w := wire.NewWriter(nil)
	return w.WriteMessagePacked(GetKeyRequestTemplate,
		func(obj *wire.ObjectWriter) {
			WriteTenantInfo(obj, int(vt[GetKeyRequestSlotTenantInfo+2]), m.TenantId)
			obj.WriteStruct(int(vt[GetKeyRequestSlotSpanContext+2]), SpanContextVTable, 8, func(inner *wire.ObjectWriter) {})
			WriteReplyPromise(obj, int(vt[GetKeyRequestSlotReply+2]), m.ReplyFirst, m.ReplySecond)
			writeKeySelectorRef(obj, int(vt[GetKeyRequestSlotSel+2]), m.SelectorKey, m.SelectorOffset, m.SelectorOrEqual)
			obj.WriteInt64(int(vt[GetKeyRequestSlotVersion+2]), m.Version)
			obj.WriteBytes(int(vt[GetKeyRequestSlotSsLatestCommitVersions+2]), emptyVersionVector)
		})
}
