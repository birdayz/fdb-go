package types

import "github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/wire"

// GetKeyServerLocationsRequest — fdbclient/ClusterInterface.h
// serialize: serializer(ar, begin, end, limit, reverse, reply, spanContext, tenant, minTenantVersion, arena)
//
// VTable slot mapping:
//
//	vt[2]:  begin (dynamic_size)
//	vt[3]:  end.type (union, absent)
//	vt[4]:  limit (int32)
//	vt[5]:  reverse (bool)
//	vt[6]:  reply.type (union? — actually absent byte for reply)
//	vt[7]:  reply (serialize_member)
//	vt[8]:  spanContext (serialize_member)
//	vt[9]:  tenant (serialize_member, TenantInfo)
//	vt[10]: minTenantVersion (int64)
type GetKeyServerLocationsRequest struct {
	Begin            []byte
	Limit            int32
	ReplyFirst       uint64
	ReplySecond      uint64
	TenantId         int64
	MinTenantVersion int64
}

func (m *GetKeyServerLocationsRequest) MarshalFDB() []byte {
	vt := GetKeyServerLocationsRequestVTable
	w := wire.NewWriter(nil)
	return w.WriteMessage(GetKeyServerLocationsRequestFileID, vt, 8, func(obj *wire.ObjectWriter) {
		obj.WriteBytes(int(vt[2]), m.Begin)
		obj.WriteInt32(int(vt[4]), m.Limit)
		WriteReplyPromise(obj, int(vt[7]), m.ReplyFirst, m.ReplySecond)
		WriteTenantInfo(obj, int(vt[9]), m.TenantId)
		obj.WriteInt64(int(vt[10]), m.MinTenantVersion)
	})
}
