package types

import "github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/wire"

// MarshalGetKeyServerLocationsRequest builds a GetKeyServerLocationsRequest from parameters.
func MarshalGetKeyServerLocationsRequest(
	begin []byte, limit int32,
	replyFirst, replySecond uint64,
	tenantId, minTenantVersion int64,
) []byte {
	vt := GetKeyServerLocationsRequestVTable
	w := wire.NewWriter(nil)
	return w.WriteMessagePacked(GetKeyServerLocationsRequestTemplate,
		func(obj *wire.ObjectWriter) {
			obj.WriteBytes(int(vt[GetKeyServerLocationsRequestSlotBegin+2]), begin)
			obj.WriteInt32(int(vt[GetKeyServerLocationsRequestSlotLimit+2]), limit)
			WriteReplyPromise(obj, int(vt[GetKeyServerLocationsRequestSlotReply+2]), replyFirst, replySecond)
			WriteTenantInfo(obj, int(vt[GetKeyServerLocationsRequestSlotTenant+2]), tenantId)
			obj.WriteInt64(int(vt[GetKeyServerLocationsRequestSlotMinTenantVersion+2]), minTenantVersion)
		})
}
