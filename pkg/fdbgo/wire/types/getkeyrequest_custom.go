package types

import "github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/wire"

// MarshalGetKeyRequest builds a GetKeyRequest from parameters.
func MarshalGetKeyRequest(
	selectorKey []byte, selectorOffset int32, selectorOrEqual bool,
	version int64,
	replyFirst, replySecond uint64,
	tenantId int64,
) []byte {
	vt := GetKeyRequestVTable
	w := wire.NewWriter(nil)
	return w.WriteMessagePacked(GetKeyRequestTemplate,
		func(obj *wire.ObjectWriter) {
			WriteTenantInfo(obj, int(vt[GetKeyRequestSlotTenantInfo+2]), tenantId)
			obj.WriteStruct(int(vt[GetKeyRequestSlotSpanContext+2]), SpanContextVTable, 8, func(inner *wire.ObjectWriter) {})
			WriteReplyPromise(obj, int(vt[GetKeyRequestSlotReply+2]), replyFirst, replySecond)
			writeKeySelectorRef(obj, int(vt[GetKeyRequestSlotSel+2]), selectorKey, selectorOffset, selectorOrEqual)
			obj.WriteInt64(int(vt[GetKeyRequestSlotVersion+2]), version)
			obj.WriteBytes(int(vt[GetKeyRequestSlotSsLatestCommitVersions+2]), emptyVersionVector)
		})
}
