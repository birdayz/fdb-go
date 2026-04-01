package types

import "github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/wire"

func (m *GetKeyValuesRequest) UnmarshalFDB(data []byte) error {
	panic("GetKeyValuesRequest.UnmarshalFDB not implemented")
}

func (m *GetKeyValuesRequest) MarshalFDB() []byte {
	vt := GetKeyValuesRequestVTable
	w := wire.NewWriter(nil)
	return w.WriteMessagePacked(GetKeyValuesRequestTemplate,
		func(obj *wire.ObjectWriter) {
			WriteTenantInfo(obj, int(vt[GetKeyValuesRequestSlotTenantInfo+2]), m.TenantId)
			obj.WriteStruct(int(vt[GetKeyValuesRequestSlotSpanContext+2]), SpanContextVTable, 8, func(inner *wire.ObjectWriter) {})
			WriteReplyPromise(obj, int(vt[GetKeyValuesRequestSlotReply+2]), m.ReplyFirst, m.ReplySecond)
			writeKeySelectorRef(obj, int(vt[GetKeyValuesRequestSlotEnd+2]), m.EndKey, m.EndOffset, m.EndOrEqual)
			writeKeySelectorRef(obj, int(vt[GetKeyValuesRequestSlotBegin+2]), m.BeginKey, m.BeginOffset, m.BeginOrEqual)
			obj.WriteInt64(int(vt[GetKeyValuesRequestSlotVersion+2]), m.Version)
			obj.WriteInt32(int(vt[GetKeyValuesRequestSlotLimit+2]), m.Limit)
			obj.WriteInt32(int(vt[GetKeyValuesRequestSlotLimitBytes+2]), m.LimitBytes)
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
