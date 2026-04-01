package types

import "github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/wire"

func (m *GetKeyServerLocationsRequest) UnmarshalFDB(data []byte) error {
	panic("GetKeyServerLocationsRequest.UnmarshalFDB not implemented")
}

func (m *GetKeyServerLocationsRequest) MarshalFDB() []byte {
	vt := GetKeyServerLocationsRequestVTable
	w := wire.NewWriter(nil)
	return w.WriteMessagePacked(GetKeyServerLocationsRequestTemplate,
		func(obj *wire.ObjectWriter) {
			obj.WriteBytes(int(vt[GetKeyServerLocationsRequestSlotBegin+2]), m.Begin)
			obj.WriteInt32(int(vt[GetKeyServerLocationsRequestSlotLimit+2]), m.Limit)
			WriteReplyPromise(obj, int(vt[GetKeyServerLocationsRequestSlotReply+2]), m.ReplyFirst, m.ReplySecond)
			WriteTenantInfo(obj, int(vt[GetKeyServerLocationsRequestSlotTenant+2]), m.TenantId)
			obj.WriteInt64(int(vt[GetKeyServerLocationsRequestSlotMinTenantVersion+2]), m.MinTenantVersion)
		})
}
