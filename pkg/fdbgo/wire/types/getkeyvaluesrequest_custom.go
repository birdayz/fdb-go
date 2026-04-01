package types

import "github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/wire"

// MarshalGetKeyValuesRequest builds a GetKeyValuesRequest from parameters.
func MarshalGetKeyValuesRequest(
	beginKey []byte, beginOffset int32, beginOrEqual bool,
	endKey []byte, endOffset int32, endOrEqual bool,
	version int64, limit, limitBytes int32,
	replyFirst, replySecond uint64,
	tenantId int64,
) []byte {
	vt := GetKeyValuesRequestVTable
	w := wire.NewWriter(nil)
	return w.WriteMessagePacked(GetKeyValuesRequestTemplate,
		func(obj *wire.ObjectWriter) {
			WriteTenantInfo(obj, int(vt[GetKeyValuesRequestSlotTenantInfo+2]), tenantId)
			obj.WriteStruct(int(vt[GetKeyValuesRequestSlotSpanContext+2]), SpanContextVTable, 8, func(inner *wire.ObjectWriter) {})
			WriteReplyPromise(obj, int(vt[GetKeyValuesRequestSlotReply+2]), wire.UIDFromParts(replyFirst, replySecond))
			writeKeySelectorRef(obj, int(vt[GetKeyValuesRequestSlotEnd+2]), endKey, endOffset, endOrEqual)
			writeKeySelectorRef(obj, int(vt[GetKeyValuesRequestSlotBegin+2]), beginKey, beginOffset, beginOrEqual)
			obj.WriteInt64(int(vt[GetKeyValuesRequestSlotVersion+2]), version)
			obj.WriteInt32(int(vt[GetKeyValuesRequestSlotLimit+2]), limit)
			obj.WriteInt32(int(vt[GetKeyValuesRequestSlotLimitBytes+2]), limitBytes)
			obj.WriteBytes(int(vt[GetKeyValuesRequestSlotSsLatestCommitVersions+2]), emptyVersionVector)
		})
}

// writeKeySelectorRef writes a KeySelectorRef nested struct.
func writeKeySelectorRef(obj *wire.ObjectWriter, parentOffset int, key []byte, offset int32, orEqual bool) {
	ksVT := KeySelectorRefVTable
	obj.WriteStruct(parentOffset, ksVT, 4, func(inner *wire.ObjectWriter) {
		inner.WriteBytes(int(ksVT[KeySelectorRefSlotKey+2]), key)
		if orEqual {
			inner.WriteUint8(int(ksVT[KeySelectorRefSlotOrEqual+2]), 1)
		}
		inner.WriteInt32(int(ksVT[KeySelectorRefSlotOffset+2]), offset)
	})
}
